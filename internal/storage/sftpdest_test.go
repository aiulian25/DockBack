package storage

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"dockback/internal/dockercli"
)

// F66 SFTP destination: config validation, atomic temp+rename Put, recursive
// List, TOFU pin + changed-key refusal — against a real in-process SSH+SFTP
// server rooted in a temp dir.

// startSFTPServer runs a minimal SSH server (password auth u/p) whose "sftp"
// subsystem serves root. Returns the address, the host public key, and a stop.
func startSFTPServer(t *testing.T, root string) (addr string, hostPub ssh.PublicKey, stop func()) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(m ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			if m.User() == "u" && string(pw) == "p" {
				return nil, nil
			}
			return nil, errors.New("denied")
		},
	}
	cfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
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
							// subsystem payload: uint32 len + name
							if req.Type == "subsystem" && len(req.Payload) >= 4 && string(req.Payload[4:]) == "sftp" {
								_ = req.Reply(true, nil)
								srv, serr := sftp.NewServer(ch, sftp.WithServerWorkingDirectory(root))
								if serr == nil {
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
	return ln.Addr().String(), signer.PublicKey(), func() { ln.Close(); close(done) }
}

func sftpTestCfg(addr string) map[string]string {
	host, port, _ := net.SplitHostPort(addr)
	return map[string]string{"host": host, "port": port, "user": "u", "password": "p", "dir": "backups"}
}

func TestSFTPConfigValidation(t *testing.T) {
	for _, c := range []map[string]string{
		{},                         // nothing
		{"host": "h"},              // no user
		{"host": "h", "user": "u"}, // no credential
		{"host": "h:22", "user": "u", "password": "p"},             // port smuggled into host
		{"host": "h", "port": "abc", "user": "u", "password": "p"}, // bad port
	} {
		if _, err := NewSFTP(c); err == nil {
			t.Fatalf("config %v must be rejected", c)
		}
	}
	if _, err := NewSFTP(map[string]string{"host": "h", "user": "u", "password": "p"}); err != nil {
		t.Fatalf("minimal valid config rejected: %v", err)
	}
}

func TestSFTPRoundTripAndPin(t *testing.T) {
	root := t.TempDir()
	addr, hostPub, stop := startSFTPServer(t, root)
	defer stop()
	ctx := context.Background()

	// First use with no pin: connects (TOFU) and learns the host key.
	be, err := NewSFTP(sftpTestCfg(addr))
	if err != nil {
		t.Fatal(err)
	}
	sf := be.(*SFTP)
	if err := sf.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	ak, fp, learned := sf.LearnedHostKey()
	if !learned || ak == "" || !strings.HasPrefix(fp, "SHA256:") {
		t.Fatalf("first use must learn the host key: ak=%q fp=%q", ak, fp)
	}
	if want := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(hostPub))); ak != want {
		t.Fatalf("learned key mismatch:\n got %q\nwant %q", ak, want)
	}

	// Pinned backend: full Put/Get/Stat/List/Delete parity.
	cfg := sftpTestCfg(addr)
	cfg["host_key"] = ak
	be, err = NewSFTP(cfg)
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("archive-bytes-"), 100)
	if _, err := be.Put(ctx, "node/stack/app/b1.dback", bytes.NewReader(payload)); err != nil {
		t.Fatalf("put: %v", err)
	}
	if _, err := be.Put(ctx, "node/app2/b2.dback", strings.NewReader("x")); err != nil {
		t.Fatalf("put2: %v", err)
	}
	// The object landed under the base dir, and no temp junk remains (atomic Put).
	if _, err := os.Stat(filepath.Join(root, "backups", "node", "stack", "app", "b1.dback")); err != nil {
		t.Fatalf("object not at expected path: %v", err)
	}
	var stray []string
	_ = filepath.Walk(root, func(p string, fi os.FileInfo, _ error) error {
		if fi != nil && !fi.IsDir() && strings.Contains(fi.Name(), ".dback-tmp-") {
			stray = append(stray, p)
		}
		return nil
	})
	if len(stray) > 0 {
		t.Fatalf("temp upload names left behind: %v", stray)
	}

	rc, err := be.Get(ctx, "node/stack/app/b1.dback")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, payload) {
		t.Fatal("round-trip bytes differ")
	}
	if n, ok, err := be.Stat(ctx, "node/stack/app/b1.dback"); err != nil || !ok || n != int64(len(payload)) {
		t.Fatalf("stat: n=%d ok=%v err=%v", n, ok, err)
	}
	if _, ok, err := be.Stat(ctx, "node/missing.dback"); err != nil || ok {
		t.Fatalf("missing stat: ok=%v err=%v", ok, err)
	}

	// Recursive List from the base (adopt/backfill contract).
	keys, err := be.(Lister).List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(keys)
	if len(keys) != 2 || keys[0] != "node/app2/b2.dback" || keys[1] != "node/stack/app/b1.dback" {
		t.Fatalf("recursive list = %v", keys)
	}

	// Probe (write/read/delete round-trip) — the Test Connection path.
	if err := Probe(ctx, be); err != nil {
		t.Fatalf("probe: %v", err)
	}

	if err := be.Delete(ctx, "node/app2/b2.dback"); err != nil {
		t.Fatal(err)
	}
	if err := be.Delete(ctx, "node/app2/b2.dback"); err != nil {
		t.Fatalf("delete must be idempotent: %v", err)
	}

	// Unsafe keys are refused before any network use.
	if _, err := be.Put(ctx, "../escape.dback", strings.NewReader("x")); !errors.Is(err, ErrUnsafeKey) {
		t.Fatalf("traversal key must be refused, got %v", err)
	}

	// A CHANGED host key refuses every operation with the typed pin error.
	cfg2 := sftpTestCfg(addr)
	_, wrongPriv, _ := ed25519.GenerateKey(rand.Reader)
	wrongSigner, _ := ssh.NewSignerFromKey(wrongPriv)
	cfg2["host_key"] = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(wrongSigner.PublicKey())))
	be2, err := NewSFTP(cfg2)
	if err != nil {
		t.Fatal(err)
	}
	if err := be2.(*SFTP).Ping(ctx); !errors.Is(err, dockercli.ErrHostKeyChanged) {
		t.Fatalf("changed host key must refuse with the pin error, got %v", err)
	}
	if _, err := be2.Put(ctx, "a/b.dback", strings.NewReader("x")); !errors.Is(err, dockercli.ErrHostKeyChanged) {
		t.Fatalf("upload against a changed key must refuse, got %v", err)
	}
}
