package storage

import (
	"context"
	"errors"
	"net"
	"testing"
)

// Confining a backend to its own prefix is a property of the INTERFACE. Get was
// the one method without the check, so it was the one path the others could not
// rely on.
func TestS3GetValidatesTheKeyLikeEveryOtherMethod(t *testing.T) {
	be, err := NewS3(map[string]string{
		"endpoint": "127.0.0.1:1", "bucket": "backups",
		"access_key": "a", "secret_key": "s", "region": "us-east-1", "insecure": "true",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// A key that escapes the prefix, or names nothing. The endpoint above does not
	// exist, so an ErrUnsafeKey proves the guard ran BEFORE the network and a dial
	// error would prove it did not.
	for _, key := range []string{"../../etc/passwd", "a/../../b", "..", "a/./b/../..", ""} {
		if _, err := be.Get(ctx, key); !errors.Is(err, ErrUnsafeKey) {
			t.Errorf("Get(%q) = %v, want it refused before any request", key, err)
		}
		if _, _, err := be.Stat(ctx, key); !errors.Is(err, ErrUnsafeKey) {
			t.Errorf("Stat(%q) = %v, want ErrUnsafeKey", key, err)
		}
		if err := be.Delete(ctx, key); !errors.Is(err, ErrUnsafeKey) {
			t.Errorf("Delete(%q) = %v, want ErrUnsafeKey", key, err)
		}
	}
	// The local backend enforces the same contract, so the guarantee is the
	// interface's rather than one backend's.
	local, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := local.Get(ctx, "../../etc/passwd"); err == nil {
		t.Error("the local backend must refuse a traversal too")
	}
}

// The colon guard was aimed at a host smuggling its own port. An IPv6 literal is
// all colons and is not that — it made IPv6-only hosts unusable as destinations.
func TestSFTPAcceptsIPv6LiteralHosts(t *testing.T) {
	cfg := func(host string) map[string]string {
		return map[string]string{"host": host, "port": "2222", "user": "u", "password": "p"}
	}

	for _, host := range []string{"2001:db8::1", "::1", "fe80::1", "2001:0db8:0000:0000:0000:0000:0000:0001"} {
		raw, err := NewSFTP(cfg(host))
		if err != nil {
			t.Errorf("IPv6 host %q must be accepted: %v", host, err)
			continue
		}
		be, ok := raw.(*SFTP)
		if !ok {
			t.Fatalf("NewSFTP returned %T", raw)
		}
		// And it must be bracketed, or the dial parses the port out of the address.
		wantHost, wantPort, perr := net.SplitHostPort(be.addr)
		if perr != nil {
			t.Errorf("host %q produced an unparseable address %q: %v", host, be.addr, perr)
			continue
		}
		if wantPort != "2222" {
			t.Errorf("host %q lost its port: %q", host, be.addr)
		}
		if net.ParseIP(wantHost) == nil {
			t.Errorf("host %q did not round-trip: %q", host, wantHost)
		}
	}

	// What the guard is actually for is still refused.
	for _, bad := range []string{"example.com:22", "example.com/path", "host:2222"} {
		if _, err := NewSFTP(cfg(bad)); err == nil {
			t.Errorf("%q smuggles a port or path and must still be refused", bad)
		}
	}
	// Ordinary hosts are unaffected.
	for _, good := range []string{"nas.local", "192.0.2.10", "backup-host"} {
		if _, err := NewSFTP(cfg(good)); err != nil {
			t.Errorf("ordinary host %q must be accepted: %v", good, err)
		}
	}
}
