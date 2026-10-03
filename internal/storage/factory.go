package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"dockback/internal/egress"
)

// Destination type identifiers.
const (
	TypeLocal  = "local"
	TypeSMB    = "smb"
	TypeWebDAV = "webdav"
	TypeS3     = "s3"
	TypeSFTP   = "sftp" // F66: plain SSH box with pinned host key
)

// NewFromConfig builds a Backend for a destination type + config map (PLAN §4.9).
// "synology", "samba" and "nextcloud" are friendly aliases that map onto the
// underlying SMB/WebDAV transports.
func NewFromConfig(typ string, cfg map[string]string) (Backend, error) {
	// Egress allow-list enforcement (PLAN §3.10). For remote backends, refuse up
	// front if the configured host is not permitted — this surfaces a clear error
	// when a destination is added/tested, not only mid-backup. The local backend
	// makes no outbound connection and is always allowed.
	if host := remoteHost(typ, cfg); host != "" {
		if err := egress.Default().Enforce(host); err != nil { // F207: audit mode observes instead of blocking
			return nil, err
		}
	}
	switch typ {
	case TypeSMB, "samba", "cifs", "synology", "synology-smb":
		return NewSMB(cfg)
	case TypeWebDAV, "nextcloud", "synology-webdav":
		return NewWebDAV(cfg)
	case TypeS3, "b2", "backblaze", "minio", "wasabi":
		return NewS3(cfg)
	case TypeSFTP, "ssh":
		return NewSFTP(cfg)
	case TypeLocal:
		return NewLocalFromConfig(cfg)
	default:
		return nil, fmt.Errorf("unknown destination type %q", typ)
	}
}

// RemoteHost returns the raw outbound endpoint (host, host:port, or URL) a remote
// backend type connects to, or "" for the local backend. Exported so callers
// outside this package (e.g. the egress allow-list suggester, F54) derive the
// endpoint from the SAME per-type config keys the factory uses — keep it in sync
// with remoteHost.
func RemoteHost(typ string, cfg map[string]string) string { return remoteHost(typ, cfg) }

// remoteHost returns the outbound host for a remote backend type, or "" for the
// local backend (which never connects out). It mirrors the config key each
// backend reads its endpoint from.
func remoteHost(typ string, cfg map[string]string) string {
	switch typ {
	case TypeSMB, "samba", "cifs", "synology", "synology-smb":
		return strings.TrimSpace(cfg["host"])
	case TypeWebDAV, "nextcloud", "synology-webdav":
		return strings.TrimSpace(cfg["url"])
	case TypeS3, "b2", "backblaze", "minio", "wasabi":
		return strings.TrimSpace(cfg["endpoint"])
	case TypeSFTP, "ssh":
		return strings.TrimSpace(cfg["host"])
	default:
		return ""
	}
}

// Capacity reports free/total bytes if the backend exposes it.
type Capacity interface {
	FreeBytes(ctx context.Context) (uint64, error)
	TotalBytes(ctx context.Context) (uint64, error)
}

// Usage optionally reports bytes used at the destination — useful when total
// capacity is unknown (e.g. an unlimited Nextcloud quota still exposes "used").
type Usage interface {
	UsedBytes(ctx context.Context) (uint64, error)
}

// Pinger optionally reports whether a destination is reachable *right now* via a
// cheap liveness check that writes no data. Backends that can't answer cheaply
// simply don't implement it.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Lister optionally lists object keys under a prefix, RECURSIVELY (every object at
// or below the prefix, not just one directory level) — so a nested archive layout
// is fully enumerated (F20 adopt). Returned keys are in the same form Get/Delete
// accept (relative to the backend's configured dir/prefix).
type Lister interface {
	List(ctx context.Context, prefix string) ([]string, error)
}

// ListKeys lists keys under prefix using the backend's Lister, or returns an
// error if the backend can't list.
func ListKeys(ctx context.Context, b Backend, prefix string) ([]string, error) {
	l, ok := b.(Lister)
	if !ok {
		return nil, fmt.Errorf("%s does not support listing", b.Name())
	}
	return l.List(ctx, prefix)
}

// Reachable performs a cheap liveness check on a backend: its Ping if it has
// one, otherwise a capacity read (which still needs a live connection). Returns
// nil when the destination is reachable. Unlike Probe it never writes data, so
// it's safe to call on a periodic health refresh.
func Reachable(ctx context.Context, b Backend) error {
	if p, ok := b.(Pinger); ok {
		return p.Ping(ctx)
	}
	_, err := b.FreeBytes(ctx)
	return err
}

// Probe verifies a backend works by writing, reading back, and deleting a tiny
// marker object — a real round-trip, not just a connect (PLAN §4.4 pre-flight).
func Probe(ctx context.Context, b Backend) error {
	key := fmt.Sprintf(".dockback-probe-%d", time.Now().UnixNano())
	payload := []byte("dockback-probe")
	if _, err := b.Put(ctx, key, bytes.NewReader(payload)); err != nil {
		return fmt.Errorf("write test failed: %w", err)
	}
	defer b.Delete(ctx, key)
	rc, err := b.Get(ctx, key)
	if err != nil {
		return fmt.Errorf("read-back failed: %w", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, payload) {
		return fmt.Errorf("read-back mismatch")
	}
	return nil
}
