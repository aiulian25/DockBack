// Package dockercli is DockBack's multi-node Docker integration layer. It
// holds one pooled, long-lived client per managed host and supports all four
// connection transports (PLAN §2.14): the bundled socket-proxy sidecar,
// a remote socket-proxy over TCP, SSH (pure-Go, no shelling out), and daemon
// mTLS. Designed for fleets of 10-20+ nodes (PLAN §4.13): clients are reused,
// failures are isolated per node, and a dead node never blocks others.
package dockercli

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/client"
	"golang.org/x/crypto/ssh"
)

// Transport identifiers (stored on store.Node.Transport).
const (
	TransportLocalProxy = "local-proxy"
	TransportTCPProxy   = "tcp-proxy"
	TransportSSH        = "ssh"
	TransportMTLS       = "mtls"
)

// Connection tuning shared by all TCP-based transports (PLAN §4.13). A bounded
// dial keeps a dead/slow node from hanging a request, and TCP keep-alive lets a
// pooled, long-lived client notice a silently-dropped peer and reconnect.
const (
	dialTimeout  = 10 * time.Second
	tcpKeepAlive = 30 * time.Second
	// sshHandshakeTimeout bounds key exchange and authentication together. x/crypto's
	// NewClientConn applies no timeout of its own, and a server that accepts TCP and
	// then says nothing would otherwise hold the caller for ever.
	sshHandshakeTimeout = 30 * time.Second
	// sshChannelTimeout bounds opening one channel on an established connection.
	// Expiring closes that connection, and with it any stream sharing it, so this is
	// long enough that only a connection that is truly gone reaches it.
	sshChannelTimeout = 30 * time.Second
)

// Reconnect back-off bounds (PLAN §4.13). After a node fails to respond the
// background refresh stops re-dialing it every cycle and instead waits an
// exponentially growing cooldown (so recovery is still detected, but a dead node
// is probed rarely and never starves live nodes' refresh slots).
const (
	backoffBase     = 15 * time.Second
	backoffMax      = 5 * time.Minute
	backoffShiftMax = 5 // 15s<<5 = 8m, clamped to backoffMax
)

// NodeConn is the connection spec for one host, derived from a store.Node.
type NodeConn struct {
	ID        string
	Transport string
	Address   string // DOCKER_HOST-style address
	// Secret holds transport-specific credentials decrypted at use time:
	//   ssh  -> PEM private key bytes
	//   mtls -> PEM bundle "CA\n---\nCERT\n---\nKEY" (see splitMTLS)
	Secret []byte
}

// Registry pools clients keyed by node ID.
type Registry struct {
	mu      sync.Mutex
	clients map[string]*client.Client
	tunnels map[string]*sshTunnel // persistent SSH client per node (ssh transport)
	specs   map[string]NodeConn
	health  map[string]*nodeHealth // per-node reconnect back-off state

	// SSH host-key pinning (F1). Wired to the store by the API layer. LoadHostKey
	// returns a node's pinned host key (marshaled authorized-key bytes) if one
	// exists; SaveHostKey persists the key seen on first connect (TOFU). Both are
	// nil-safe (an un-wired registry simply can't pin).
	LoadHostKey func(nodeID string) ([]byte, bool)
	SaveHostKey func(nodeID string, pub []byte)

	// Volume-sidecar image pinning (F88), wired the same way. LoadSidecarPin
	// returns a node's pinned "<ref>|<digest>" if one exists; SaveSidecarPin
	// records what was seen on first use (TOFU). Both nil-safe — an un-wired
	// registry simply doesn't pin, and the sidecar behaves as it always did.
	LoadSidecarPin func(nodeID string) (string, bool)
	SaveSidecarPin func(nodeID, pin string)
}

// nodeHealth tracks consecutive connection failures so a dead node is re-probed
// on an exponential back-off instead of every refresh cycle (PLAN §4.13).
type nodeHealth struct {
	fails   int
	until   time.Time
	lastErr string
}

// NewRegistry creates an empty client registry.
func NewRegistry() *Registry {
	r := &Registry{
		clients: map[string]*client.Client{},
		tunnels: map[string]*sshTunnel{},
		specs:   map[string]NodeConn{},
		health:  map[string]*nodeHealth{},
	}
	// F88: publish this registry so the sidecar choke point can resolve a client
	// back to its node without threading a node id through ~20 call sites.
	activeRegistry.Store(r)
	return r
}

