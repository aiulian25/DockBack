package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"dockback/internal/dockercli"
	"dockback/internal/egress"
)

// SFTP destination (F66): a plain Linux box / Pi / NAS reachable over SSH — the
// commonest homelab offsite. Auth is a private key or a password; the server's
// host key is PINNED (trust-on-first-use) with the exact refusal posture nodes
// use: a changed key refuses every operation until the operator resets the pin.
// Per-op sessions like SMB (dial → auth → op → close); uploads are atomic via
// temp-name + rename so a torn transfer never looks like a finished archive.

const sftpDialTimeout = 15 * time.Second

// SFTP implements Backend (+ Lister, Pinger, Capacity) over pkg/sftp.
type SFTP struct {
	addr string // host:port
	user string
	pass string
	key  []byte // PEM private key ("" = password auth)
	pkPw string // optional private-key passphrase
	dir  string // remote base directory (all keys confined under it)

	pinned string // authorized-keys line of the pinned host key ("" = not pinned yet)

	// learned records a key pinned in-memory during THIS backend's lifetime
	// (first-use with no stored pin). The API layer reads it back and persists
	// it into the destination's sealed config, making the pin durable.
	mu      sync.Mutex
	learned ssh.PublicKey

	// F97: an optional post-write retention lock (chmod, plus a best-effort
	// `chattr +i` where the remote user is permitted).
	retentionLock
}

// NewSFTP builds the backend from a destination config map. Required: host,
// user, and one of private_key / password. Optional: port (22), dir, host_key
// (the pinned authorized-keys line, managed via LearnedHostKey / reset).
func NewSFTP(cfg map[string]string) (Backend, error) {
	host := strings.TrimSpace(cfg["host"])
	if host == "" {
		return nil, fmt.Errorf("sftp: host is required")
	}
	port := strings.TrimSpace(cfg["port"])
	if port == "" {
		port = "22"
	}
	if _, err := strconv.Atoi(port); err != nil {
		return nil, fmt.Errorf("sftp: invalid port %q", port)
	}
	// The guard is against a host smuggling its own port or path, which is why a
	// colon is refused. An IPv6 literal is all colons and is not that: it is
	// checked as an address first, and JoinHostPort below brackets it correctly.
	if net.ParseIP(host) == nil && strings.ContainsAny(host, ":/") {
		return nil, fmt.Errorf("sftp: host must be a bare hostname or IP (set the port separately)")
	}
	user := strings.TrimSpace(cfg["user"])
	if user == "" {
		return nil, fmt.Errorf("sftp: user is required")
	}
	key := strings.TrimSpace(cfg["private_key"])
	pass := cfg["password"]
	if key == "" && pass == "" {
		return nil, fmt.Errorf("sftp: a private key or a password is required")
	}
	return &SFTP{
		addr:   net.JoinHostPort(host, port),
		user:   user,
		pass:   pass,
		key:    []byte(key),
		pkPw:   cfg["key_passphrase"],
		dir:    strings.Trim(strings.ReplaceAll(cfg["dir"], "\\", "/"), "/"),
		pinned: strings.TrimSpace(cfg["host_key"]),

		retentionLock: retentionLock{lockDays: parseRetentionLockDays(cfg)},
	}, nil
}

// hostKeyCallback wires the shared TOFU pinning used for nodes (F1/F67): a
// stored pin must match exactly or the connection is REFUSED with the typed
// host-key-changed error; no stored pin accepts and records first use.
func (s *SFTP) hostKeyCallback() ssh.HostKeyCallback {
	load := func() ([]byte, bool) {
		if s.pinned == "" {
			return nil, false
		}
		return []byte(s.pinned), true
	}
	save := func(pub ssh.PublicKey) {
		s.mu.Lock()
		s.learned = pub
		s.mu.Unlock()
	}
	return dockercli.PinningHostKeyCallback(load, save)
}

