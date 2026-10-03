package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"path"
	"strings"
	"time"

	"dockback/internal/egress"

	"github.com/hirochachacha/go-smb2"
)

// smbIdleTimeout bounds INACTIVITY on the SMB connection, not total transfer
// time. The connection's deadline is refreshed on every streamed read/write
// chunk (see deadlineWriter / smbReadCloser), so a slow-but-steady multi-hour
// upload or download completes while a genuinely hung/unresponsive server still
// trips after this long with no I/O progress. It replaces a former ABSOLUTE
// 15-minute connection deadline that silently failed any SMB transfer taking
// longer than 15 minutes (e.g. a large image-included backup to a NAS),
// regardless of steady progress. The mirror's own no-progress stall guard
// (10 min, engine.go) is the higher-level protection; this is the low-level
// backstop that actually unblocks a hung socket op. (PLAN §4.5/§4.9/§9.1.)
const smbIdleTimeout = 3 * time.Minute

// deadlineWriter refreshes the connection's I/O deadline before each write, so
// each chunk gets a fresh idle window and only true inactivity (a hung server)
// trips smbIdleTimeout — the transfer as a whole is never capped. It exposes
// only Write so io.Copy can't take a ReaderFrom fast-path that would bypass the
// per-chunk refresh.
type deadlineWriter struct {
	w    io.Writer
	conn net.Conn
}

func (d *deadlineWriter) Write(p []byte) (int, error) {
	_ = d.conn.SetDeadline(time.Now().Add(smbIdleTimeout))
	return d.w.Write(p)
}

// SMB is an SMB2/3 (Samba/CIFS) backup destination implemented entirely in
// userspace (no kernel mount), so it works in the hardened, non-root,
// read-only container (PLAN §4.9). Synology NAS over SMB uses this too.
type SMB struct {
	addr   string // host:port (default 445)
	share  string
	user   string
	pass   string
	domain string
	dir    string // base directory inside the share
	// F97: an optional post-write retention lock, applied as the SMB read-only
	// attribute.
	retentionLock
}

// NewSMB builds an SMB backend from a config map.
func NewSMB(cfg map[string]string) (*SMB, error) {
	host := strings.TrimSpace(cfg["host"])
	if host == "" {
		return nil, fmt.Errorf("smb: host is required")
	}
	if !strings.Contains(host, ":") {
		host += ":445"
	}
	share := strings.Trim(cfg["share"], "/")
	if share == "" {
		return nil, fmt.Errorf("smb: share is required")
	}
	return &SMB{
		addr:   host,
		share:  share,
		user:   cfg["username"],
		pass:   cfg["password"],
		domain: cfg["domain"],
		dir:    strings.Trim(strings.ReplaceAll(cfg["path"], "\\", "/"), "/"),

		retentionLock: retentionLock{lockDays: parseRetentionLockDays(cfg)},
	}, nil
}

// session dials + authenticates + mounts the share, returning the underlying
// net.Conn so streaming ops (put/get) can refresh its idle deadline as data
// flows. Caller must call cleanup.
func (s *SMB) session() (*smb2.Share, net.Conn, func(), error) {
	// Egress allow-list (PLAN §3.10): refuse before dialing a non-allowed host.
	if err := egress.Default().Enforce(s.addr); err != nil { // F207
		return nil, nil, nil, err
	}
	conn, err := net.DialTimeout("tcp", s.addr, 12*time.Second)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("smb dial %s: %w", s.addr, err)
	}
	// Initial idle deadline covering dial→auth→mount and the quick control ops;
	// streaming ops refresh it per chunk so it never caps a long transfer.
	_ = conn.SetDeadline(time.Now().Add(smbIdleTimeout))
	d := &smb2.Dialer{Initiator: &smb2.NTLMInitiator{User: s.user, Password: s.pass, Domain: s.domain}}
	sess, err := d.Dial(conn)
	if err != nil {
		conn.Close()
		return nil, nil, nil, fmt.Errorf("smb auth: %w", err)
	}
	fs, err := sess.Mount(s.share)
	if err != nil {
		sess.Logoff()
		conn.Close()
		return nil, nil, nil, fmt.Errorf("smb mount %q: %w", s.share, err)
	}
	cleanup := func() { fs.Umount(); sess.Logoff(); conn.Close() }
	return fs, conn, cleanup, nil
}