// detachedNode is a node's client and tunnel, out of the registry and ready to close.
type detachedNode struct {
	client *client.Client
	tunnel *sshTunnel
}

// close shuts down what was detached. It talks to a network that may not answer, so
// it runs after r.mu is released: one dead node must not hold up the whole fleet.
func (d detachedNode) close() {
	if d.client != nil {
		d.client.Close()
	}
	if d.tunnel != nil {
		d.tunnel.close()
	}
}

// detachNodeLocked takes the cached client and SSH tunnel for a node out of the
// registry for the caller to close once r.mu is released. It deliberately does NOT
// clear back-off state: Drop uses it to discard a broken client for reconnect, and
// the failure count must keep accumulating so the cooldown grows. Back-off is reset
// only on Set/Remove (a spec change or deletion). Caller must hold r.mu.
func (r *Registry) detachNodeLocked(id string) detachedNode {
	detached := detachedNode{client: r.clients[id], tunnel: r.tunnels[id]}
	delete(r.clients, id)
	delete(r.tunnels, id)
	return detached
}

// Backoff reports whether a node is currently in connection back-off — i.e. it
// failed recently and should not be re-dialed until the cooldown elapses — along
// with the last error for display. The background refresh consults this so a
// dead/slow node is skipped cheaply and never starves live nodes' refresh slots
// at fleet scale (PLAN §4.13). Explicit user actions bypass it (they call Get/
// Ping directly), so a manual "Test connection" always attempts a live connect.
func (r *Registry) Backoff(id string) (backed bool, lastErr string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	h := r.health[id]
	if h == nil {
		return false, ""
	}
	return time.Now().Before(h.until), h.lastErr
}

// RecordHealth updates a node's back-off state from a reachability result: a nil
// error clears it (the node is healthy again); a non-nil error grows an
// exponential cooldown capped at backoffMax (PLAN §4.13).
func (r *Registry) RecordHealth(id string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err == nil {
		delete(r.health, id)
		return
	}
	h := r.health[id]
	if h == nil {
		h = &nodeHealth{}
		r.health[id] = h
	}
	h.fails++
	h.lastErr = err.Error()
	d := backoffBase << min(h.fails-1, backoffShiftMax)
	if d > backoffMax {
		d = backoffMax
	}
	h.until = time.Now().Add(d)
}

// Set registers/updates a node's connection spec, discarding any cached client
// so the next Get rebuilds it.
func (r *Registry) Set(spec NodeConn) {
	r.mu.Lock()
	detached := r.detachNodeLocked(spec.ID)
	delete(r.health, spec.ID) // a (re)configured node is probed immediately
	r.specs[spec.ID] = spec
	r.mu.Unlock()
	detached.close()
}

// Remove drops a node and closes its client.
func (r *Registry) Remove(id string) {
	r.mu.Lock()
	detached := r.detachNodeLocked(id)
	delete(r.health, id)
	delete(r.specs, id)
	r.mu.Unlock()
	detached.close()
}

// Get returns a pooled client for the node, building it on first use.
func (r *Registry) Get(id string) (*client.Client, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.clients[id]; ok {
		return c, nil
	}
	spec, ok := r.specs[id]
	if !ok {
		return nil, fmt.Errorf("node %q not registered", id)
	}
	c, tunnel, err := r.build(spec)
	if err != nil {
		return nil, err
	}
	r.clients[id] = c
	if tunnel != nil {
		r.tunnels[id] = tunnel
	}
	return c, nil
}

// Drop discards a possibly-broken cached client so the next Get reconnects.
func (r *Registry) Drop(id string) {
	r.mu.Lock()
	detached := r.detachNodeLocked(id)
	r.mu.Unlock()
	detached.close()
}

// Ping checks reachability with a short timeout, reconnecting once on failure.
func (r *Registry) Ping(ctx context.Context, id string) error {
	c, err := r.Get(id)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	// NOTE: assign (not :=) so the retry result is returned, not a stale nil.
	_, err = c.Ping(ctx)
	if err != nil {
		r.Drop(id)
		c, err2 := r.Get(id)
		if err2 != nil {
			return err
		}
		_, err = c.Ping(ctx)
	}
	return err
}