// LearnedHostKey returns the host key pinned on first use during this
// backend's lifetime (authorized-keys line + SHA256 fingerprint), so the API
// layer can persist it into the destination's sealed config.
func (s *SFTP) LearnedHostKey() (authorizedKey, fingerprint string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.learned == nil {
		return "", "", false
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.learned))), ssh.FingerprintSHA256(s.learned), true
}

// sshDial opens one authenticated SSH connection. Split out of session() so a
// plain remote command (F97's best-effort chattr) reuses exactly the same auth,
// egress allow-list and host-key pinning as every data operation.
func (s *SFTP) sshDial() (*ssh.Client, error) {
	// Egress allow-list (PLAN §3.10): refuse before dialing a non-allowed host.
	if err := egress.Default().Enforce(s.addr); err != nil { // F207
		return nil, err
	}
	var auth []ssh.AuthMethod
	if len(s.key) > 0 {
		var signer ssh.Signer
		var err error
		if s.pkPw != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase(s.key, []byte(s.pkPw))
		} else {
			signer, err = ssh.ParsePrivateKey(s.key)
		}
		if err != nil {
			return nil, fmt.Errorf("sftp: parse private key: %w", err)
		}
		auth = append(auth, ssh.PublicKeys(signer))
	}
	if s.pass != "" {
		auth = append(auth, ssh.Password(s.pass))
	}
	conn, err := ssh.Dial("tcp", s.addr, &ssh.ClientConfig{
		User:            s.user,
		Auth:            auth,
		HostKeyCallback: s.hostKeyCallback(),
		Timeout:         sftpDialTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("sftp connect %s: %w", s.addr, err)
	}
	return conn, nil
}

// session dials one SSH+SFTP session for one operation.
func (s *SFTP) session() (*sftp.Client, func(), error) {
	conn, err := s.sshDial()
	if err != nil {
		return nil, nil, err
	}
	cli, err := sftp.NewClient(conn)
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("sftp subsystem: %w", err)
	}
	return cli, func() { cli.Close(); conn.Close() }, nil
}

// full confines a key under the base directory (validateKey already rejected
// empty/".."/absolute segments).
func (s *SFTP) full(key string) string {
	p := strings.Trim(strings.ReplaceAll(key, "\\", "/"), "/")
	if s.dir != "" {
		p = s.dir + "/" + p
	}
	return p
}

// Put streams an object atomically: upload under a temp name, then rename into
// place — a crashed/torn transfer never looks like a finished archive.
func (s *SFTP) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	if err := validateKey(key); err != nil {
		return 0, err
	}
	return runCtx(ctx, func() (int64, error) { return s.put(key, r) })
}

func (s *SFTP) put(key string, r io.Reader) (int64, error) {
	cli, cleanup, err := s.session()
	if err != nil {
		return 0, err
	}
	defer cleanup()
	full := s.full(key)
	if dir := path.Dir(full); dir != "." && dir != "/" {
		if err := cli.MkdirAll(dir); err != nil {
			return 0, fmt.Errorf("sftp mkdir %q: %w", dir, err)
		}
	}
	tmp := full + fmt.Sprintf(".dback-tmp-%d", time.Now().UnixNano())
	f, err := cli.Create(tmp)
	if err != nil {
		return 0, fmt.Errorf("sftp create %q: %w", tmp, err)
	}
	n, err := io.Copy(f, r)
	cerr := f.Close()
	if err == nil {
		err = cerr
	}
	if err != nil {
		_ = cli.Remove(tmp)
		return 0, err
	}
	// Rename into place (PosixRename overwrites atomically where supported).
	if err := cli.PosixRename(tmp, full); err != nil {
		_ = cli.Remove(full) // plain rename cannot overwrite on some servers
		if err = cli.Rename(tmp, full); err != nil {
			_ = cli.Remove(tmp)
			return 0, fmt.Errorf("sftp rename into place: %w", err)
		}
	}
	return n, nil
}

