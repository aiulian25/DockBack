package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"dockback/internal/config"
	"dockback/internal/storage"
	"dockback/internal/store"
)

// F66 for the APP-backup path. The archive pushed to these destinations holds
// every node credential and every destination secret. The dialog and the README
// promise a pinned SSH host key; this path had none, so any on-path attacker
// could present its own key and receive the archive.

// startTestSSH runs a minimal SSH server with an sftp subsystem, so the pin is
// exercised against a real handshake rather than a stub.
func startTestSSH(t *testing.T, root string) (addr string, stop func()) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{PasswordCallback: func(m ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
		if m.User() == "u" && string(pw) == "p" {
			return nil, nil
		}
		return nil, errors.New("denied")
	}}
	cfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func(c net.Conn) {
				sconn, chans, reqs, herr := ssh.NewServerConn(c, cfg)
				if herr != nil {
					c.Close()
					return
				}
				go ssh.DiscardRequests(reqs)
				for nc := range chans {
					if nc.ChannelType() != "session" {
						_ = nc.Reject(ssh.UnknownChannelType, "")
						continue
					}
					ch, chReqs, cerr := nc.Accept()
					if cerr != nil {
						continue
					}
					go func(ch ssh.Channel, in <-chan *ssh.Request) {
						for req := range in {
							if req.Type == "subsystem" && len(req.Payload) >= 4 && string(req.Payload[4:]) == "sftp" {
								_ = req.Reply(true, nil)
								if srv, serr := sftp.NewServer(ch, sftp.WithServerWorkingDirectory(root)); serr == nil {
									_ = srv.Serve()
									srv.Close()
								}
								ch.Close()
								return
							}
							_ = req.Reply(false, nil)
						}
					}(ch, chReqs)
				}
				_ = sconn.Wait()
			}(conn)
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

func appDestServer(t *testing.T) *Server {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 3)
	}
	return &Server{store: testStore(t), cfg: &config.Config{EncryptionKey: key}}
}

func postAppDest(t *testing.T, s *Server, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest("POST", "/api/app/destinations", strings.NewReader(string(raw)))
	rec := httptest.NewRecorder()
	s.handleAppDestAdd(rec, r)
	return rec
}