// ProbeSSH performs a real SSH connect + tunnel to the remote Docker socket and
// returns the underlying error directly (the Docker client otherwise masks it
// behind a generic "cannot connect" message via the sentinel host). It records
// the host's key fingerprint for display but deliberately does NOT pin it — a
// Test Connection is not a commitment (F1); pinning happens on a managed node's
// first real connect.
func ProbeSSH(ctx context.Context, address string, secret []byte) (fingerprint string, err error) {
	creds := ParseSSHCreds(secret)
	var fp string
	record := func(_ string, _ net.Addr, pub ssh.PublicKey) error {
		fp = ssh.FingerprintSHA256(pub)
		return nil
	}
	tunnel, err := newSSHTunnel(address, creds, record)
	if err != nil {
		return "", err
	}
	defer tunnel.close()
	conn, err := tunnel.dial(ctx)
	if err != nil {
		// The host-key callback fires during the handshake, BEFORE authentication,
		// so fp is captured even when auth then fails — return it so the UI can show
		// the host key for the user to verify regardless of a wrong key/password.
		return fp, err
	}
	conn.Close()
	return fp, nil
}

// PinningHostKeyCallback implements trust-on-first-use SSH host-key pinning (F1).
// If a key is stored (load returns it), the presented key must match exactly
// (compared as marshaled authorized-key bytes) or the connection is refused with
// an actionable error naming both fingerprints. If none is stored, the presented
// key is saved (save) and accepted. load/save may be nil (an un-wired registry
// can't pin — it accepts first-use but never persists).
func PinningHostKeyCallback(load func() ([]byte, bool), save func(pub ssh.PublicKey)) ssh.HostKeyCallback {
	return func(hostname string, _ net.Addr, key ssh.PublicKey) error {
		presented := ssh.MarshalAuthorizedKey(key)
		var stored []byte
		var have bool
		if load != nil {
			stored, have = load()
		}
		if !have {
			if save != nil {
				save(key)
			}
			return nil
		}
		if bytes.Equal(bytes.TrimSpace(stored), bytes.TrimSpace(presented)) {
			return nil
		}
		oldFP := "SHA256:unknown"
		if pk, _, _, _, perr := ssh.ParseAuthorizedKey(stored); perr == nil {
			oldFP = ssh.FingerprintSHA256(pk)
		}
		return &HostKeyChangedError{Host: hostname, OldFP: oldFP, NewFP: ssh.FingerprintSHA256(key)}
	}
}

// ErrHostKeyChanged is the sentinel callers match with errors.Is to distinguish
// a REFUSED pin mismatch (possible reinstall or man-in-the-middle, F67) from an
// ordinary connection failure.
var ErrHostKeyChanged = errors.New("ssh host key changed")

// HostKeyChangedError is the typed pin-mismatch refusal: it unwraps to
// ErrHostKeyChanged and carries both fingerprints, so the alert layer can dedup
// per key-pair and name exactly what changed.
type HostKeyChangedError struct {
	Host  string
	OldFP string
	NewFP string
}

func (e *HostKeyChangedError) Error() string {
	return fmt.Sprintf("ssh host key changed for %s — refusing to connect (was %s, now %s). If you re-installed this host, reset its pinned key in the node settings.",
		e.Host, e.OldFP, e.NewFP)
}

func (e *HostKeyChangedError) Unwrap() error { return ErrHostKeyChanged }

// pinningVerifier builds the pinning callback for one node, bound to the
// registry's store-backed load/save closures (F1).
func (r *Registry) pinningVerifier(nodeID string) ssh.HostKeyCallback {
	load := func() ([]byte, bool) {
		if r.LoadHostKey == nil {
			return nil, false
		}
		return r.LoadHostKey(nodeID)
	}
	save := func(pub ssh.PublicKey) {
		if r.SaveHostKey != nil {
			r.SaveHostKey(nodeID, ssh.MarshalAuthorizedKey(pub))
		}
	}
	return PinningHostKeyCallback(load, save)
}

