// Command dockback is the single-binary DockBack server: it serves the
// embedded React UI and REST API, talks to one or more Docker hosts through the
// socket-proxy/SSH/mTLS transports, and runs the backup/verify/restore engine
// (PLAN §1, §6).
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"dockback/internal/api"
	"dockback/internal/config"
	"dockback/internal/dockercli"
	"dockback/internal/store"
	"dockback/internal/version"
	"dockback/internal/web"
)

// exitUsage is the conventional exit status for a command-line mistake.
const exitUsage = 2

func main() {
	if len(os.Args) > 1 {
		os.Exit(runCommand(os.Args[1:], os.Stdout, os.Stderr))
	}
	if err := run(); err != nil {
		log.Fatalf("fatal: %v", err)
	}
}

// runCommand handles the binary's one-shot flags and refuses everything else.
//
// Refusing matters: an unrecognised argument used to fall through to the
// server, so `dockback -version` run inside the container booted a SECOND
// DockBack against the live data directory. It cleared the work directories
// and every session before failing on the busy port.
func runCommand(args []string, stdout, stderr io.Writer) int {
	switch args[0] {
	case "-healthcheck", "--healthcheck":
		// The Docker HEALTHCHECK entry: the distroless image has no shell or
		// curl, so the binary probes its own liveness endpoint.
		return healthcheck()
	case "-version", "--version":
		fmt.Fprintln(stdout, version.Version)
		return 0
	default:
		fmt.Fprintf(stderr, "dockback: unknown argument %q. Supported: -healthcheck, -version; run with no arguments to start the server.\n", args[0])
		return exitUsage
	}
}

// healthcheck GETs the public /healthz endpoint over loopback and returns a
// process exit code (0 = healthy). It reads only PORT — no DB or config load.
func healthcheck() int {
	port := os.Getenv("PORT")
	if port == "" {
		port = "28734"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck: status", resp.StatusCode)
		return 1
	}
	return 0
}