func (s *SMB) full(key string) string {
	p := strings.Trim(strings.ReplaceAll(key, "\\", "/"), "/")
	if s.dir != "" {
		p = s.dir + "/" + p
	}
	return p
}

// mkdirAll creates parent directories (SMB Share has no MkdirAll).
func mkdirAll(fs *smb2.Share, dir string) {
	dir = strings.Trim(dir, "/")
	if dir == "" {
		return
	}
	var cur string
	for _, seg := range strings.Split(dir, "/") {
		if cur == "" {
			cur = seg
		} else {
			cur = cur + "/" + seg
		}
		_ = fs.Mkdir(cur, 0o755) // ignore "already exists"
	}
}

// Put streams an object to the share.
func (s *SMB) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	if err := validateKey(key); err != nil {
		return 0, err
	}
	return runCtx(ctx, func() (int64, error) { return s.put(key, r) })
}

func (s *SMB) put(key string, r io.Reader) (int64, error) {
	fs, conn, cleanup, err := s.session()
	if err != nil {
		return 0, err
	}
	defer cleanup()
	full := s.full(key)
	mkdirAll(fs, path.Dir(full))
	f, err := fs.Create(full)
	if err != nil {
		return 0, fmt.Errorf("smb create %q: %w", full, err)
	}
	// Refresh the idle deadline per chunk so a long steady upload isn't capped.
	n, err := io.Copy(&deadlineWriter{w: f, conn: conn}, r)
	cerr := f.Close()
	if err != nil {
		return 0, err
	}
	return n, cerr
}

// Get opens an object. The cleanup is bound to the returned ReadCloser.
func (s *SMB) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	return runCtx(ctx, func() (io.ReadCloser, error) { return s.get(key) })
}

func (s *SMB) get(key string) (io.ReadCloser, error) {
	fs, conn, cleanup, err := s.session()
	if err != nil {
		return nil, err
	}
	f, err := fs.Open(s.full(key))
	if err != nil {
		cleanup()
		return nil, err
	}
	return &smbReadCloser{f: f, conn: conn, cleanup: cleanup}, nil
}

// Delete removes an object (idempotent). Never targets the share/folder root.
// LockUntil sets the SMB READ-ONLY attribute on an already-written object (F97).
//
// go-smb2 maps a mode with no owner-write bit onto FILE_ATTRIBUTE_READONLY, which
// is what a Windows/Samba server actually enforces — a client that respects the
// attribute (Explorer, Finder, most tooling, and the encrypt-in-place pattern
// ransomware uses) is refused. A client with write access to the share can still
// clear the attribute, so this is a speed bump, not WORM; the UI and docs say so.
func (s *SMB) LockUntil(ctx context.Context, key string, _ time.Time) error {
	if err := validateKey(key); err != nil {
		return err
	}
	_, err := runCtx(ctx, func() (struct{}, error) { return struct{}{}, s.lock(key) })
	return err
}

func (s *SMB) lock(key string) error {
	fs, _, cleanup, err := s.session()
	if err != nil {
		return err
	}
	defer cleanup()
	if err := fs.Chmod(s.full(key), 0o440); err != nil {
		return fmt.Errorf("smb retention lock %q: %w", key, err)
	}
	return nil
}

func (s *SMB) Delete(ctx context.Context, key string) error {
	if err := validateKey(key); err != nil {
		return err
	}
	_, err := runCtx(ctx, func() (struct{}, error) { return struct{}{}, s.del(key) })
	return err
}

func (s *SMB) del(key string) error {
	fs, _, cleanup, err := s.session()
	if err != nil {
		return err
	}
	defer cleanup()
	full := s.full(key)
	err = fs.Remove(full)
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "no such file") {
		return nil
	}
	// F97: a server that honours the READ-ONLY attribute refuses to remove the
	// file while it is set. Once DockBack has decided this copy may go (the
	// recorded lock has expired — prune checks that before ever calling Delete),
	// clearing the attribute is the intended completion of the lock's life, not a
	// way around it. Best-effort: if this fails, the original error stands.
	if err != nil && s.lockDays > 0 {
		if cerr := fs.Chmod(full, 0o660); cerr == nil {
			if rerr := fs.Remove(full); rerr == nil {
				return nil
			}
		}
	}
	return err
}