// build constructs a *client.Client for the given transport (PLAN §2.14). For
// the SSH transport it also returns the persistent tunnel so the registry can
// close it when the node is dropped, and pins the host key on first connect (F1).
func (r *Registry) build(spec NodeConn) (*client.Client, *sshTunnel, error) {
	switch spec.Transport {
	case TransportLocalProxy, TransportTCPProxy:
		c, err := client.NewClientWithOpts(
			client.WithHost(spec.Address),
			// Bounded dial + TCP keep-alive on the pooled client so a dead/slow
			// node fails fast and a silently-dropped peer is noticed (PLAN §4.13).
			client.WithDialContext((&net.Dialer{Timeout: dialTimeout, KeepAlive: tcpKeepAlive}).DialContext),
			client.WithAPIVersionNegotiation(),
		)
		return c, nil, err

	case TransportSSH:
		creds := ParseSSHCreds(spec.Secret)
		tunnel, err := newSSHTunnel(spec.Address, creds, r.pinningVerifier(spec.ID))
		if err != nil {
			return nil, nil, err
		}
		c, err := client.NewClientWithOpts(
			client.WithHost("tcp://ssh-tunnel.invalid:2375"),
			client.WithDialContext(func(ctx context.Context, _, _ string) (net.Conn, error) {
				return tunnel.dial(ctx)
			}),
			client.WithAPIVersionNegotiation(),
		)
		if err != nil {
			tunnel.close()
			return nil, nil, err
		}
		return c, tunnel, nil

	case TransportMTLS:
		tlsCfg, err := mtlsConfig(spec.Secret)
		if err != nil {
			return nil, nil, err
		}
		// Pooled, keep-alive transport with a bounded dial + TLS handshake so a
		// dead/slow node can't hang a request and connections are reused (§4.13).
		hc := &http.Client{Transport: &http.Transport{
			TLSClientConfig:     tlsCfg,
			DialContext:         (&net.Dialer{Timeout: dialTimeout, KeepAlive: tcpKeepAlive}).DialContext,
			MaxIdleConns:        16,
			MaxIdleConnsPerHost: 16,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: dialTimeout,
			ForceAttemptHTTP2:   true,
		}}
		c, err := client.NewClientWithOpts(
			client.WithHost(spec.Address),
			client.WithHTTPClient(hc),
			client.WithScheme("https"),
			client.WithAPIVersionNegotiation(),
		)
		return c, nil, err

	default:
		return nil, nil, fmt.Errorf("unknown transport %q", spec.Transport)
	}
}

// SSHCreds is a decoded SSH credential: a private key (+ its passphrase) and/or a
// password. Key auth is preferred; password auth is the fallback for hosts that
// don't use keys. Both may be present (tried key-first).
type SSHCreds struct {
	Key        []byte
	Passphrase string
	Password   string
}

// ParseSSHCreds decodes a stored SSH credential blob (JSON {"key","passphrase",
// "password"}). A bare PEM (legacy) is treated as a key with no passphrase.
func ParseSSHCreds(b []byte) SSHCreds {
	var s struct {
		Key        string `json:"key"`
		Passphrase string `json:"passphrase"`
		Password   string `json:"password"`
	}
	// A valid JSON object is our own format (a raw PEM is never valid JSON), so trust
	// it even when empty — that's an unset credential, not a raw key of "{}".
	if json.Unmarshal(b, &s) == nil {
		return SSHCreds{Key: []byte(s.Key), Passphrase: s.Passphrase, Password: s.Password}
	}
	return SSHCreds{Key: b} // legacy: raw PEM, no passphrase
}

// ParseSSHSecret decodes a stored SSH credential blob into the private key and its
// passphrase (kept for callers that only need key material).
func ParseSSHSecret(b []byte) (key []byte, passphrase string) {
	c := ParseSSHCreds(b)
	return c.Key, c.Passphrase
}