// Get opens an object; the session's cleanup is bound to the ReadCloser.
func (s *SFTP) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	return runCtx(ctx, func() (io.ReadCloser, error) { return s.get(key) })
}

func (s *SFTP) get(key string) (io.ReadCloser, error) {
	cli, cleanup, err := s.session()
	if err != nil {
		return nil, err
	}
	f, err := cli.Open(s.full(key))
	if err != nil {
		cleanup()
		return nil, err
	}
	return &sftpReadCloser{f: f, cleanup: cleanup}, nil
}

type sftpReadCloser struct {
	f       *sftp.File
	cleanup func()
}

func (r *sftpReadCloser) Read(p []byte) (int, error) { return r.f.Read(p) }
func (r *sftpReadCloser) Close() error {
	err := r.f.Close()
	r.cleanup()
	return err
}

// Delete removes an object (idempotent — a missing object is not an error).
func (s *SFTP) Delete(ctx context.Context, key string) error {
	if err := validateKey(key); err != nil {
		return err
	}
	_, err := runCtx(ctx, func() (struct{}, error) {
		cli, cleanup, err := s.session()
		if err != nil {
			return struct{}{}, err
		}
		defer cleanup()
		full := s.full(key)
		err = cli.Remove(full)
		if err == nil || strings.Contains(strings.ToLower(err.Error()), "not exist") {
			return struct{}{}, nil
		}
		// F97: if this destination locks its copies, the removal may be refused by
		// an immutable flag or a stripped write bit THIS backend applied. Prune only
		// calls Delete once the recorded lock has expired, so undoing our own lock
		// here is the intended end of its life — not a way around it. Best-effort:
		// the original error stands if the retry also fails.
		if s.lockDays > 0 {
			s.tryChattr(full, "-i")
			_ = cli.Chmod(full, 0o640)
			if rerr := cli.Remove(full); rerr == nil {
				return struct{}{}, nil
			}
		}
		return struct{}{}, err
	})
	return err
}

// LockUntil makes an already-written object read-only on the remote box (F97).
//
// Two layers, deliberately in this order:
//
//  1. chmod 0440 — always works, and stops an in-place overwrite.
//  2. `chattr +i` — a genuine kernel-level immutable flag on ext4/xfs/btrfs that
//     refuses deletion even by the owner. It needs CAP_LINUX_IMMUTABLE, which the
//     SSH user usually does NOT have, so failure is expected and non-fatal: a
//     destination that cannot take the stronger lock still keeps the weaker one
//     rather than failing the mirror.
//
// The lock EXPIRY is not written to the remote host — there is nowhere portable
// to record it. It lives in the backup's location entry, which is what prune
// reads before deciding a copy may go.
func (s *SFTP) LockUntil(ctx context.Context, key string, _ time.Time) error {
	if err := validateKey(key); err != nil {
		return err
	}
	_, err := runCtx(ctx, func() (struct{}, error) {
		cli, cleanup, err := s.session()
		if err != nil {
			return struct{}{}, err
		}
		defer cleanup()
		full := s.full(key)
		if err := cli.Chmod(full, 0o440); err != nil {
			return struct{}{}, fmt.Errorf("sftp retention lock %q: %w", key, err)
		}
		s.tryChattr(full, "+i") // best-effort; absence of the privilege is normal
		return struct{}{}, nil
	})
	return err
}

// tryChattr attempts a kernel immutable-flag change over the same SSH transport.
//
// Entirely best-effort and silent: the remote user usually lacks the capability,
// the filesystem may not support the flag, and `chattr` may not be installed. The
// path is passed as an ARGUMENT after `--`, never interpolated into a shell
// string, so no key spelling can be read as shell syntax.
func (s *SFTP) tryChattr(full, flag string) {
	conn, err := s.sshDial()
	if err != nil {
		return
	}
	defer conn.Close()
	sess, err := conn.NewSession()
	if err != nil {
		return
	}
	defer sess.Close()
	_ = sess.Run("chattr " + flag + " -- " + shellQuote(full))
}