// AC1 — adding an SFTP app destination pins the live host's key into the sealed
// config, so every later push authenticates the server.
func TestAppDestAddPinsSFTPHostKey(t *testing.T) {
	addr, stop := startTestSSH(t, t.TempDir())
	defer stop()
	host, port, _ := net.SplitHostPort(addr)
	s := appDestServer(t)

	rec := postAppDest(t, s, map[string]any{
		"name": "offsite-ssh", "type": "sftp",
		"config": map[string]string{"host": host, "port": port, "user": "u", "password": "p", "dir": "cfg"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var out map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	d, err := s.store.GetAppDestination(out["id"])
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := s.decryptDestConfig(d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(strings.TrimSpace(cfg["host_key"]), "ssh-ed25519 ") {
		t.Fatalf("no host key was pinned: %q", cfg["host_key"])
	}
	// The pin rides the sealed config: neither it nor the password is readable in
	// the stored bytes.
	for _, secret := range []string{"host_key", "ssh-ed25519", "password"} {
		if strings.Contains(string(d.ConfigEnc), secret) {
			t.Errorf("the stored config is not sealed — %q is readable in it", secret)
		}
	}
}

// AC2 — a host that cannot be reached cannot be pinned, so it cannot be saved.
// Accepting it would leave a destination that trusts whatever answers later.
func TestAppDestAddRefusesUnpinnableHost(t *testing.T) {
	s := appDestServer(t)
	// Port 1 on loopback: nothing listens, and the dial fails immediately.
	rec := postAppDest(t, s, map[string]any{
		"name": "offsite-ssh", "type": "sftp",
		"config": map[string]string{"host": "127.0.0.1", "port": "1", "user": "u", "password": "p", "dir": "cfg"},
	})
	if rec.Code == http.StatusOK {
		t.Fatal("an SFTP destination whose key cannot be pinned must not be saved")
	}
	if !strings.Contains(rec.Body.String(), "could not reach the SSH host to pin its key") {
		t.Errorf("the refusal must say why: %s", rec.Body.String())
	}
	if ds, _ := s.store.ListAppDestinations(); len(ds) != 0 {
		t.Errorf("nothing should have been stored, got %d rows", len(ds))
	}
}

// AC3 — the regression that matters: every other destination type is unchanged.
// Pinning must not add a reachability requirement to an S3 or WebDAV target.
func TestAppDestAddUnchangedForOtherTypes(t *testing.T) {
	s := appDestServer(t)
	rec := postAppDest(t, s, map[string]any{
		"name": "b2", "type": "s3",
		"config": map[string]string{"endpoint": "s3.example.invalid", "bucket": "backups", "access_key": "a", "secret_key": "b"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("a non-SSH destination must save without any host being reachable: %s", rec.Body.String())
	}
}

// A destination that predates pinning gains one on its first successful use,
// and the pin is written back sealed. This is the upgrade path for rows already
// in the database.
func TestAppDestPinsOnFirstUse(t *testing.T) {
	addr, stop := startTestSSH(t, t.TempDir())
	defer stop()
	host, port, _ := net.SplitHostPort(addr)
	s := appDestServer(t)

	// Planted directly, the way an existing row looks: no host_key.
	cfg := map[string]string{"host": host, "port": port, "user": "u", "password": "p", "dir": "cfg"}
	enc, err := s.encryptDestConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	d := &store.Destination{ID: "old1", Name: "legacy-ssh", Type: "sftp", Enabled: true, Status: "unknown", ConfigEnc: enc}
	if err := s.store.CreateAppDestination(d); err != nil {
		t.Fatal(err)
	}

	be, err := storage.NewFromConfig("sftp", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.Reachable(context.Background(), be); err != nil {
		t.Fatalf("the test host should be reachable: %v", err)
	}
	s.persistLearnedAppDestHostKey(d, be, cfg)

	stored, err := s.store.GetAppDestination("old1")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.decryptDestConfig(stored)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(strings.TrimSpace(got["host_key"]), "ssh-ed25519 ") {
		t.Fatalf("first use must pin the host key, got %q", got["host_key"])
	}
	if got["password"] != "p" || got["user"] != "u" {
		t.Error("persisting the pin must not disturb the rest of the config")
	}
}

// An already-pinned destination is never silently re-pinned: that is what makes
// a changed key a refusal rather than a shrug.
func TestAppDestKeepsAnExistingPin(t *testing.T) {
	addr, stop := startTestSSH(t, t.TempDir())
	defer stop()
	host, port, _ := net.SplitHostPort(addr)
	s := appDestServer(t)

	const otherKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	cfg := map[string]string{"host": host, "port": port, "user": "u", "password": "p", "dir": "cfg", "host_key": otherKey}
	enc, _ := s.encryptDestConfig(cfg)
	d := &store.Destination{ID: "pinned1", Name: "pinned-ssh", Type: "sftp", Enabled: true, ConfigEnc: enc}
	if err := s.store.CreateAppDestination(d); err != nil {
		t.Fatal(err)
	}

	be, err := storage.NewFromConfig("sftp", cfg)
	if err != nil {
		t.Fatal(err)
	}
	// The server presents a DIFFERENT key than the pin, so the connection must be
	// refused rather than quietly accepted.
	if err := storage.Reachable(context.Background(), be); err == nil {
		t.Fatal("a host presenting a different key must be refused")
	}
	s.persistLearnedAppDestHostKey(d, be, cfg)

	stored, _ := s.store.GetAppDestination("pinned1")
	got, _ := s.decryptDestConfig(stored)
	if strings.TrimSpace(got["host_key"]) != otherKey {
		t.Errorf("an existing pin must never be overwritten: %q", got["host_key"])
	}
}