// sshTunnel maintains a single persistent SSH client per node and multiplexes
// Docker connections as lightweight channels over it (golang.org/x/crypto/ssh,
// so it works from a distroless image with no ssh binary — PLAN §2.14 transport
// ③). The expensive TCP+handshake+auth happens once; each subsequent Docker
// request just opens a new channel, and a dead connection is transparently
// re-established. This is what keeps multi-container pages from timing out on
// slow links — the old per-request full SSH dial does not scale. The host key is
// verified/pinned via the cfg.HostKeyCallback supplied at construction (F1).
type sshTunnel struct {
	cfg              *ssh.ClientConfig
	hostport         string
	remoteSocket     string
	handshakeTimeout time.Duration
	channelTimeout   time.Duration

	// closed ends every dial in progress when the tunnel is closed.
	closed context.Context
	cancel context.CancelFunc
	// connecting is a slot for the one connect in progress. A channel rather than a
	// mutex, so a request waiting behind a stalled connect can still give up.
	connecting chan struct{}

	mu     sync.Mutex // guards client only: never held across network I/O
	client *ssh.Client
}

// newSSHTunnel parses an ssh:// address + credentials into a tunnel (no connection
// is made until the first dial). It authenticates with a private key when one is
// supplied, a password when one is supplied (with a keyboard-interactive fallback
// so PAM/interactive sshd setups also work), or both (tried key-first). verifier
// checks/pins the remote host key on connect (F1); it must be non-nil.
func newSSHTunnel(address string, creds SSHCreds, verifier ssh.HostKeyCallback) (*sshTunnel, error) {
	u, err := url.Parse(address)
	if err != nil {
		return nil, fmt.Errorf("parsing ssh address: %w", err)
	}
	if u.Scheme != "ssh" {
		return nil, fmt.Errorf("ssh transport requires ssh:// address, got %q", address)
	}
	user := "root"
	if u.User != nil {
		user = u.User.Username()
	}
	hostport := u.Host
	if u.Port() == "" {
		hostport = net.JoinHostPort(u.Hostname(), "22")
	}

	var auth []ssh.AuthMethod
	if len(creds.Key) > 0 {
		var signer ssh.Signer
		if creds.Passphrase != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase(creds.Key, []byte(creds.Passphrase))
		} else {
			signer, err = ssh.ParsePrivateKey(creds.Key)
			if _, missing := err.(*ssh.PassphraseMissingError); missing {
				return nil, fmt.Errorf("this SSH key is passphrase-protected; enter its passphrase")
			}
		}
		if err != nil {
			return nil, fmt.Errorf("parsing ssh private key: %w", err)
		}
		auth = append(auth, ssh.PublicKeys(signer))
	}
	if creds.Password != "" {
		auth = append(auth, ssh.Password(creds.Password))
		// Keyboard-interactive with the same password — many sshd/PAM configs prompt
		// for the password this way rather than accepting plain "password" auth.
		auth = append(auth, ssh.KeyboardInteractive(func(_, _ string, questions []string, _ []bool) ([]string, error) {
			ans := make([]string, len(questions))
			for i := range ans {
				ans[i] = creds.Password
			}
			return ans, nil
		}))
	}
	if len(auth) == 0 {
		return nil, fmt.Errorf("no ssh credentials provided — add a private key or a password")
	}

	remoteSocket := "/var/run/docker.sock"
	if p := u.Path; p != "" && p != "/" {
		remoteSocket = p
	}
	closed, cancel := context.WithCancel(context.Background())
	return &sshTunnel{
		cfg: &ssh.ClientConfig{
			User:            user,
			Auth:            auth,
			HostKeyCallback: verifier, // trust-on-first-use pinning (F1)
			Timeout:         10 * time.Second,
		},
		hostport:         hostport,
		remoteSocket:     remoteSocket,
		handshakeTimeout: sshHandshakeTimeout,
		channelTimeout:   sshChannelTimeout,
		closed:           closed,
		cancel:           cancel,
		connecting:       make(chan struct{}, 1),
	}, nil
}

// ParseHostKey decodes marshaled authorized-key bytes (as produced by
// ssh.MarshalAuthorizedKey) into the pieces the store persists (F1): the key
// type, base64 of the wire-format public key, and the SHA256 fingerprint.
func ParseHostKey(pub []byte) (keyType, keyB64, fingerprint string, ok bool) {
	pk, _, _, _, err := ssh.ParseAuthorizedKey(pub)
	if err != nil {
		return "", "", "", false
	}
	return pk.Type(), base64.StdEncoding.EncodeToString(pk.Marshal()), ssh.FingerprintSHA256(pk), true
}