// shellQuote wraps a path in single quotes for a remote shell, escaping any
// embedded single quote. Keys are already validated (no traversal, no absolute
// segments) — this is the second layer, so that even a key spelling that slipped
// through cannot be read as shell syntax by the remote sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Stat reports size + existence.
func (s *SFTP) Stat(ctx context.Context, key string) (int64, bool, error) {
	if err := validateKey(key); err != nil {
		return 0, false, err
	}
	type sr struct {
		size   int64
		exists bool
	}
	r, err := runCtx(ctx, func() (sr, error) {
		cli, cleanup, err := s.session()
		if err != nil {
			return sr{}, err
		}
		defer cleanup()
		fi, err := cli.Stat(s.full(key))
		if err != nil {
			// pkg/sftp normalises SSH_FX_NO_SUCH_FILE to fs.ErrNotExist; a
			// permission error or a dropped session is NOT an absent file.
			// See Backend.Stat.
			if errors.Is(err, fs.ErrNotExist) {
				return sr{}, nil
			}
			return sr{}, fmt.Errorf("sftp stat %q: %w", key, err)
		}
		return sr{fi.Size(), true}, nil
	})
	return r.size, r.exists, err
}

// List enumerates every object under prefix RECURSIVELY (breadth-first) —
// the uniform Lister contract adopt/backfill rely on (F20/F51).
func (s *SFTP) List(ctx context.Context, prefix string) ([]string, error) {
	return runCtx(ctx, func() ([]string, error) {
		cli, cleanup, err := s.session()
		if err != nil {
			return nil, err
		}
		defer cleanup()
		out := []string{}
		queue := []string{strings.Trim(strings.ReplaceAll(prefix, "\\", "/"), "/")}
		for len(queue) > 0 {
			rel := queue[0]
			queue = queue[1:]
			des, derr := cli.ReadDir(s.full(rel))
			if derr != nil {
				continue // missing/inaccessible dir → skip (empty for the start prefix)
			}
			for _, de := range des {
				child := de.Name()
				if rel != "" {
					child = rel + "/" + de.Name()
				}
				if de.IsDir() {
					queue = append(queue, child)
					continue
				}
				out = append(out, child)
			}
		}
		return out, nil
	})
}

// FreeBytes / TotalBytes report the remote filesystem's space via the SFTP
// statvfs extension (0 when the server doesn't support it — capacity is then
// simply unknown, never an error).
func (s *SFTP) FreeBytes(ctx context.Context) (uint64, error) {
	f, _, err := s.statVFS(ctx)
	return f, err
}
func (s *SFTP) TotalBytes(ctx context.Context) (uint64, error) {
	_, t, err := s.statVFS(ctx)
	return t, err
}

func (s *SFTP) statVFS(ctx context.Context) (free, total uint64, err error) {
	type vfs struct{ free, total uint64 }
	r, err := runCtx(ctx, func() (vfs, error) {
		cli, cleanup, err := s.session()
		if err != nil {
			return vfs{}, err
		}
		defer cleanup()
		base := s.dir
		if base == "" {
			base = "."
		}
		st, verr := cli.StatVFS(base)
		if verr != nil {
			return vfs{}, nil // extension unsupported → unknown, not an error
		}
		return vfs{free: st.FreeSpace(), total: st.TotalSpace()}, nil
	})
	return r.free, r.total, err
}

// Ping verifies dial+auth+subsystem (and, implicitly, the host-key pin).
func (s *SFTP) Ping(ctx context.Context) error {
	_, err := runCtx(ctx, func() (struct{}, error) {
		_, cleanup, err := s.session()
		if err != nil {
			return struct{}{}, err
		}
		cleanup()
		return struct{}{}, nil
	})
	return err
}

func (s *SFTP) Name() string { return fmt.Sprintf("sftp://%s@%s/%s", s.user, s.addr, s.dir) }