// Stat reports size + existence.
func (s *SMB) Stat(ctx context.Context, key string) (int64, bool, error) {
	if err := validateKey(key); err != nil {
		return 0, false, err
	}
	type sr struct {
		size   int64
		exists bool
	}
	r, err := runCtx(ctx, func() (sr, error) {
		sz, ex, e := s.stat(key)
		return sr{sz, ex}, e
	})
	return r.size, r.exists, err
}

func (s *SMB) stat(key string) (int64, bool, error) {
	share, _, cleanup, err := s.session()
	if err != nil {
		return 0, false, err
	}
	defer cleanup()
	fi, err := share.Stat(s.full(key))
	if err != nil {
		// The library maps STATUS_OBJECT_NAME_NOT_FOUND to fs.ErrNotExist; every
		// other failure (access denied, a dropped session) is unanswered, not
		// absent. See Backend.Stat.
		if errors.Is(err, fs.ErrNotExist) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("smb stat %q: %w", key, err)
	}
	return fi.Size(), true, nil
}

// FreeBytes reports free space on the share (PLAN §4.4).
func (s *SMB) FreeBytes(ctx context.Context) (uint64, error) {
	return runCtx(ctx, func() (uint64, error) { return s.freeBytes() })
}

func (s *SMB) freeBytes() (uint64, error) {
	fs, _, cleanup, err := s.session()
	if err != nil {
		return 0, err
	}
	defer cleanup()
	stat, err := fs.Statfs(".")
	if err != nil {
		return 0, nil // unknown
	}
	return stat.FreeBlockCount() * uint64(stat.BlockSize()), nil
}

// TotalBytes reports total capacity of the share (0 if unknown).
func (s *SMB) TotalBytes(ctx context.Context) (uint64, error) {
	return runCtx(ctx, func() (uint64, error) { return s.totalBytes() })
}

func (s *SMB) totalBytes() (uint64, error) {
	fs, _, cleanup, err := s.session()
	if err != nil {
		return 0, err
	}
	defer cleanup()
	stat, err := fs.Statfs(".")
	if err != nil {
		return 0, nil
	}
	return stat.TotalBlockCount() * uint64(stat.BlockSize()), nil
}

// Ping is a cheap reachability check — open an authenticated session to the
// share and close it. Writes nothing.
func (s *SMB) Ping(ctx context.Context) error {
	_, err := runCtx(ctx, func() (struct{}, error) {
		_, _, cleanup, err := s.session()
		if err != nil {
			return struct{}{}, err
		}
		cleanup()
		return struct{}{}, nil
	})
	return err
}

// List returns object keys directly under prefix (non-recursive, files only).
func (s *SMB) List(ctx context.Context, prefix string) ([]string, error) {
	return runCtx(ctx, func() ([]string, error) {
		fs, _, cleanup, err := s.session()
		if err != nil {
			return nil, err
		}
		defer cleanup()
		// Walk RECURSIVELY (breadth-first) so nested archive layouts are enumerated,
		// matching the S3/local backends — a uniform "every object under prefix"
		// contract (F20 adopt). A flat prefix still returns just its files.
		out := []string{}
		queue := []string{strings.Trim(strings.ReplaceAll(prefix, "\\", "/"), "/")}
		for len(queue) > 0 {
			rel := queue[0]
			queue = queue[1:]
			des, derr := fs.ReadDir(s.full(rel))
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

// Name identifies the backend.
func (s *SMB) Name() string { return fmt.Sprintf("smb://%s/%s/%s", s.addr, s.share, s.dir) }

type smbReadCloser struct {
	f       *smb2.File
	conn    net.Conn
	cleanup func()
}

// Read refreshes the idle deadline per chunk so a large download (restore /
// verification from SMB) isn't capped by total transfer time either.
func (r *smbReadCloser) Read(p []byte) (int, error) {
	_ = r.conn.SetDeadline(time.Now().Add(smbIdleTimeout))
	return r.f.Read(p)
}
func (r *smbReadCloser) Close() error {
	err := r.f.Close()
	r.cleanup()
	return err
}