// AuthorizedKeyBytes reconstructs the marshaled authorized-key line from a stored
// key type + base64, so a pinned key can be compared against a presented one.
func AuthorizedKeyBytes(keyType, keyB64 string) []byte {
	return []byte(keyType + " " + keyB64)
}

// SSHHostPort extracts host:port from an ssh:// address (defaulting to :22), for
// the informational hostport column on a pinned key. Empty on a bad address.
func SSHHostPort(address string) string {
	u, err := url.Parse(address)
	if err != nil || u.Host == "" {
		return ""
	}
	if u.Port() == "" {
		return net.JoinHostPort(u.Hostname(), "22")
	}
	return u.Host
}

// dial opens a new channel to the remote Docker socket, reusing the persistent
// SSH client and (re)connecting it only when needed.
func (t *sshTunnel) dial(ctx context.Context) (net.Conn, error) {
	ctx, cancel := t.untilClosed(ctx)
	defer cancel()
	stale := t.currentClient()
	if stale != nil {
		conn, err := t.openChannel(ctx, stale)
		if err == nil || ctx.Err() != nil {
			return conn, err
		}
		// Channel open failed — the SSH connection is likely dead; reconnect.
	}
	cli, err := t.connectedClient(ctx, stale)
	if err != nil {
		return nil, err
	}
	return t.openChannel(ctx, cli)
}

// sshClient returns the tunnel's live *ssh.Client, connecting it if needed. Used to
// open an ancillary session (F57 host-file read) over the same pinned connection.
func (t *sshTunnel) sshClient(ctx context.Context) (*ssh.Client, error) {
	ctx, cancel := t.untilClosed(ctx)
	defer cancel()
	return t.connectedClient(ctx, nil)
}

// untilClosed is ctx, also ended by the tunnel closing.
func (t *sshTunnel) untilClosed(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(t.closed, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}

func (t *sshTunnel) currentClient() *ssh.Client {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.client
}

// connectedClient returns a client other than stale, connecting one if nobody else
// has. Only one connect runs at a time, and a request waiting for it can leave.
func (t *sshTunnel) connectedClient(ctx context.Context, stale *ssh.Client) (*ssh.Client, error) {
	select {
	case t.connecting <- struct{}{}:
		defer func() { <-t.connecting }()
	case <-ctx.Done():
		return nil, fmt.Errorf("ssh connect %s: %w", t.hostport, ctx.Err())
	}
	// Another goroutine may have reconnected while we waited for the slot.
	if cli := t.currentClient(); cli != nil && cli != stale {
		return cli, nil
	}
	t.discardClient()
	cli, err := t.connect(ctx)
	if err != nil {
		return nil, err
	}
	return cli, t.storeClient(cli)
}

// connect runs the TCP and SSH phases. The deadline covers a server that goes silent
// and the AfterFunc covers a caller that gives up, since neither reaches a blocked
// read on its own. On success the deadline is cleared, because the connection is
// long-lived and idles between requests.
func (t *sshTunnel) connect(ctx context.Context) (*ssh.Client, error) {
	dialer := net.Dialer{Timeout: dialTimeout, KeepAlive: tcpKeepAlive}
	tcp, err := dialer.DialContext(ctx, "tcp", t.hostport)
	if err != nil {
		return nil, fmt.Errorf("ssh dial %s: %w", t.hostport, err)
	}
	stop := context.AfterFunc(ctx, func() { tcp.Close() })
	defer stop()
	if err := tcp.SetDeadline(time.Now().Add(t.handshakeTimeout)); err != nil {
		tcp.Close()
		return nil, fmt.Errorf("ssh handshake %s: %w", t.hostport, err)
	}
	sshConn, chans, reqs, err := ssh.NewClientConn(tcp, t.hostport, t.cfg)
	if err != nil {
		tcp.Close()
		return nil, t.handshakeError(ctx, err)
	}
	if err := tcp.SetDeadline(time.Time{}); err != nil {
		sshConn.Close()
		return nil, fmt.Errorf("ssh handshake %s: %w", t.hostport, err)
	}
	if !stop() {
		sshConn.Close()
		return nil, fmt.Errorf("ssh handshake %s: %w", t.hostport, ctx.Err())
	}
	return ssh.NewClient(sshConn, chans, reqs), nil
}

// handshakeError names what actually ended a handshake. Closing the connection to
// stop it surfaces as a confusing "use of closed network connection".
func (t *sshTunnel) handshakeError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("ssh handshake %s: %w", t.hostport, ctx.Err())
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return fmt.Errorf("ssh handshake %s: no answer within %s: %w", t.hostport, t.handshakeTimeout, err)
	}
	return fmt.Errorf("ssh handshake %s: %w", t.hostport, err)
}

