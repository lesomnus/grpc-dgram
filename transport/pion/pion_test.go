package pion_test

// Real pion stack end-to-end: generated gRPC stubs over in-process WebRTC
// DataChannels, ICE on loopback host candidates (no STUN). The reliable
// tests pass no mode or timing options anywhere — that the calls behave like
// plain gRPC with every timer off is the point: the client discovers the
// mode from the channel configuration (drpc.TransportInfo), and the server
// runs each peer in its channel's mode via ServePeer's annotation
// (drpc.NewReliableContext).

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
	svc      *echo.EchoServer
	clientDC *webrtc.DataChannel
}

// serveReliable is the roadmap finale wiring: gRPC over a reliable
// DataChannel, mode auto-detected on both sides — the client from its
// channel config, the server from ServePeer's per-peer annotation.
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
	svc := &echo.EchoServer{}
	echo.RegisterEchoServiceServer(srv, svc)
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
		svc:      svc,
		clientDC: <-dcc,
	}
}

// serveUnreliable wires an unordered, zero-retransmit channel: the client is
// the offerer, the server the answerer, and the peer runs unreliable via
// ServePeer's annotation.
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

// The wiring this library exists for: ONE PeerConnection carrying a
// reliable control channel and an unreliable telemetry channel, ONE
// answerer-side Server serving both — each peer in its own channel's mode
// (drpc.NewReliableContext, PROTOCOL.md §4.3), no mode options anywhere.
// The reliable channel's idle stream surviving past T_live is the proof the
// server did not run it under the unreliable liveness machinery.
func TestMixedChannels(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())

	// An aggressive T_live makes the idle-survival window cheap to cross.
	fast := drpc.Timing{
		Call:       2 * time.Second,
		Liveness:   900 * time.Millisecond,
		Retransmit: 100 * time.Millisecond,
	}
	gw := pion.NewGateway()
	srv := drpc.NewServer(gw, drpc.WithTiming(fast))
	echo.RegisterEchoServiceServer(srv, &echo.EchoServer{})

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

	answerer.OnDataChannel(func(dc *webrtc.DataChannel) {
		gw.Bind(dc)
		go gw.ServePeer(ctx, srv, dc)
	})

	// Two channels, one connection: reliable control, unreliable telemetry.
	ctrlDC, err := offerer.CreateDataChannel("control", nil)
	if err != nil {
		t.Fatal(err)
	}
	ordered := false
	retx := uint16(0)
	teleDC, err := offerer.CreateDataChannel("telemetry", &webrtc.DataChannelInit{
		Ordered:        &ordered,
		MaxRetransmits: &retx,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctrlTp, teleTp := pion.New(ctrlDC), pion.New(teleDC)

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

	ctrl := drpc.NewConn(ctrlTp) // reliable: auto-detected, no options
	tele := drpc.NewConn(teleTp, drpc.WithTiming(fast))
	t.Cleanup(func() {
		srv.Stop()
		ctrl.Close(nil)
		tele.Close(nil)
		cancel()
	})

	// A long-lived control stream and telemetry traffic, side by side.
	stream, err := conn2client(ctrl).Live(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(echo.EchoRequest_builder{Message: "up", Repeat: 1}.Build()); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatal(err)
	}
	res, err := conn2client(tele).Once(t.Context(), echo.EchoRequest_builder{
		Message:       "abc",
		CircularShift: 1,
	}.Build())
	if err != nil {
		t.Fatal(err)
	}
	if got := res.GetMessage(); got != "bca" {
		t.Fatalf("telemetry got %q, want %q", got, "bca")
	}

	// The control stream goes idle well past the server's T_live. A server
	// running this peer in unreliable mode would expire it (a reliable-mode
	// client sends no keepalive); per-peer mode must leave it untouched.
	time.Sleep(3 * fast.Liveness)
	if err := stream.Send(echo.EchoRequest_builder{Message: "still", Repeat: 1}.Build()); err != nil {
		t.Fatal(err)
	}
	got, err := stream.Recv()
	if err != nil {
		t.Fatalf("idle control stream was killed: %v", err)
	}
	if got.GetMessage() != "still" {
		t.Fatalf("got %q, want %q", got.GetMessage(), "still")
	}
}

func conn2client(conn *drpc.Conn) echo.EchoServiceClient {
	return echo.NewEchoServiceClient(conn)
}

// A channel whose association never establishes (a failed dial) fires no
// pion callbacks at all — dc.Close included. Teardown and send bounds must
// not depend on them: the send-stall budget bounds a call on the un-opened
// channel, and Close trips the death latch itself.
func TestNeverOpenedChannel(t *testing.T) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	dc, err := pc.CreateDataChannel("drpc", nil) // never signaled: never opens
	if err != nil {
		t.Fatal(err)
	}
	conn := drpc.NewConn(pion.New(dc, pion.WithSendStallTimeout(200*time.Millisecond)))

	// Reliable config, deadline-less call: only the stall budget bounds it —
	// including the abort path, which transmits with an unbounded ctx.
	start := time.Now()
	if _, err := conn2client(conn).Once(t.Context(), echo.EchoRequest_builder{
		Message: "x",
	}.Build()); err == nil {
		t.Fatal("a call on a never-opened channel cannot succeed")
	}
	if e := time.Since(start); e > 2*time.Second {
		t.Fatalf("call returned in %v, want the stall bound (~200ms)", e)
	}

	// Close must not block on callbacks that will never come...
	done := make(chan struct{})
	go func() {
		conn.Close(nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked on a never-opened channel")
	}
	// ...and the conn is latched closed.
	_, err = conn2client(conn).Once(t.Context(), echo.EchoRequest_builder{}.Build())
	if got := status.Code(err); got != codes.Unavailable {
		t.Fatalf("call on a closed conn: got %v (%v), want Unavailable", got, err)
	}
}

func TestReliableEcho(t *testing.T) {
	e := serveReliable(t)

	t.Run("mode is auto-detected", func(t *testing.T) {
		if !e.tp.Reliable() {
			t.Error("transport did not derive reliable from the channel config")
		}
		// Server side: the mode travels per peer via the ServePeer
		// annotation; the idle-survival assertion in TestMixedChannels is
		// the behavioral proof.
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

// The serve loop blocked in reliable-mode backpressure (PROTOCOL.md §4.2)
// must still be torn down when the channel dies: a blocked loop cannot
// observe ch.dead between deliveries, so channel death (OnClose/OnError,
// send stall, Transport.Close) cancels the delivery ctx the blocked Handle
// waits on (§4.5). Before that link existed, this scenario wedged ServePeer,
// the handler, and the peer state permanently.
func TestBlockedDeliveryDeath(t *testing.T) {
	e := serveReliable(t)
	hit := make(chan struct{})
	e.svc.SetHit(func() { close(hit) })

	// The handler parks in a pure ctx-wait: nothing ever drains this call's
	// rx buffer again.
	stream, err := e.client.Live(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(echo.EchoRequest_builder{OverVoid: true}.Build()); err != nil {
		t.Fatal(err)
	}
	<-hit

	// Flood past the rx buffer (default 32): the server's serve loop is now
	// blocked inside Handle delivering the overflow frame.
	for range 40 {
		if err := stream.Send(echo.EchoRequest_builder{Message: "x"}.Build()); err != nil {
			t.Fatal(err)
		}
	}

	// The client vanishes. Only the OnClose→dead→delivery-ctx link can reach
	// the blocked serve loop.
	if err := e.clientDC.Close(); err != nil {
		t.Fatal(err)
	}

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		e.srv.GracefulStop()
	}()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("blocked delivery wedged: channel death was never delivered (§4.5)")
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
