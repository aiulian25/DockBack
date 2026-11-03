package dockercli

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// promptly is how long something that must not wait on a silent server may take. It
// is far below every timeout under test, so passing cannot come from one expiring.
const (
	promptly        = 2 * time.Second
	neverFires      = time.Hour
	shortTimeout    = 150 * time.Millisecond
	fakeSSHPassword = "fixture-password"
	fakeSSHBanner   = "SSH-2.0-fake\r\n"
)

// serverBehaviour is where the fake server stops answering.
type serverBehaviour int

const (
	silentAtBanner serverBehaviour = iota
	silentAfterBanner
	silentAtChannel
	healthy
)

// fakeSSHServer speaks real SSH on a local port, up to the point it is told to stall.
type fakeSSHServer struct {
	t         *testing.T
	listener  net.Listener
	behaviour serverBehaviour
	signer    ssh.Signer
	accepted  atomic.Int32
	reached   chan struct{}
	release   chan struct{}
	wg        sync.WaitGroup
	mu        sync.Mutex
	conns     []net.Conn
}

func newFakeSSHServer(t *testing.T, behaviour serverBehaviour) *fakeSSHServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	server := &fakeSSHServer{t: t, listener: listener, behaviour: behaviour, signer: signer, reached: make(chan struct{}, 16), release: make(chan struct{})}
	server.wg.Add(1)
	go server.serve()
	t.Cleanup(server.stop)
	return server
}

func (f *fakeSSHServer) address() string { return "ssh://root@" + f.listener.Addr().String() }

func (f *fakeSSHServer) stop() {
	f.listener.Close()
	f.dropConnections()
	select {
	case <-f.release:
	default:
		close(f.release)
	}
	f.wg.Wait()
}

func (f *fakeSSHServer) dropConnections() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, conn := range f.conns {
		conn.Close()
	}
	f.conns = nil
}

func (f *fakeSSHServer) serve() {
	defer f.wg.Done()
	for {
		conn, err := f.listener.Accept()
		if err != nil {
			return
		}
		f.accepted.Add(1)
		f.mu.Lock()
		f.conns = append(f.conns, conn)
		f.mu.Unlock()
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			f.handle(conn)
		}()
	}
}

func (f *fakeSSHServer) stall() {
	f.reached <- struct{}{}
	<-f.release
}

func (f *fakeSSHServer) handle(conn net.Conn) {
	defer conn.Close()
	switch f.behaviour {
	case silentAtBanner:
		f.stall()
		return
	case silentAfterBanner:
		_, _ = conn.Write([]byte(fakeSSHBanner))
		f.stall()
		return
	}
	config := &ssh.ServerConfig{PasswordCallback: func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) { return nil, nil }}
	config.AddHostKey(f.signer)
	serverConn, channels, requests, err := ssh.NewServerConn(conn, config)
	if err != nil {
		return
	}
	defer serverConn.Close()
	go ssh.DiscardRequests(requests)
	for newChannel := range channels {
		if f.behaviour == silentAtChannel {
			f.reached <- struct{}{}
			continue
		}
		channel, channelRequests, err := newChannel.Accept()
		if err != nil {
			return
		}
		go ssh.DiscardRequests(channelRequests)
		go func() {
			defer channel.Close()
			_, _ = io.Copy(channel, channel)
		}()
	}
}

func (f *fakeSSHServer) awaitStall() {
	f.t.Helper()
	select {
	case <-f.reached:
	case <-time.After(promptly):
		f.t.Fatal("the server never reached its stall")
	}
}

func fakeTunnel(t *testing.T, server *fakeSSHServer, verifier ssh.HostKeyCallback) *sshTunnel {
	t.Helper()
	if verifier == nil {
		verifier = ssh.InsecureIgnoreHostKey()
	}
	tunnel, err := newSSHTunnel(server.address(), SSHCreds{Password: fakeSSHPassword}, verifier)
	if err != nil {
		t.Fatal(err)
	}
	tunnel.handshakeTimeout = neverFires
	tunnel.channelTimeout = neverFires
	t.Cleanup(tunnel.close)
	return tunnel
}