// storeClient publishes a new client, unless the tunnel closed while it connected.
func (t *sshTunnel) storeClient(cli *ssh.Client) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.closed.Err(); err != nil {
		cli.Close()
		return fmt.Errorf("ssh tunnel closed: %w", err)
	}
	t.client = cli
	return nil
}

// discardClient closes the current client, if any.
func (t *sshTunnel) discardClient() {
	t.mu.Lock()
	cli := t.client
	t.client = nil
	t.mu.Unlock()
	if cli != nil {
		cli.Close()
	}
}

// openChannel opens one channel to the Docker socket, bounded.
func (t *sshTunnel) openChannel(ctx context.Context, cli *ssh.Client) (net.Conn, error) {
	conn, err := openBounded(ctx, cli, t.channelTimeout, func() (net.Conn, error) { return cli.Dial("unix", t.remoteSocket) })
	if err != nil {
		return nil, fmt.Errorf("ssh->unix %s: %w", t.remoteSocket, err)
	}
	return conn, nil
}

// openSession opens one exec session, bounded.
func (t *sshTunnel) openSession(ctx context.Context, cli *ssh.Client) (*ssh.Session, error) {
	sess, err := openBounded(ctx, cli, t.channelTimeout, cli.NewSession)
	if err != nil {
		return nil, fmt.Errorf("ssh session %s: %w", t.hostport, err)
	}
	return sess, nil
}

// openBounded runs one channel open on cli. The ssh package takes no context here, so
// it runs aside. A caller that gives up returns at once, while a client that never
// answers is closed when the timeout expires, which also ends the open and tells every
// other user of that connection it is dead. What opens after its caller has gone is
// closed rather than leaked.
func openBounded[T io.Closer](ctx context.Context, cli *ssh.Client, timeout time.Duration, open func() (T, error)) (T, error) {
	type opened struct {
		channel T
		err     error
	}
	var none T
	result := make(chan opened, 1)
	go func() {
		channel, err := open()
		result <- opened{channel, err}
	}()
	timer := time.NewTimer(timeout)
	settle := func() {
		defer timer.Stop()
		select {
		case late := <-result:
			if late.err == nil {
				late.channel.Close()
			}
		case <-timer.C:
			cli.Close()
		}
	}
	select {
	case got := <-result:
		timer.Stop()
		return got.channel, got.err
	case <-timer.C:
		cli.Close()
		return none, fmt.Errorf("no answer within %s", timeout)
	case <-ctx.Done():
		go settle()
		return none, ctx.Err()
	}
}

// close tears down the persistent SSH client and ends any connect in progress. It
// does not wait for that connect, so it is safe with a server that has gone silent.
func (t *sshTunnel) close() {
	t.cancel()
	t.discardClient()
}

// mtlsConfig builds a tls.Config from a PEM bundle (CA + client cert + key).
func mtlsConfig(bundle []byte) (*tls.Config, error) {
	ca, cert, key, err := splitMTLS(bundle)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("mtls: invalid CA PEM")
	}
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		return nil, fmt.Errorf("mtls: invalid client cert/key: %w", err)
	}
	return &tls.Config{
		RootCAs:      pool,
		Certificates: []tls.Certificate{pair},
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// splitMTLS parses a bundle of "CA\n---\nCERT\n---\nKEY".
func splitMTLS(bundle []byte) (ca, cert, key []byte, err error) {
	parts := strings.SplitN(string(bundle), "\n---\n", 3)
	if len(parts) != 3 {
		return nil, nil, nil, errors.New("mtls bundle must be CA---CERT---KEY separated by \\n---\\n")
	}
	return []byte(parts[0]), []byte(parts[1]), []byte(parts[2]), nil
}
