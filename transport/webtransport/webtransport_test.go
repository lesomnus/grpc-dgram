package webtransport_test

// Real QUIC stack end-to-end: generated gRPC stubs over WebTransport
// datagrams on loopback, both ends Go (quic-go/webtransport-go), TLS from a
// self-signed certificate generated in-process. Loopback rarely loses
// datagrams, so this exercises the adapter contract — peer routing,
// serialization, the size limit, the teardown duty — while loss behavior
// itself is characterized in the core suite.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	drpc "github.com/lesomnus/grpc-dgram"
	"github.com/lesomnus/grpc-dgram/internal/echo"
	"github.com/lesomnus/grpc-dgram/transport/webtransport"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	wt "github.com/quic-go/webtransport-go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// timing keeps the loopback tests snappy; these are real timers, not
// synctest (a real network stack cannot enter a bubble). The teardown tests
// bound their waits well inside Liveness: the adapter's death report, not
// the timer, must be what fails the calls.
var timing = drpc.Timing{
	Call:       2 * time.Second,
	Liveness:   3 * time.Second,
	Retransmit: 100 * time.Millisecond,
}

// testCert is a self-signed ECDSA P-256 leaf valid for 13 days: the shape a
// browser accepts through serverCertificateHashes (ECDSA, under two weeks),
// and what a Go client trusts through RootCAs with the leaf as its own root.
func testCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(13 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

// listener is one WebTransport server on loopback. Every accepted session is
// announced on sessions and handed to the fixture's onSession — served
// through a Gateway, or kept raw for adapter-level assertions.
type listener struct {
	addr     string
	url      string
	pool     *x509.CertPool
	sessions chan *wt.Session

	s         *wt.Server
	closeOnce sync.Once
	done      chan error
}

func listen(t *testing.T, onSession func(*wt.Session)) *listener {
	t.Helper()
	cert, pool := testCert(t)
	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	l := &listener{
		addr:     pc.LocalAddr().String(),
		url:      fmt.Sprintf("https://%s/rpc", pc.LocalAddr()),
		pool:     pool,
		sessions: make(chan *wt.Session, 8),
		done:     make(chan error, 1),
		s: &wt.Server{
			H3: &http3.Server{
				TLSConfig:  http3.ConfigureTLSConfig(&tls.Config{Certificates: []tls.Certificate{cert}}),
				QUICConfig: &quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true},
				Handler:    mux,
			},
			// A Go client sends no Origin; browsers do, and a dev page
			// rarely shares the server's host.
			CheckOrigin: func(*http.Request) bool { return true },
		},
	}
	mux.HandleFunc("/rpc", func(w http.ResponseWriter, r *http.Request) {
		sess, err := l.s.Upgrade(w, r)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		// The session outlives this handler (it is built on a
		// non-cancelling ctx), so returning at once is fine.
		l.sessions <- sess
		onSession(sess)
	})
	go func() { l.done <- l.s.Serve(pc) }()
	t.Cleanup(func() {
		l.close()
		pc.Close()
	})
	return l
}

// close shuts the server down: every session gets a QUIC CONNECTION_CLOSE
// while the socket is still open. Idempotent for the tests that shut down
// on purpose before cleanup.
func (l *listener) close() {
	l.closeOnce.Do(func() {
		l.s.Close()
		<-l.done
	})
}

// dialSession opens one session; the caller owns it.
func (l *listener) dialSession(t *testing.T) *wt.Session {
	t.Helper()
	// Dial fills in the h3 ALPN and the two QUIC flags a session needs.
	return l.dialWith(t, &wt.Transport{TLSClientConfig: &tls.Config{RootCAs: l.pool}})
}

