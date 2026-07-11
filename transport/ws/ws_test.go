package ws_test

// Real-socket end-to-end: generated gRPC stubs over loopback WebSockets
// (httptest + gorilla Upgrader). These are real timers and real TCP, not
// synctest — real I/O cannot enter a bubble.
//
// No test passes WithReliable or WithTiming: reliable mode must be
// discovered from the adapter's TransportInfo. TestTransportDeath is what
// proves the discovery — with timers off, only the adapter's §4.5 teardown
// can unblock a call within its 2-second bound (unreliable-mode liveness
// would need T_live = 15s).

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	drpc "github.com/lesomnus/grpc-dgram"
	"github.com/lesomnus/grpc-dgram/transport/ws"
	"github.com/lesomnus/grpc-dgram/internal/echo"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type testServer struct {
	url string
	srv *drpc.Server
}

func serveEcho(t *testing.T, opts ...ws.Option) *testServer {
	t.Helper()

	gw := ws.NewGateway(opts...)
	srv := drpc.NewServer(gw)
	echo.RegisterEchoServiceServer(srv, &echo.EchoServer{})

	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	up := websocket.Upgrader{}
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		wg.Add(1)
		defer wg.Done()
		// The error is not asserted: tests kill sockets on purpose, and
		// teardown cancels ctx while connections are live.
		gw.ServePeer(ctx, srv, c)
	}))
	t.Cleanup(func() {
		srv.Stop()
		cancel()
		wg.Wait() // handlers must return before Close, or it blocks forever
		hs.Close()
	})
	return &testServer{
		url: "ws" + strings.TrimPrefix(hs.URL, "http"),
		srv: srv,
	}
}

type testClient struct {
	echo.EchoServiceClient
	ws *websocket.Conn
}

func (s *testServer) dial(t *testing.T, opts ...ws.Option) *testClient {
	t.Helper()

	c, _, err := websocket.DefaultDialer.Dial(s.url, nil)
	if err != nil {
		t.Fatal(err)
	}
	tp := ws.New(c, opts...)
	conn := drpc.NewConn(tp) // reliable mode is discovered, not passed

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		// The error is not asserted: tests kill sockets on purpose.
		tp.ServeConn(ctx, conn)
	}()
	t.Cleanup(func() {
		cancel()
		c.Close()
		<-done
	})
	return &testClient{
		EchoServiceClient: echo.NewEchoServiceClient(conn),
		ws:                c,
	}
}

