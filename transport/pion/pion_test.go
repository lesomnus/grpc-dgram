package pion_test

// Real pion stack end-to-end: generated gRPC stubs over in-process WebRTC
// DataChannels, ICE on loopback host candidates (no STUN). The reliable
// tests pass no mode or timing options anywhere — that the calls behave like
// plain gRPC with every timer off is the point: the mode is discovered from
// the channel configuration through drpc.TransportInfo.

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	drpc "github.com/lesomnus/grpc-dgram"
	"github.com/lesomnus/grpc-dgram/internal/echo"
	"github.com/lesomnus/grpc-dgram/transport/pion"
	"github.com/pion/webrtc/v4"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// timing keeps the unreliable-mode tests snappy; these are real timers, not
// synctest (a real network stack cannot enter a bubble).
var timing = drpc.Timing{
	Call:       2 * time.Second,
	Liveness:   3 * time.Second,
	Retransmit: 100 * time.Millisecond,
}

// dial wires two in-process PeerConnections and negotiates one DataChannel
// labeled "drpc" with the given init. Loopback candidates are enabled so ICE
// connects on hosts whose only interface is lo. local runs on the offerer's
// channel before signaling starts; accept runs synchronously inside the
// answerer's OnDataChannel — pion holds the channel's read loop until that
// callback returns, so handlers registered in either observe every message.
func dial(t *testing.T, init *webrtc.DataChannelInit, local, accept func(*webrtc.DataChannel)) {
	t.Helper()

	se := webrtc.SettingEngine{}
	se.SetIncludeLoopbackCandidate(true)
	api := webrtc.NewAPI(webrtc.WithSettingEngine(se))

	offerer, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { offerer.Close() })
	answerer, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { answerer.Close() })

	answerer.OnDataChannel(accept)
	dc, err := offerer.CreateDataChannel("drpc", init)
	if err != nil {
		t.Fatal(err)
	}
	local(dc)

	offer, err := offerer.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gathered := webrtc.GatheringCompletePromise(offerer)
	if err := offerer.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-gathered
	if err := answerer.SetRemoteDescription(*offerer.LocalDescription()); err != nil {
		t.Fatal(err)
	}
	answer, err := answerer.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gathered = webrtc.GatheringCompletePromise(answerer)
	if err := answerer.SetLocalDescription(answer); err != nil {
		t.Fatal(err)
	}
	<-gathered
	if err := offerer.SetRemoteDescription(*answerer.LocalDescription()); err != nil {
		t.Fatal(err)
	}
}

type ends struct {
	client   echo.EchoServiceClient
	tp       *pion.Transport
	gw       *pion.Gateway
	srv      *drpc.Server
	clientDC *webrtc.DataChannel
}

// serveReliable is the roadmap finale wiring: gRPC over a reliable
// DataChannel, mode auto-detected on both sides. The server sits on the
// offerer so its Bind precedes drpc.NewServer and fixes the gateway mode
// from the channel configuration.
func serveReliable(t *testing.T) *ends {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())

	gw := pion.NewGateway()
	var serverDC *webrtc.DataChannel
	tpc := make(chan *pion.Transport, 1)
	dcc := make(chan *webrtc.DataChannel, 1)
	dial(t, nil,
		func(dc *webrtc.DataChannel) {
			serverDC = dc
			gw.Bind(dc)
		},
		func(dc *webrtc.DataChannel) {
			tpc <- pion.New(dc)
			dcc <- dc
		},
	)

	srv := drpc.NewServer(gw)
	echo.RegisterEchoServiceServer(srv, &echo.EchoServer{})
	sdone := make(chan struct{})
	go func() {
		defer close(sdone)
		gw.ServePeer(ctx, srv, serverDC)
	}()

	var tp *pion.Transport
	select {
	case tp = <-tpc:
	case <-time.After(10 * time.Second):
		t.Fatal("data channel never announced: ICE failed?")
	}
	// drpc.NewConn discovers the transport: mode via TransportInfo, receive
	// pump via ConnAttacher — no goroutine here, one Close in cleanup.
	conn := drpc.NewConn(tp)

	t.Cleanup(func() {
		srv.Stop()
		conn.Close(nil)
		cancel()
		<-sdone
	})
	return &ends{
		client:   echo.NewEchoServiceClient(conn),
		tp:       tp,
		gw:       gw,
		srv:      srv,
		clientDC: <-dcc,
	}
}

// serveUnreliable wires an unordered, zero-retransmit channel: the client is
// the offerer, the server the answerer — the server is built before the
// channel arrives, so the gateway latches unreliable on its own.
func serveUnreliable(t *testing.T) *ends {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())

	gw := pion.NewGateway()
	srv := drpc.NewServer(gw, drpc.WithTiming(timing))
	echo.RegisterEchoServiceServer(srv, &echo.EchoServer{})

	var tp *pion.Transport
	ordered := false
	retx := uint16(0)
	dial(t, &webrtc.DataChannelInit{Ordered: &ordered, MaxRetransmits: &retx},
		func(dc *webrtc.DataChannel) {
			tp = pion.New(dc)
		},
		func(dc *webrtc.DataChannel) {
			gw.Bind(dc)
			go gw.ServePeer(ctx, srv, dc)
		},
	)

	conn := drpc.NewConn(tp, drpc.WithTiming(timing))

	t.Cleanup(func() {
		srv.Stop()
		conn.Close(nil)
		cancel()
	})
	return &ends{client: echo.NewEchoServiceClient(conn), tp: tp, gw: gw, srv: srv}
}