// dialInBackground runs a dial and returns its result channel.
func dialInBackground(ctx context.Context, tunnel *sshTunnel) <-chan error {
	done := make(chan error, 1)
	go func() {
		conn, err := tunnel.dial(ctx)
		if conn != nil {
			conn.Close()
		}
		done <- err
	}()
	return done
}

func awaitResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(promptly):
		t.Fatal("still blocked on a server that will never answer")
		return nil
	}
}

var stallPhases = map[string]serverBehaviour{
	"banner":    silentAtBanner,
	"handshake": silentAfterBanner,
	"channel":   silentAtChannel,
}

func TestCancellingADialReturnsPromptlyAtEveryPhase(t *testing.T) {
	for name, behaviour := range stallPhases {
		t.Run(name, func(t *testing.T) {
			server := newFakeSSHServer(t, behaviour)
			tunnel := fakeTunnel(t, server, nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := dialInBackground(ctx, tunnel)
			server.awaitStall()
			cancel()
			if err := awaitResult(t, done); !errors.Is(err, context.Canceled) {
				t.Fatalf("want the cancellation named, got %v", err)
			}
		})
	}
}

func TestASilentServerTimesOutAtEveryPhase(t *testing.T) {
	for name, behaviour := range stallPhases {
		t.Run(name, func(t *testing.T) {
			server := newFakeSSHServer(t, behaviour)
			tunnel := fakeTunnel(t, server, nil)
			tunnel.handshakeTimeout = shortTimeout
			tunnel.channelTimeout = shortTimeout
			err := awaitResult(t, dialInBackground(context.Background(), tunnel))
			if err == nil || !strings.Contains(err.Error(), "no answer within") {
				t.Fatalf("want a timeout that says so, got %v", err)
			}
		})
	}
}

// The mutex this replaces also held up every request behind the stalled one.
func TestARequestWaitingBehindAStalledConnectCanLeave(t *testing.T) {
	server := newFakeSSHServer(t, silentAtBanner)
	tunnel := fakeTunnel(t, server, nil)
	dialInBackground(context.Background(), tunnel)
	server.awaitStall()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiting := dialInBackground(ctx, tunnel)
	cancel()
	if err := awaitResult(t, waiting); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if got := server.accepted.Load(); got != 1 {
		t.Fatalf("%d connects: only one may run at a time", got)
	}
}

func TestClosingATunnelEndsAStalledDial(t *testing.T) {
	for name, behaviour := range stallPhases {
		t.Run(name, func(t *testing.T) {
			server := newFakeSSHServer(t, behaviour)
			tunnel := fakeTunnel(t, server, nil)
			done := dialInBackground(context.Background(), tunnel)
			server.awaitStall()

			closed := make(chan error, 1)
			go func() {
				tunnel.close()
				closed <- nil
			}()
			_ = awaitResult(t, closed)
			if err := awaitResult(t, done); err == nil {
				t.Fatal("a dial on a closed tunnel must fail")
			}
			if err := awaitResult(t, dialInBackground(context.Background(), tunnel)); err == nil {
				t.Fatal("a closed tunnel must not connect again")
			}
		})
	}
}

// The finding's whole point: one node's silent server must not touch the others.
func TestAStalledNodeDoesNotBlockTheRegistry(t *testing.T) {
	cleanups := map[string]func(*Registry, string){
		"drop":   func(r *Registry, id string) { r.Drop(id) },
		"remove": func(r *Registry, id string) { r.Remove(id) },
		"set": func(r *Registry, id string) {
			r.Set(NodeConn{ID: id, Transport: TransportTCPProxy, Address: "tcp://127.0.0.1:1"})
		},
	}
	for name, cleanup := range cleanups {
		t.Run(name, func(t *testing.T) {
			server := newFakeSSHServer(t, silentAtBanner)
			registry := NewRegistry()
			registry.Set(NodeConn{ID: "stalled", Transport: TransportSSH, Address: server.address(), Secret: []byte(`{"password":"` + fakeSSHPassword + `"}`)})
			registry.Set(NodeConn{ID: "other", Transport: TransportTCPProxy, Address: "tcp://127.0.0.1:1"})
			stalled, err := registry.Get("stalled")
			if err != nil {
				t.Fatal(err)
			}
			pinged := make(chan error, 1)
			go func() {
				_, perr := stalled.Ping(context.Background())
				pinged <- perr
			}()
			server.awaitStall()

			finished := make(chan error, 1)
			go func() {
				cleanup(registry, "stalled")
				_, gerr := registry.Get("other")
				finished <- gerr
			}()
			if err := awaitResult(t, finished); err != nil {
				t.Fatalf("the unrelated node: %v", err)
			}
			if err := awaitResult(t, pinged); err == nil {
				t.Fatal("the stalled request must fail once its node is gone")
			}
		})
	}
}

func TestAHealthyTunnelReusesItsConnectionAndOutlivesTheDeadline(t *testing.T) {
	server := newFakeSSHServer(t, healthy)
	tunnel := fakeTunnel(t, server, nil)
	tunnel.handshakeTimeout = shortTimeout

	roundTrip := func() {
		t.Helper()
		conn, err := tunnel.dial(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		const payload = "ping"
		if _, err := conn.Write([]byte(payload)); err != nil {
			t.Fatal(err)
		}
		reply := make([]byte, len(payload))
		if _, err := io.ReadFull(conn, reply); err != nil || string(reply) != payload {
			t.Fatalf("reply %q, err %v", reply, err)
		}
	}
	roundTrip()
	// Idle past the handshake's deadline: it applied to the handshake, not the connection.
	time.Sleep(2 * shortTimeout)
	roundTrip()
	if got := server.accepted.Load(); got != 1 {
		t.Fatalf("%d connections, want the one reused", got)
	}
}

func TestADeadConnectionReconnectsWithThePinnedKey(t *testing.T) {
	server := newFakeSSHServer(t, healthy)
	var pinned []byte
	verifier := PinningHostKeyCallback(
		func() ([]byte, bool) { return pinned, pinned != nil },
		func(pub ssh.PublicKey) { pinned = ssh.MarshalAuthorizedKey(pub) },
	)
	tunnel := fakeTunnel(t, server, verifier)
	first, err := tunnel.dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	first.Close()
	server.dropConnections()

	second, err := tunnel.dial(context.Background())
	if err != nil {
		t.Fatalf("a valid pinned host must reconnect: %v", err)
	}
	second.Close()
	if got := server.accepted.Load(); got != 2 {
		t.Fatalf("%d connections", got)
	}
}

func TestAChangedHostKeyIsStillRefused(t *testing.T) {
	server := newFakeSSHServer(t, healthy)
	other := newFakeSSHServer(t, healthy)
	pinned := ssh.MarshalAuthorizedKey(other.signer.PublicKey())
	verifier := PinningHostKeyCallback(func() ([]byte, bool) { return pinned, true }, nil)
	tunnel := fakeTunnel(t, server, verifier)
	if _, err := tunnel.dial(context.Background()); !errors.Is(err, ErrHostKeyChanged) {
		t.Fatalf("got %v", err)
	}
}

func TestAStalledDialLeaksNoGoroutines(t *testing.T) {
	before := runtime.NumGoroutine()
	for _, behaviour := range stallPhases {
		server := newFakeSSHServer(t, behaviour)
		tunnel := fakeTunnel(t, server, nil)
		ctx, cancel := context.WithCancel(context.Background())
		done := dialInBackground(ctx, tunnel)
		server.awaitStall()
		cancel()
		_ = awaitResult(t, done)
		tunnel.close()
		server.stop()
	}
	deadline := time.Now().Add(promptly)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Fatalf("%d goroutines before, %d after", before, after)
	}
}

func TestFetchNodeFileHonoursItsContextOnASilentServer(t *testing.T) {
	for name, behaviour := range stallPhases {
		t.Run(name, func(t *testing.T) {
			server := newFakeSSHServer(t, behaviour)
			registry := NewRegistry()
			registry.Set(NodeConn{ID: "node", Transport: TransportSSH, Address: server.address(), Secret: []byte(`{"password":"` + fakeSSHPassword + `"}`)})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := registry.FetchNodeFile(ctx, "node", "/etc/compose.yml", 1024)
				done <- err
			}()
			server.awaitStall()
			cancel()
			if err := awaitResult(t, done); !errors.Is(err, context.Canceled) {
				t.Fatalf("got %v", err)
			}
		})
	}
}