func run() error {
	// F106: align the Go runtime with the container's cgroup limits BEFORE any
	// real work. Go does not read cgroups on its own, so without this a backup
	// can grow the heap past the container's memory cap and be OOM-killed
	// mid-run, and GOMAXPROCS would be sized from host cores rather than the CPU
	// quota. Raising a container's limits does nothing until the runtime is told.
	log.Print(config.TuneRuntime())

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// Ensure writable dirs exist (read-only rootfs means these are mounts).
	for _, d := range []string{cfg.DataDir, cfg.BackupsDir, cfg.TmpDir, cfg.WorkDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return fmt.Errorf("creating %s: %w", d, err)
		}
	}

	// One DockBack per data directory, settled BEFORE anything below touches
	// shared state: the startup cleanup deletes work directories and clears
	// sessions, which a second instance must never do to a running one.
	releaseLock, err := config.AcquireInstanceLock(cfg.DataDir)
	if err != nil {
		return err
	}
	defer releaseLock()

	// Clear crash-orphaned backup spool dirs (a backup interrupted mid-run leaves
	// a dback-* work dir; nothing is running yet at startup, so they're junk) —
	// reclaims disk before we begin (pairs with FailRunningBackups below).
	if leftovers, _ := filepath.Glob(filepath.Join(cfg.WorkDir, "dback-*")); len(leftovers) > 0 {
		for _, d := range leftovers {
			_ = os.RemoveAll(d)
		}
		log.Printf("startup: cleared %d orphaned backup work dir(s) in %s", len(leftovers), cfg.WorkDir)
	}

	// Apply a staged application-config restore (if any) BEFORE opening the DB —
	// the only safe moment to replace the live database file (PLAN §6.5/§9.3). A
	// staged file that fails validation is set aside, never applied.
	if err := store.ApplyPendingRestore(cfg.DataDir); err != nil {
		log.Printf("restore: %v", err)
	}

	// A database that does not exist yet is a fresh install: the only moment
	// DockBack chooses defaults on the operator's behalf.
	dbPath := filepath.Join(cfg.DataDir, "dockback.db")
	_, statErr := os.Stat(dbPath)
	freshInstall := errors.Is(statErr, fs.ErrNotExist)
	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	// Bootstrap the single admin account (PLAN §8.2).
	if pw, rejected, err := api.EnsureAdmin(st, cfg.AdminUser, cfg.AdminPass, cfg.MinPasswordLen); err != nil {
		return err
	} else if pw != "" {
		log.Printf("=====================================================================")
		log.Printf(" DockBack first-run: created admin user %q", cfg.AdminUser)
		log.Printf(" GENERATED PASSWORD: %s", pw)
		if rejected {
			// F203: say why their password was not used, or the generated one
			// looks like the app ignored a setting at random.
			log.Printf(" (DOCKBACK_ADMIN_PASSWORD was shorter than the %d-character minimum", cfg.MinPasswordLen)
			log.Printf("  and was NOT used. Set a longer one, or sign in with the password above.)")
		} else {
			log.Printf(" Save it now — it is not shown again. Set DOCKBACK_ADMIN_PASSWORD to pin one.")
		}
		log.Printf("=====================================================================")
	}
	// Reflect a changed DOCKBACK_ADMIN_USER on the existing account (rename only,
	// keyed by stable id — password/2FA/sessions/data preserved). Only when the
	// env var is explicitly set, so removing it never reverts the name.
	if cfg.AdminUserExplicit {
		if old, err := api.SyncAdminUsername(st, cfg.AdminUser); err != nil {
			return err
		} else if old != "" {
			log.Printf("admin account renamed %q -> %q (credentials, 2FA and data preserved)", old, cfg.AdminUser)
		}
	}

	// Force re-authentication after any restart/rebuild/update: sessions never
	// survive a process restart.
	if err := st.ClearSessions(); err != nil {
		log.Printf("startup: clear sessions: %v", err)
	} else {
		log.Printf("startup: cleared sessions — sign-in required")
	}

	reg := dockercli.NewRegistry()

	// Auto-register the local node (transport ①) on first run (PLAN §8.1).
	if cfg.LocalDockerHost != "" {
		if _, err := st.GetNode("local"); errors.Is(err, store.ErrNotFound) {
			n := &store.Node{
				ID: "local", Name: "local", Cluster: "default",
				Transport: dockercli.TransportLocalProxy, Address: cfg.LocalDockerHost,
				Status: "unknown",
			}
			if err := st.UpsertNode(n); err != nil {
				return err
			}
			log.Printf("registered local node via %s", cfg.LocalDockerHost)
		}
	}

	if freshInstall {
		_, localErr := st.GetNode("local")
		if summary, err := api.ApplyFreshInstallDefaults(st, localErr == nil); err != nil {
			log.Printf("fresh install: could not apply the defaults: %v", err)
		} else {
			log.Printf("fresh install: %s", summary)
		}
	}

	// Load all nodes into the connection registry.
	nodes, err := st.ListNodes()
	if err != nil {
		return err
	}
	for _, n := range nodes {
		// Node secrets are sealed at rest (F2); hand the registry the plaintext.
		// A legacy (pre-F2) plaintext blob passes through unchanged and is sealed
		// in the DB by the one-time migration in api.New.
		reg.Set(dockercli.NodeConn{ID: n.ID, Transport: n.Transport, Address: n.Address, Secret: api.OpenNodeSecret(n.SecretEnc, cfg.EncryptionKey)})
	}
	log.Printf("loaded %d node(s)", len(nodes))

	uiFS, err := web.FS()
	if err != nil {
		return err
	}

	srv := api.New(cfg, st, reg)
	srv.SetRecoveryTool("dockback-recover.py", recoverScript) // offline recovery kit (F35)
	srv.StartBackground()                                     // begin polling node inventory into the cache (PLAN §4.13)
	handler := srv.Handler(uiFS)

	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		var err error
		switch {
		case cfg.TLSCert != "" && cfg.TLSKey != "":
			log.Printf("DockBack listening on https://%s (TLS: cert/key)", addr)
			err = httpSrv.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
		case cfg.TLSSelfSigned:
			tc, terr := config.SelfSignedTLSConfig(cfg.Host)
			if terr != nil {
				log.Fatalf("self-signed TLS: %v", terr)
			}
			httpSrv.TLSConfig = tc
			log.Printf("DockBack listening on https://%s (TLS: self-signed — browsers will warn)", addr)
			err = httpSrv.ListenAndServeTLS("", "")
		default:
			log.Printf("DockBack listening on http://%s", addr)
			err = httpSrv.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server error: %v", err)
		}
	}()

	// Graceful shutdown — on a signal, or on a requested restart (e.g. to apply a
	// staged application-config restore: the process exits and the container's
	// restart policy brings it back up, applying the restore on the way).
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-stop:
		log.Printf("shutting down…")
	case <-srv.RestartRequested():
		log.Printf("application-config restore staged — restarting to apply…")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return httpSrv.Shutdown(ctx)
}