// The common answerer-server flow: the server is built before any channel
// exists, so the gateway latches unreliable — which must still serve a
// default-config (reliable) channel, merely with redundant timers. This is
// the ordering shown in Bind's own OnDataChannel example.
func TestAnswererServerServesReliableChannel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())

	gw := pion.NewGateway()
	srv := drpc.NewServer(gw, drpc.WithTiming(timing)) // latches unreliable
	echo.RegisterEchoServiceServer(srv, &echo.EchoServer{})

	var tp *pion.Transport
	served := make(chan error, 1)
	dial(t, nil, // default config: a reliable channel
		func(dc *webrtc.DataChannel) {
			tp = pion.New(dc)
		},
		func(dc *webrtc.DataChannel) {
			gw.Bind(dc)
			go func() { served <- gw.ServePeer(ctx, srv, dc) }()
		},
	)

	conn := drpc.NewConn(tp, drpc.WithTiming(timing))
	t.Cleanup(func() {
		srv.Stop()
		conn.Close(nil)
		cancel()
	})

	res, err := conn2client(conn).Once(t.Context(), echo.EchoRequest_builder{
		Message:       "abc",
		CircularShift: 1,
	}.Build())
	if err != nil {
		t.Fatal(err)
	}
	if got := res.GetMessage(); got != "bca" {
		t.Fatalf("got %q, want %q", got, "bca")
	}
	select {
	case err := <-served:
		t.Fatalf("ServePeer refused the reliable channel: %v", err)
	default:
	}
}

func conn2client(conn *drpc.Conn) echo.EchoServiceClient {
	return echo.NewEchoServiceClient(conn)
}

func TestReliableEcho(t *testing.T) {
	e := serveReliable(t)

	t.Run("mode is auto-detected", func(t *testing.T) {
		if !e.tp.Reliable() {
			t.Error("transport did not derive reliable from the channel config")
		}
		if !e.gw.Reliable() {
			t.Error("gateway did not derive reliable from the bound channel")
		}
	})
	t.Run("unary", func(t *testing.T) {
		res, err := e.client.Once(t.Context(), echo.EchoRequest_builder{
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
		stream, err := e.client.Many(t.Context(), echo.EchoRequest_builder{
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
		if want := []string{"bca", "cab", "abc"}; len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})
	t.Run("bidi", func(t *testing.T) {
		stream, err := e.client.Live(t.Context())
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

func TestUnreliableEcho(t *testing.T) {
	e := serveUnreliable(t)

	t.Run("mode is auto-detected", func(t *testing.T) {
		if e.tp.Reliable() {
			t.Error("transport took a zero-retransmit channel for reliable")
		}
		if e.gw.Reliable() {
			t.Error("gateway latched reliable with no reliable channel in sight")
		}
	})
	t.Run("unary", func(t *testing.T) {
		res, err := e.client.Once(t.Context(), echo.EchoRequest_builder{
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
		stream, err := e.client.Many(t.Context(), echo.EchoRequest_builder{
			Message:       "abc",
			CircularShift: 1,
			Repeat:        3,
		}.Build())
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for {
			_, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			n++
		}
		if n != 3 {
			t.Fatalf("got %d responses, want 3", n)
		}
	})
	t.Run("too large fails ResourceExhausted, small still flows", func(t *testing.T) {
		_, err := e.client.Once(t.Context(), echo.EchoRequest_builder{
			Message: strings.Repeat("x", 2*pion.DefaultMaxMessageSizeUnreliable),
		}.Build())
		if got := status.Code(err); got != codes.ResourceExhausted {
			t.Fatalf("got %v (%v), want ResourceExhausted", got, err)
		}

		res, err := e.client.Once(t.Context(), echo.EchoRequest_builder{
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

// TestTeardownDuty covers PROTOCOL.md §4.5 on a reliable channel, where the
// adapter's death report is the only unblocking mechanism: no timer will ever
// fail the blocked Recv or release the server handler.
func TestTeardownDuty(t *testing.T) {
	e := serveReliable(t)

	stream, err := e.client.Live(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(echo.EchoRequest_builder{
		Message: "ping", Repeat: 1,
	}.Build()); err != nil {
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
	if err := e.clientDC.Close(); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-recv:
		if got := status.Code(err); got != codes.Unavailable {
			t.Fatalf("got %v (%v), want Unavailable", got, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Recv still blocked 5s after the channel closed")
	}

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		e.srv.GracefulStop()
	}()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("GracefulStop still blocked 5s after the channel closed")
	}
}

func TestLargeMessage(t *testing.T) {
	e := serveReliable(t)

	// Message sized so the marshaled envelop stays under the 16 KiB reliable
	// default while dwarfing any single SCTP packet.
	msg := strings.Repeat("x", 16_000)
	res, err := e.client.Once(t.Context(), echo.EchoRequest_builder{
		Message: msg,
	}.Build())
	if err != nil {
		t.Fatal(err)
	}
	if got := res.GetMessage(); got != msg {
		t.Fatalf("got %d bytes back, want the %d sent intact", len(got), len(msg))
	}
}