func TestEcho(t *testing.T) {
	client := serveEcho(t).dial(t)

	t.Run("unary", func(t *testing.T) {
		res, err := client.Once(t.Context(), echo.EchoRequest_builder{
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
		stream, err := client.Many(t.Context(), echo.EchoRequest_builder{
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
		stream, err := client.Buff(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for range 3 {
			if err := stream.Send(echo.EchoRequest_builder{
				Message:       "abc",
				CircularShift: 1,
				Repeat:        1,
			}.Build()); err != nil {
				t.Fatal(err)
			}
		}
		res, err := stream.CloseAndRecv()
		if err != nil {
			t.Fatal(err)
		}
		if got := len(res.GetItems()); got != 3 {
			t.Fatalf("got %d items, want 3", got)
		}
	})
	t.Run("bidi", func(t *testing.T) {
		stream, err := client.Live(t.Context())
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
}

// A reliable transport is size-agnostic: with the default (unlimited) send
// limit, a message far past any datagram MTU round-trips intact.
func TestLargeMessage(t *testing.T) {
	client := serveEcho(t).dial(t)

	msg := strings.Repeat("x", 256<<10)
	res, err := client.Once(t.Context(), echo.EchoRequest_builder{
		Message: msg,
	}.Build())
	if err != nil {
		t.Fatal(err)
	}
	if got := res.GetMessage(); got != msg {
		t.Fatalf("got %d bytes, want %d intact", len(got), len(msg))
	}
}

// An explicit send limit refuses oversized envelops with
// drpc.ErrMessageTooLarge, which the core maps to ResourceExhausted on the
// owning call — without disturbing the connection.
func TestMaxMessageSize(t *testing.T) {
	client := serveEcho(t).dial(t, ws.WithMaxMessageSize(1024))

	_, err := client.Once(t.Context(), echo.EchoRequest_builder{
		Message: strings.Repeat("x", 2048),
	}.Build())
	if got := status.Code(err); got != codes.ResourceExhausted {
		t.Fatalf("got %v (%v), want ResourceExhausted", got, err)
	}

	res, err := client.Once(t.Context(), echo.EchoRequest_builder{
		Message:       "abc",
		CircularShift: 1,
	}.Build())
	if err != nil {
		t.Fatal(err)
	}
	if got := res.GetMessage(); got != "bca" {
		t.Fatalf("got %q, want %q", got, "bca")
	}
}

// The §4.5 teardown duty, end to end: with protocol timers off, a blocked
// Recv on a died transport can only be unblocked by the adapter calling
// Conn.Close / Server.DisconnectPeer. Both sides must react within a couple
// of seconds of a hard TCP close.
func TestTransportDeath(t *testing.T) {
	server := serveEcho(t)
	client := server.dial(t)

	stream, err := client.Live(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// One round trip pins the call live on the server before the kill.
	if err := stream.Send(echo.EchoRequest_builder{
		Message: "ping", Repeat: 1,
	}.Build()); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatal(err)
	}

	recvErr := make(chan error, 1)
	go func() {
		_, err := stream.Recv()
		recvErr <- err
	}()

	client.ws.UnderlyingConn().Close() // hard TCP close, no close handshake

	select {
	case err := <-recvErr:
		if got := status.Code(err); got != codes.Unavailable {
			t.Fatalf("got %v (%v), want Unavailable", got, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Recv still blocked after transport death")
	}

	// The server must have torn the peer's call down too: GracefulStop
	// waits for in-flight handlers, so a leaked handler would wedge it.
	stopped := make(chan struct{})
	go func() {
		server.srv.GracefulStop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("GracefulStop wedged: handler leaked past transport death")
	}
}

// Two concurrent clients are two peers: each stream sees exactly its own
// answers.
func TestPeerIsolation(t *testing.T) {
	server := serveEcho(t)

	var wg sync.WaitGroup
	for _, msg := range []string{"first", "second"} {
		client := server.dial(t)
		wg.Add(1)
		go func() {
			defer wg.Done()
			stream, err := client.Live(t.Context())
			if err != nil {
				t.Error(err)
				return
			}
			for range 10 {
				if err := stream.Send(echo.EchoRequest_builder{
					Message: msg, Repeat: 1,
				}.Build()); err != nil {
					t.Error(err)
					return
				}
				res, err := stream.Recv()
				if err != nil {
					t.Error(err)
					return
				}
				if got := res.GetMessage(); got != msg {
					t.Errorf("got %q, want %q", got, msg)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// A peer that never reads answers no pings (gorilla replies to pings only
// from within a read), so the gateway must declare it dead by keepalive.
func TestKeepaliveDeath(t *testing.T) {
	gw := ws.NewGateway(ws.WithKeepalive(50*time.Millisecond, 250*time.Millisecond))
	srv := drpc.NewServer(gw)
	echo.RegisterEchoServiceServer(srv, &echo.EchoServer{})
	defer srv.Stop()

	served := make(chan error, 1)
	up := websocket.Upgrader{}
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		served <- gw.ServePeer(r.Context(), srv, c)
	}))
	defer hs.Close()

	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(hs.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	select {
	case err := <-served:
		var ne net.Error
		if !errors.As(err, &ne) || !ne.Timeout() {
			t.Fatalf("got %v, want a timeout error", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("gateway did not detect the dead peer")
	}
}
