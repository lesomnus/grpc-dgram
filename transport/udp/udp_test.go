package udp_test

// Real-socket end-to-end: generated gRPC stubs over loopback UDP. Loopback
// rarely loses datagrams, so this exercises the adapter contract — peer
// routing, serialization, the size limit — while loss behavior itself is
// characterized in the core suite.

import (
	"context"
	"errors"
	"io"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	drpc "github.com/lesomnus/grpc-dgram"
	"github.com/lesomnus/grpc-dgram/transport/udp"
	"github.com/lesomnus/grpc-dgram/internal/echo"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// timing keeps the loopback tests snappy; these are real timers, not
// synctest (real sockets cannot enter a bubble).
var timing = drpc.Timing{
	Call:       2 * time.Second,
	Liveness:   3 * time.Second,
	Retransmit: 100 * time.Millisecond,
}

func serveEcho(t *testing.T, opts ...udp.Option) serverAddr {
	t.Helper()

	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	gw := udp.NewGateway(pc, opts...)
	srv := drpc.NewServer(gw, drpc.WithTiming(timing))
	echo.RegisterEchoServiceServer(srv, &echo.EchoServer{})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := gw.Serve(ctx, srv); err != nil {
			t.Errorf("gateway serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		srv.Stop()
		cancel()
		pc.Close()
		<-done
	})
	return serverAddr{addr: pc.LocalAddr().String()}
}

type serverAddr struct{ addr string }

func (a serverAddr) dial(t *testing.T, opts ...udp.Option) echo.EchoServiceClient {
	t.Helper()

	c, err := net.Dial("udp", a.addr)
	if err != nil {
		t.Fatal(err)
	}
	tp := udp.New(c, opts...)
	conn := drpc.NewConn(tp, drpc.WithTiming(timing))

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := tp.Serve(ctx, conn); err != nil {
			t.Errorf("transport serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		conn.Close(nil)
		cancel()
		c.Close()
		<-done
	})
	return echo.NewEchoServiceClient(conn)
}

func TestEcho(t *testing.T) {
	addr := serveEcho(t)
	client := addr.dial(t)

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
	t.Run("too large fails ResourceExhausted, small still flows", func(t *testing.T) {
		_, err := client.Once(t.Context(), echo.EchoRequest_builder{
			Message: strings.Repeat("x", 2*udp.DefaultMaxMessageSize),
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
	})
}

// A momentarily absent server — the restart the core is designed to ride
// out — surfaces on a connected socket as ICMP unreachable (ECONNREFUSED on
// reads and writes). Neither may kill the transport: the read pump must
// survive to hear the revived server, and a send must count as datagram
// loss, not a call-fatal error.
func TestServerAbsenceIsNotFatal(t *testing.T) {
	first, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	addr := first.LocalAddr().(*net.UDPAddr)
	first.Close() // the port is ours for the duration of the test

	c, err := net.Dial("udp", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	tp := udp.New(c)
	conn := drpc.NewConn(tp, drpc.WithTiming(timing))
	defer conn.Close(nil)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- tp.Serve(ctx, conn) }()
	client := echo.NewEchoServiceClient(conn)

	// No server: the OPEN and its retransmissions draw ICMP refusals on both
	// the write and the blocked read. The call must end by its deadline —
	// not by a transport error — and Serve must still be running.
	dctx, dcancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	_, err = client.Once(dctx, echo.EchoRequest_builder{Message: "abc"}.Build())
	dcancel()
	if got := status.Code(err); got != codes.DeadlineExceeded {
		t.Fatalf("call against an absent server ended %v (%v), want DeadlineExceeded", got, err)
	}
	select {
	case err := <-served:
		t.Fatalf("Serve exited on ICMP unreachable: %v", err)
	default:
	}

	// The server comes back on the same address; the same conn works.
	pc, err := net.ListenUDP("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	gw := udp.NewGateway(pc)
	srv := drpc.NewServer(gw, drpc.WithTiming(timing))
	echo.RegisterEchoServiceServer(srv, &echo.EchoServer{})
	sdone := make(chan struct{})
	go func() {
		defer close(sdone)
		gw.Serve(ctx, srv)
	}()
	defer func() {
		srv.Stop()
		pc.Close()
		<-sdone
	}()

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
	cancel()
	c.Close()
	if err := <-served; err != nil {
		t.Fatalf("Serve: %v", err)
	}
}

func TestPeerIsolation(t *testing.T) {
	addr := serveEcho(t)
	a := addr.dial(t)
	b := addr.dial(t)

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