// dialThrough opens one session over the test's own socket, so the test
// can cut the path from under a live connection; the caller owns the
// session.
func (l *listener) dialThrough(t *testing.T, pc net.PacketConn) *wt.Session {
	t.Helper()
	tr := &quic.Transport{Conn: pc}
	t.Cleanup(func() { tr.Close() })
	return l.dialWith(t, &wt.Transport{
		TLSClientConfig: &tls.Config{RootCAs: l.pool},
		DialAddr: func(ctx context.Context, addr string, tlsCfg *tls.Config, cfg *quic.Config) (*quic.Conn, error) {
			ua, err := net.ResolveUDPAddr("udp", addr)
			if err != nil {
				return nil, err
			}
			return tr.DialEarly(ctx, ua, tlsCfg, cfg)
		},
	})
}

func (l *listener) dialWith(t *testing.T, d *wt.Transport) *wt.Session {
	t.Helper()
	t.Cleanup(func() { d.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, sess, err := d.Dial(ctx, l.url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return sess
}

// blackhole is a PacketConn whose path can be cut under a live connection:
// with drop set, writes vanish and reads discard — the peer behind a closed
// laptop lid, seen from this end. Nothing errors, so the connection notices
// nothing until its idle timeout.
type blackhole struct {
	net.PacketConn
	drop atomic.Bool
}

func (b *blackhole) WriteTo(p []byte, addr net.Addr) (int, error) {
	if b.drop.Load() {
		return len(p), nil
	}
	return b.PacketConn.WriteTo(p, addr)
}

func (b *blackhole) ReadFrom(p []byte) (int, net.Addr, error) {
	for {
		n, addr, err := b.PacketConn.ReadFrom(p)
		if err == nil && b.drop.Load() {
			continue
		}
		return n, addr, err
	}
}

// accepted returns the next server-side session.
func (l *listener) accepted(t *testing.T) *wt.Session {
	t.Helper()
	select {
	case sess := <-l.sessions:
		return sess
	case <-time.After(5 * time.Second):
		t.Fatal("no session accepted")
		return nil
	}
}

// ends is the echo service behind a Gateway, with hooks into both sides.
type ends struct {
	*listener
	gw     *webtransport.Gateway
	srv    *drpc.Server
	svc    *echo.EchoServer
	served chan error // ServePeer results, one per accepted session
}

func serveEcho(t *testing.T, opts ...webtransport.Option) *ends {
	t.Helper()
	gw := webtransport.NewGateway(opts...)
	srv := drpc.NewServer(gw, drpc.WithTiming(timing))
	svc := &echo.EchoServer{}
	echo.RegisterEchoServiceServer(srv, svc)

	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	e := &ends{gw: gw, srv: srv, svc: svc, served: make(chan error, 8)}
	e.listener = listen(t, func(sess *wt.Session) {
		// ServePeer blocks until the session dies; the server-lifetime ctx,
		// not the request's, bounds it.
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.served <- gw.ServePeer(ctx, srv, sess)
		}()
	})
	t.Cleanup(func() {
		srv.Stop()
		cancel()
		wg.Wait()
	})
	return e
}

type client struct {
	echo.EchoServiceClient
	conn *drpc.Conn
	tp   *webtransport.Transport
}

func (e *ends) dial(t *testing.T, opts ...webtransport.Option) *client {
	t.Helper()
	tp := webtransport.New(e.dialSession(t), opts...)
	// drpc.NewConn discovers the transport via ConnAttacher: the receive
	// pump starts by itself, and Close tears the session down too.
	conn := drpc.NewConn(tp, drpc.WithTiming(timing))
	t.Cleanup(func() { conn.Close(nil) })
	return &client{EchoServiceClient: echo.NewEchoServiceClient(conn), conn: conn, tp: tp}
}

// servedWithin returns the next ServePeer result, failing if the gateway
// has not let go of the peer by then — the §4.5 duty is time-bound here.
func (e *ends) servedWithin(t *testing.T, d time.Duration) error {
	t.Helper()
	select {
	case err := <-e.served:
		return err
	case <-time.After(d):
		t.Fatalf("ServePeer still running %v after the session ended", d)
		return nil
	}
}

func TestEcho(t *testing.T) {
	e := serveEcho(t)
	c := e.dial(t)

	t.Run("mode is auto-detected", func(t *testing.T) {
		if c.tp.Reliable() || e.gw.Reliable() {
			t.Fatal("datagrams are unreliable; both endpoints must say so")
		}
	})
	t.Run("unary", func(t *testing.T) {
		res, err := c.Once(t.Context(), echo.EchoRequest_builder{
			Message:       "abc",
			CircularShift: 1,
		}.Build())
		if err != nil {
			t.Fatal(err)
		}
		if got := res.GetMessage(); got != "bca" {
			t.Fatalf("got %q, want %q", got, "bca")
		}
	})
	t.Run("server-streaming to EOF", func(t *testing.T) {
		stream, err := c.Many(t.Context(), echo.EchoRequest_builder{
			Message:       "abc",
			CircularShift: 1,
			Repeat:        3,
		}.Build())
		if err != nil {
			t.Fatal(err)
		}
		got := []string{}
		for {
			res, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, res.GetMessage())
		}
		if want := []string{"bca", "cab", "abc"}; !slices.Equal(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})
	t.Run("client-streaming", func(t *testing.T) {
		stream, err := c.Buff(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, msg := range []string{"a", "b", "c"} {
			if err := stream.Send(echo.EchoRequest_builder{Message: msg, Repeat: 1}.Build()); err != nil {
				t.Fatal(err)
			}
		}
		res, err := stream.CloseAndRecv()
		if err != nil {
			t.Fatal(err)
		}
		got := []string{}
		for _, item := range res.GetItems() {
			got = append(got, item.GetMessage())
		}
		if want := []string{"a", "b", "c"}; !slices.Equal(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})
	t.Run("bidi", func(t *testing.T) {
		stream, err := c.Live(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for range 3 {
			if err := stream.Send(echo.EchoRequest_builder{
				Message: "ping", Repeat: 1,
			}.Build()); err != nil {
				t.Fatal(err)
			}
			if _, err := stream.Recv(); err != nil {
				t.Fatal(err)
			}
		}
		if err := stream.CloseSend(); err != nil {
			t.Fatal(err)
		}
		if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
			t.Fatalf("got %v, want io.EOF", err)
		}
	})
	t.Run("peer is the session's address", func(t *testing.T) {
		// TransportPeer on the client: grpc.Peer sees the server's socket.
		var p peer.Peer
		if _, err := c.Once(t.Context(), echo.EchoRequest_builder{}.Build(), grpc.Peer(&p)); err != nil {
			t.Fatal(err)
		}
		if p.Addr == nil || p.Addr.String() != e.addr {
			t.Fatalf("peer %v, want the server at %s", p.Addr, e.addr)
		}
	})
}

func TestPeerIsolation(t *testing.T) {
	e := serveEcho(t)
	a := e.dial(t)
	b := e.dial(t)

	// Two sessions, two peers: each dial is its own QUIC connection.
	e.accepted(t)
	e.accepted(t)

	sa, err := a.Live(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	sb, err := b.Live(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	for i, s := range []interface {
		Send(*echo.EchoRequest) error
		Recv() (*echo.EchoResponse, error)
	}{sa, sb} {
		msg := []string{"first", "second"}[i]
		if err := s.Send(echo.EchoRequest_builder{Message: msg, Repeat: 1}.Build()); err != nil {
			t.Fatal(err)
		}
		res, err := s.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if got := res.GetMessage(); got != msg {
			t.Fatalf("stream %d got %q, want %q", i, got, msg)
		}
	}
}

// TestTeardownDuty covers PROTOCOL.md §4.5: the session's closure — from
// either side, or from the server shutting down — must fail the live calls
// through the adapter's own death report, carrying the cause, long before
// the core's liveness timer would notice, and must run
// Server.DisconnectPeer so the peer's handlers are released.
func TestTeardownDuty(t *testing.T) {
	// live opens a bidi stream and parks a Recv on it.
	live := func(t *testing.T, c *client) <-chan error {
		t.Helper()
		stream, err := c.Live(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if err := stream.Send(echo.EchoRequest_builder{Message: "ping", Repeat: 1}.Build()); err != nil {
			t.Fatal(err)
		}
		if _, err := stream.Recv(); err != nil {
			t.Fatal(err)
		}
		recv := make(chan error, 1)
		go func() {
			_, err := stream.Recv()
			recv <- err
		}()
		return recv
	}
	// failedWithin asserts the parked Recv ended Unavailable, with the
	// cause the adapter reported, inside d.
	failedWithin := func(t *testing.T, recv <-chan error, d time.Duration, cause string) {
		t.Helper()
		select {
		case err := <-recv:
			if got := status.Code(err); got != codes.Unavailable {
				t.Fatalf("got %v (%v), want Unavailable", got, err)
			}
			if msg := status.Convert(err).Message(); !strings.Contains(msg, cause) {
				t.Fatalf("status %q does not carry the cause %q", msg, cause)
			}
		case <-time.After(d):
			t.Fatalf("Recv still blocked %v after the session ended", d)
		}
	}
	// stoppedWithin asserts GracefulStop returns inside d: the served
	// peer's handlers were released by DisconnectPeer.
	stoppedWithin := func(t *testing.T, srv *drpc.Server, d time.Duration) {
		t.Helper()
		stopped := make(chan struct{})
		go func() {
			defer close(stopped)
			srv.GracefulStop()
		}()
		select {
		case <-stopped:
		case <-time.After(d):
			t.Fatalf("GracefulStop still blocked %v after the session ended", d)
		}
	}

	t.Run("server closes the session", func(t *testing.T) {
		e := serveEcho(t)
		c := e.dial(t)
		ss := e.accepted(t)
		recv := live(t, c)

		// A coded close: the code and message are the cause on both sides.
		ss.CloseWithError(42, "bye")
		failedWithin(t, recv, time.Second, "code 42: bye")
		if err := e.servedWithin(t, time.Second); err == nil || !strings.Contains(err.Error(), "code 42: bye") {
			t.Fatalf("ServePeer returned %v, want the close as the cause", err)
		}
		// The client conn is latched closed, session included: a new call
		// fails at once rather than waiting for a peer that is gone.
		if _, err := c.Once(t.Context(), echo.EchoRequest_builder{}.Build()); status.Code(err) != codes.Unavailable {
			t.Fatalf("call on the dead conn: got %v, want Unavailable", err)
		}
	})
	t.Run("client closes the conn", func(t *testing.T) {
		e := serveEcho(t)
		c := e.dial(t)
		hit := make(chan struct{})
		e.svc.SetHit(func() { close(hit) })

		// The handler parks in a pure ctx-wait: only DisconnectPeer can
		// release it.
		stream, err := c.Live(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if err := stream.Send(echo.EchoRequest_builder{OverVoid: true}.Build()); err != nil {
			t.Fatal(err)
		}
		<-hit

		// conn.Close closes the transport, which says goodbye (code 0):
		// a clean end for the gateway, whose ServePeer returns nil.
		c.conn.Close(nil)
		if err := e.servedWithin(t, time.Second); err != nil {
			t.Fatalf("ServePeer returned %v, want nil for the peer's clean close", err)
		}
		stoppedWithin(t, e.srv, time.Second)
	})
	t.Run("server shuts down", func(t *testing.T) {
		e := serveEcho(t)
		c := e.dial(t)
		e.accepted(t)
		recv := live(t, c)

		// No goodbye, a QUIC CONNECTION_CLOSE: the session dies under the
		// client, and the death reaches the calls with the stack's error.
		e.close()
		failedWithin(t, recv, time.Second, "transport closed: webtransport: session died")
		if err := e.servedWithin(t, time.Second); err == nil {
			t.Fatal("ServePeer returned nil for a session that died under it")
		}
	})
}

// frameOf builds a frame carrying n payload bytes.
func frameOf(n int) *drpc.Frame {
	f := &drpc.Frame{}
	f.SetPayload(make([]byte, n))
	return f
}

// envelopOf builds a single-frame envelop that marshals to exactly n bytes.
func envelopOf(t *testing.T, n int) *drpc.Envelop {
	t.Helper()
	// The framing overhead depends on the payload through its length
	// varints; a few corrections converge.
	payload := n
	for range 4 {
		e := &drpc.Envelop{}
		e.SetFrames([]*drpc.Frame{frameOf(payload)})
		got := proto.Size(e)
		if got == n {
			return e
		}
		payload -= got - n
	}
	t.Fatalf("could not build a %d-byte envelop", n)
	return nil
}

// TestLargeMessage covers PROTOCOL.md §4.4: an envelop that does not fit is
// refused synchronously with ErrMessageTooLarge — by the adapter's limit, or
// by the stack's own ceiling when the limit is raised past it — and nothing
// reaches the wire; the owning call fails ResourceExhausted and the channel
// stays up.
func TestLargeMessage(t *testing.T) {
	l := listen(t, func(*wt.Session) {})
	cs := l.dialSession(t)
	t.Cleanup(func() { cs.CloseWithError(0, "") })
	ss := l.accepted(t)

	nothingArrived := func(t *testing.T) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		defer cancel()
		if b, err := ss.ReceiveDatagram(ctx); err == nil {
			t.Fatalf("a %d-byte datagram arrived; a refused send must send nothing", len(b))
		}
	}

	t.Run("over the adapter limit", func(t *testing.T) {
		tp := webtransport.New(cs)
		err := tp.Handle(t.Context(), frameOf(2*webtransport.DefaultMaxMessageSize))
		if !errors.Is(err, drpc.ErrMessageTooLarge) {
			t.Fatalf("got %v, want ErrMessageTooLarge", err)
		}
		nothingArrived(t)
	})
	t.Run("over the stack's ceiling", func(t *testing.T) {
		// A limit past what the path carries: the stack refuses before
		// queueing, and the adapter reports it as the same size refusal.
		tp := webtransport.New(cs, webtransport.WithMaxMessageSize(1<<16))
		err := tp.Handle(t.Context(), frameOf(8000))
		if !errors.Is(err, drpc.ErrMessageTooLarge) {
			t.Fatalf("got %v, want ErrMessageTooLarge", err)
		}
		nothingArrived(t)
	})
	t.Run("the call fails ResourceExhausted, small still flows", func(t *testing.T) {
		e := serveEcho(t)
		c := e.dial(t)
		_, err := c.Once(t.Context(), echo.EchoRequest_builder{
			Message: strings.Repeat("x", 2*webtransport.DefaultMaxMessageSize),
		}.Build())
		if got := status.Code(err); got != codes.ResourceExhausted {
			t.Fatalf("got %v (%v), want ResourceExhausted", got, err)
		}

		res, err := c.Once(t.Context(), echo.EchoRequest_builder{
			Message:       "abc",
			CircularShift: 1,
		}.Build())
		if err != nil {
			t.Fatal(err)
		}
		if got := res.GetMessage(); got != "bca" {
			t.Fatalf("got %q, want %q", got, "bca")
		}
	})
}

// TestSendBoundedOnDeadPath: the stack's datagram queue blocks once full,
// and on a path that has stopped acknowledging it drains one frame per PTO
// probe, at intervals that double — so a send there parks for seconds, then
// tens of seconds, until the idle timeout. The adapter must give up on the
// ctx it was handed instead: that is how the core's teardown of a vanished
// peer's calls (§4.5) releases a handler parked in Send.
func TestSendBoundedOnDeadPath(t *testing.T) {
	l := listen(t, func(*wt.Session) {})
	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	bh := &blackhole{PacketConn: pc}
	cs := l.dialThrough(t, bh)
	t.Cleanup(func() {
		bh.drop.Store(false) // the goodbyes get through
		cs.CloseWithError(0, "")
	})
	ss := l.accepted(t)
	tp := webtransport.New(cs)

	// A warm path: one datagram across.
	if err := tp.Send(t.Context(), envelopOf(t, 1000)); err != nil {
		t.Fatal(err)
	}
	rctx, rcancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer rcancel()
	if _, err := ss.ReceiveDatagram(rctx); err != nil {
		t.Fatal(err)
	}

	// Then the path dies under the connection while this end keeps sending,
	// as a handler streaming to a vanished browser would. The first sends
	// fill the congestion window and then the queue; every send past that
	// parks — and must give up on its ctx, promptly, rather than wait for
	// the probe that frees a slot.
	bh.drop.Store(true)
	e := envelopOf(t, 1000)
	const budget = 100 * time.Millisecond
	parked := 0
	for i := 0; i < 512 && parked < 4; i++ {
		ctx, cancel := context.WithTimeout(t.Context(), budget)
		start := time.Now()
		err := tp.Send(ctx, e)
		took := time.Since(start)
		cancel()
		if took > 10*budget {
			t.Fatalf("send %d took %v against a %v ctx: the stack's queue bounded it, not the ctx", i, took, budget)
		}
		switch {
		case err == nil:
		case errors.Is(err, context.DeadlineExceeded):
			parked++
		default:
			t.Fatalf("send %d: %v", i, err)
		}
	}
	if parked < 4 {
		t.Fatalf("only %d sends parked on the dead path: the queue never filled", parked)
	}
}

// TestMaxMessageSizeDefault pins the default: the library exposes no
// max-datagram-size getter, so the adapter carries transport/udp's 1200 B —
// and that must sit under the ceiling a session has right after its
// handshake, or the default would refuse what the stack could carry.
func TestMaxMessageSizeDefault(t *testing.T) {
	if webtransport.DefaultMaxMessageSize != 1200 {
		t.Fatalf("DefaultMaxMessageSize = %d, want transport/udp's 1200", webtransport.DefaultMaxMessageSize)
	}
	l := listen(t, func(*wt.Session) {})
	cs := l.dialSession(t)
	t.Cleanup(func() { cs.CloseWithError(0, "") })
	ss := l.accepted(t)
	tp := webtransport.New(cs)

	// Exactly the default: sent, and it arrives whole.
	if err := tp.Send(t.Context(), envelopOf(t, webtransport.DefaultMaxMessageSize)); err != nil {
		t.Fatalf("a %d-byte envelop must go through the default limit: %v", webtransport.DefaultMaxMessageSize, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	b, err := ss.ReceiveDatagram(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != webtransport.DefaultMaxMessageSize {
		t.Fatalf("got a %d-byte datagram, want %d", len(b), webtransport.DefaultMaxMessageSize)
	}
	if err := proto.Unmarshal(b, &drpc.Envelop{}); err != nil {
		t.Fatalf("the datagram is not the envelop: %v", err)
	}

	// One byte more is refused; the option moves the line.
	if err := tp.Send(t.Context(), envelopOf(t, webtransport.DefaultMaxMessageSize+1)); !errors.Is(err, drpc.ErrMessageTooLarge) {
		t.Fatalf("got %v, want ErrMessageTooLarge", err)
	}
	small := webtransport.New(cs, webtransport.WithMaxMessageSize(64))
	if err := small.Send(t.Context(), envelopOf(t, 65)); !errors.Is(err, drpc.ErrMessageTooLarge) {
		t.Fatalf("got %v, want ErrMessageTooLarge under a 64-byte limit", err)
	}
}
