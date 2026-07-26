//go:build js && wasm

package jsport_test

// Real MessageChannel end to end, both ends inside one wasm instance: a
// drpc.Conn on port1 and a drpc.Server behind a jsport.Gateway on port2. The
// two ends share no Go memory — everything crosses as a marshaled Envelop
// through the port — so this is the same wire a browser client drives, with
// the JS half replaced by the shortest possible piece of JS.
//
// No test passes drpc.WithReliable or drpc.WithTiming: reliable mode must be
// discovered from the adapter's TransportInfo, and the teardown tests are what
// prove it — with every protocol timer off, only the adapter's §4.5 duty can
// unblock a blocked Recv or release a parked handler.
//
// Note the shape of the runtime: the Go code and the JS event loop share one
// thread, so anything that waits for a port delivery must yield to JS.
// Ordinary blocking Go calls (a channel receive, Recv, time.After) do exactly
// that; a busy spin would wedge the process instead.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall/js"
	"testing"
	"time"

	drpc "github.com/lesomnus/grpc-dgram"
	"github.com/lesomnus/grpc-dgram/internal/echo"
	"github.com/lesomnus/grpc-dgram/transport/jsport"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

type ends struct {
	client echo.EchoServiceClient
	conn   *drpc.Conn
	tp     *jsport.Transport
	gw     *jsport.Gateway
	srv    *drpc.Server
	svc    *echo.EchoServer

	p1, p2 js.Value   // the client's and the server's end of the channel
	served chan error // what ServePeer returned
	done   chan struct{}
}

// serve wires one MessageChannel: the client transport on port1, the gateway
// on port2. client and server carry each role's options, so a ceiling can be
// imposed on one side only — the wire is symmetric, the limits need not be.
func serve(t *testing.T, client []jsport.Option, server []jsport.Option) *ends {
	t.Helper()
	ch := js.Global().Get("MessageChannel").New()
	return serveOn(t, ch.Get("port1"), ch.Get("port2"), client, server)
}

// serveOn is serve over an arbitrary pair of entangled ports.
func serveOn(t *testing.T, p1, p2 js.Value, client []jsport.Option, server []jsport.Option) *ends {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())

	gw := jsport.NewGateway(server...)
	srv := drpc.NewServer(gw)
	svc := &echo.EchoServer{}
	// Registration precedes serving (PROTOCOL.md §13); Bind precedes it too,
	// so a message posted before ServePeer starts is buffered, not lost.
	echo.RegisterEchoServiceServer(srv, svc)
	gw.Bind(p2)

	served := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		served <- gw.ServePeer(ctx, srv, p2)
	}()

	// drpc.NewConn discovers the transport: reliable mode via TransportInfo and
	// the pump via ConnAttacher — no goroutine here, and Close tears the port
	// down too.
	tp := jsport.New(p1, client...)
	conn := drpc.NewConn(tp)

	t.Cleanup(func() {
		srv.Stop()
		conn.Close(nil)
		gw.Close()
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("ServePeer did not return after the ports were closed")
		}
	})
	return &ends{
		client: echo.NewEchoServiceClient(conn),
		conn:   conn,
		tp:     tp,
		gw:     gw,
		srv:    srv,
		svc:    svc,
		p1:     p1,
		p2:     p2,
		served: served,
		done:   done,
	}
}

func TestEcho(t *testing.T) {
	e := serve(t, nil, nil)

	t.Run("both roles report reliable", func(t *testing.T) {
		// A port neither loses, duplicates nor reorders, and the core needs no
		// mode option to learn it: everything else in this file runs on that
		// discovery (PROTOCOL.md §4.3, §10.6).
		if !e.tp.Reliable() {
			t.Error("the client transport did not report reliable")
		}
		if !e.gw.Reliable() {
			t.Error("the gateway did not report reliable")
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
		// Exact sequence, not a count: reliable mode drops nothing.
		if want := []string{"bca", "cab", "abc"}; !slices.Equal(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})
	t.Run("client-streaming", func(t *testing.T) {
		stream, err := e.client.Buff(t.Context())
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
		got := []string{}
		for _, item := range res.GetItems() {
			got = append(got, item.GetMessage())
		}
		if want := []string{"bca", "bca", "bca"}; !slices.Equal(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})
	t.Run("bidi", func(t *testing.T) {
		stream, err := e.client.Live(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for i := range 3 {
			msg := fmt.Sprintf("ping-%d", i)
			if err := stream.Send(echo.EchoRequest_builder{
				Message: msg, Repeat: 1,
			}.Build()); err != nil {
				t.Fatal(err)
			}
			res, err := stream.Recv()
			if err != nil {
				t.Fatal(err)
			}
			if got := res.GetMessage(); got != msg {
				t.Fatalf("got %q, want %q", got, msg)
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

// A reliable transport is size-agnostic, and structured clone has no protocol
// ceiling: with the default (unlimited) limit a message far past any datagram
// MTU round-trips intact.
func TestLargeMessage(t *testing.T) {
	e := serve(t, nil, nil)

	msg := strings.Repeat("x", 256<<10)
	res, err := e.client.Once(t.Context(), echo.EchoRequest_builder{
		Message: msg,
	}.Build())
	if err != nil {
		t.Fatal(err)
	}
	if got := res.GetMessage(); got != msg {
		t.Fatalf("got %d bytes, want %d intact", len(got), len(msg))
	}
}

// An explicit send limit refuses an oversized envelop with
// drpc.ErrMessageTooLarge, which the core maps to ResourceExhausted on the
// owning call (PROTOCOL.md §4.4). The refusal is synchronous and local — the
// OPEN never reaches the port — so the channel is untouched and the next call
// flows.
func TestMaxMessageSize(t *testing.T) {
	e := serve(t, []jsport.Option{jsport.WithMaxMessageSize(1024)}, nil)

	_, err := e.client.Once(t.Context(), echo.EchoRequest_builder{
		Message: strings.Repeat("x", 2048),
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
}

// PROTOCOL.md §6.4 over a channel that has no addresses at all: the label is
// what both ends report, the client through grpc.Peer(&p) and the server
// through the standard peer.FromContext, which is what makes handler code
// written against gRPC work unchanged here.
func TestPeerIsTheLabel(t *testing.T) {
	seen := make(chan *peer.Peer, 1)
	ch := js.Global().Get("MessageChannel").New()
	p1, p2 := ch.Get("port1"), ch.Get("port2")

	gw := jsport.NewGateway(jsport.WithLabel("page"))
	srv := drpc.NewServer(gw, drpc.UnaryInterceptor(
		func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
			p, _ := peer.FromContext(ctx)
			seen <- p
			return h(ctx, req)
		}))
	echo.RegisterEchoServiceServer(srv, &echo.EchoServer{})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = gw.ServePeer(ctx, srv, p2) }()

	conn := drpc.NewConn(jsport.New(p1, jsport.WithLabel("wasm")))
	t.Cleanup(func() {
		srv.Stop()
		conn.Close(nil)
		cancel()
		<-done
	})

	var p peer.Peer
	if _, err := echo.NewEchoServiceClient(conn).Once(t.Context(),
		echo.EchoRequest_builder{Message: "abc"}.Build(), grpc.Peer(&p)); err != nil {
		t.Fatal(err)
	}
	if got, want := p.Addr.Network(), "js"; got != want {
		t.Errorf("client saw network %q, want %q", got, want)
	}
	if got, want := p.Addr.String(), "wasm"; got != want {
		t.Errorf("client saw peer %q, want %q", got, want)
	}
	sp := <-seen
	if sp == nil {
		t.Fatal("the handler saw no peer: the gateway did not attach one")
	}
	if got, want := sp.Addr.String(), "page"; got != want {
		t.Errorf("handler saw peer %q, want %q", got, want)
	}
}

// The goodbye, client side (PROTOCOL.md §4.5). There is no socket to die, so
// closing the client posts an empty envelop and that is the server's only
// notice: ServePeer must return and DisconnectPeer must run. The parked
// handler is the proof — with timers off nothing else would ever release it,
// and GracefulStop waits for in-flight handlers, so a leaked one wedges it.
func TestGoodbyeFromClient(t *testing.T) {
	e := serve(t, nil, nil)
	hit := make(chan struct{})
	e.svc.SetHit(func() { close(hit) })

	stream, err := e.client.Live(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// The handler parks in a pure ctx-wait: only the teardown can end it.
	if err := stream.Send(echo.EchoRequest_builder{OverVoid: true}.Build()); err != nil {
		t.Fatal(err)
	}
	<-hit

	e.conn.Close(nil) // closes the transport (io.Closer), which says goodbye

	select {
	case err := <-e.served:
		if err != nil {
			t.Fatalf("ServePeer reported %v, want nil for a clean goodbye", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ServePeer did not see the goodbye (§4.5)")
	}

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		e.srv.GracefulStop()
	}()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("GracefulStop wedged: DisconnectPeer never released the handler")
	}
}

// The goodbye on the wire, with nothing else confounding it. A 0-byte message
// is the adapter's close frame (§4.1: an envelop carries 1..n frames, so an
// empty one can only mean this), and it is posted here by hand — the port
// stays open and stays entangled, so no runtime "close" event can stand in for
// the mechanism. This is exactly the byte sequence the TypeScript twin posts,
// and the teardown must travel both ways from it: the served peer dies, and
// the goodbye ServePeer posts on its way out fails the client's live call.
func TestEmptyEnvelopIsGoodbye(t *testing.T) {
	e := serve(t, nil, nil)

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

	recvErr := make(chan error, 1)
	go func() {
		_, err := stream.Recv()
		recvErr <- err
	}()

	e.p1.Call("postMessage", js.Global().Get("Uint8Array").New(0))

	select {
	case err := <-e.served:
		if err != nil {
			t.Fatalf("ServePeer reported %v, want nil for a goodbye", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("an empty envelop was not read as the peer's goodbye (§4.5)")
	}
	select {
	case err := <-recvErr:
		if got := status.Code(err); got != codes.Unavailable {
			t.Fatalf("got %v (%v), want Unavailable", got, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the abandoned peer was never told: Recv still blocked")
	}
}

// The goodbye, server side. A wasm instance that is going away closes its
// gateway; the empty envelop it posts is what fails the client's live call
// with UNAVAILABLE, immediately. Without it the Recv below would block
// forever — reliable mode runs no timer that could ever fail it.
func TestGoodbyeFromServer(t *testing.T) {
	e := serve(t, nil, nil)

	stream, err := e.client.Live(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// One round trip pins the call live on both sides before the close.
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

	e.gw.Close()

	select {
	case err := <-recvErr:
		if got := status.Code(err); got != codes.Unavailable {
			t.Fatalf("got %v (%v), want Unavailable", got, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Recv still blocked after the server said goodbye (§4.5)")
	}
}

// The death nobody gets to announce: the port is closed out from under the
// adapter, so no empty envelop is ever posted. Where the runtime fires "close"
// on the entangled port — node does — that event is the whole death signal,
// and both ends owe the §4.5 teardown from it alone. This is the mechanism
// that covers a wasm instance which panicked and a tab that went away, and it
// is the only one that reports a cause: the peer vanished rather than left.
func TestPeerVanishes(t *testing.T) {
	e := serve(t, nil, nil)

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

	recvErr := make(chan error, 1)
	go func() {
		_, err := stream.Recv()
		recvErr <- err
	}()

	e.p1.Call("close") // not the adapter's Close: nothing is said, the port just goes

	select {
	case err := <-e.served:
		if err == nil {
			t.Fatal("ServePeer reported a clean goodbye for a peer that never said one")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ServePeer did not notice the port die (§4.5)")
	}
	select {
	case err := <-recvErr:
		if got := status.Code(err); got != codes.Unavailable {
			t.Fatalf("got %v (%v), want Unavailable", got, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Recv still blocked after the port died (§4.5)")
	}
}

// The exit path nothing else takes: ServePeer's own ctx is cancelled while a
// call is live. Serving stops, which abandons the port — the peer key is never
// reused, so its calls and state must die with it — and an abandoned peer has
// to be told, or with every timer off its calls hang forever. The goodbye
// ServePeer posts on its way out is what tells it.
func TestServePeerCtxCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	ch := js.Global().Get("MessageChannel").New()
	p1, p2 := ch.Get("port1"), ch.Get("port2")

	gw := jsport.NewGateway()
	srv := drpc.NewServer(gw)
	echo.RegisterEchoServiceServer(srv, &echo.EchoServer{})

	served := make(chan error, 1)
	go func() { served <- gw.ServePeer(ctx, srv, p2) }()

	conn := drpc.NewConn(jsport.New(p1))
	t.Cleanup(func() {
		srv.Stop()
		conn.Close(nil)
		gw.Close()
		cancel()
	})

	stream, err := echo.NewEchoServiceClient(conn).Live(t.Context())
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

	recvErr := make(chan error, 1)
	go func() {
		_, err := stream.Recv()
		recvErr <- err
	}()

	cancel()

	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("ServePeer reported %v, want nil for ctx cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ServePeer did not return when its ctx was cancelled")
	}
	select {
	case err := <-recvErr:
		if got := status.Code(err); got != codes.Unavailable {
			t.Fatalf("got %v (%v), want Unavailable", got, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the abandoned peer was never told the server stopped serving (§4.5)")
	}
}

// Bind is what makes an early message safe, and what it saves is worth being
// exact about: a MessagePort queues what arrives until start(), so it loses
// nothing on its own — but a port wired through onmessage drops every message
// posted before the handler is set, and that is the worker global scope and
// every hand-rolled shim. Hence the duck ports here. A client that opens a
// call the instant it hands the port over is the normal case, so the whole
// call below is posted while nothing is serving yet, and must still be
// answered once ServePeer arrives.
func TestBindBuffersBeforeServe(t *testing.T) {
	p1, p2 := duckPorts(t)

	gw := jsport.NewGateway()
	srv := drpc.NewServer(gw)
	echo.RegisterEchoServiceServer(srv, &echo.EchoServer{})
	gw.Bind(p2)

	conn := drpc.NewConn(jsport.New(p1))
	answered := make(chan error, 1)
	go func() {
		_, err := echo.NewEchoServiceClient(conn).Once(t.Context(),
			echo.EchoRequest_builder{Message: "abc"}.Build())
		answered <- err
	}()

	// Yield to the JS event loop: a blocking Go call is what lets it run, so
	// by the time this returns the OPEN has crossed the port with nobody
	// serving — buffered by Bind's listener or lost for good.
	time.Sleep(100 * time.Millisecond)
	select {
	case err := <-answered:
		t.Fatalf("the call finished before anything served it: %v", err)
	default:
	}

	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- gw.ServePeer(ctx, srv, p2) }()
	t.Cleanup(func() {
		srv.Stop()
		conn.Close(nil)
		gw.Close()
		cancel()
		<-served
	})

	select {
	case err := <-answered:
		if err != nil {
			t.Fatalf("the buffered call failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the message posted before ServePeer was lost")
	}
}

// entryPoint installs the accessor property a host defines before it runs the
// module, and reports what Serve publishes there. This is the readiness
// protocol in full: js.Global().Set reaches JS as Reflect.set(globalThis, ...),
// which triggers the setter, so the assignment itself is what wakes the host —
// there is no ready callback and nothing to poll. The JS below is what
// ts/src/transport/port's startWasmServer installs for the same purpose.
func entryPoint(t *testing.T, name string) <-chan js.Value {
	t.Helper()
	// Resolved through a promise rather than from the setter directly: the
	// setter runs inside Go's own js.Global().Set, and a js.Func invoked while
	// Go is already calling into JS re-enters the runtime. A .then callback
	// runs from the event loop, which is the boundary every other callback
	// here crosses.
	published := make(chan js.Value, 1)
	fn := js.FuncOf(func(_ js.Value, args []js.Value) any {
		published <- args[0]
		return nil
	})
	js.Global().Call("eval", `((name) => new Promise((resolve) => {
		let fn
		Object.defineProperty(globalThis, name, {
			configurable: true,
			get: () => fn,
			set: (v) => { fn = v; resolve(v) },
		})
	}))`).Invoke(name).Call("then", fn)
	t.Cleanup(func() {
		js.Global().Delete(name)
		fn.Release()
	})
	return published
}

// awaitEntryPoint is the host waiting for the server to be able to serve.
func awaitEntryPoint(t *testing.T, published <-chan js.Value) js.Value {
	t.Helper()
	select {
	case fn := <-published:
		return fn
	case <-time.After(2 * time.Second):
		t.Fatal("nothing was published: the readiness signal never arrived")
		return js.Undefined()
	}
}

// handOver calls the entry point the way a host does — from the JS event
// loop — because a js.Func invoked directly from Go re-enters the runtime.
func handOver(entry js.Value, args ...any) {
	js.Global().Call("eval",
		`((fn, args) => queueMicrotask(() => fn(...args)))`).Invoke(entry, args)
}

// Serve is the whole server side of a wasm instance: publish one entry point,
// serve every port the host hands to it. One port is one peer (PROTOCOL.md
// §6.4), so a second handover is a second peer off the same drpc.Server — which
// is the only reason the entry point takes a port at all.
func TestServe(t *testing.T) {
	published := entryPoint(t, jsport.DefaultEntryPoint)

	gw := jsport.NewGateway()
	srv := drpc.NewServer(gw)
	// Registration precedes serving (§13) — which is exactly why publishing may
	// be the readiness signal: nothing is on globalThis until this has run.
	echo.RegisterEchoServiceServer(srv, &echo.EchoServer{})

	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- gw.Serve(ctx, srv) }()
	t.Cleanup(func() {
		srv.Stop()
		cancel()
		select {
		case <-served:
		case <-time.After(2 * time.Second):
			t.Error("Serve did not return after its ctx was cancelled")
		}
	})

	entry := awaitEntryPoint(t, published)

	for i, want := range []string{"bca", "cab"} {
		ch := js.Global().Get("MessageChannel").New()
		handOver(entry, ch.Get("port2"))

		conn := drpc.NewConn(jsport.New(ch.Get("port1")))
		t.Cleanup(func() { conn.Close(nil) })

		res, err := echo.NewEchoServiceClient(conn).Once(t.Context(), echo.EchoRequest_builder{
			Message:       "abc",
			CircularShift: int32(i) + 1,
		}.Build())
		if err != nil {
			t.Fatalf("peer %d: %v", i, err)
		}
		if got := res.GetMessage(); got != want {
			t.Fatalf("peer %d: got %q, want %q", i, got, want)
		}
	}
}

// Serving stops with ctx, and what that owes is everything a wasm instance
// about to leave owes: the entry point comes off globalThis, so a host cannot
// hand a port to a server that is gone, and every served peer gets the goodbye
// and its §4.5 teardown — with timers off, nothing else would ever fail the
// live call below.
func TestServeCtxCancel(t *testing.T) {
	const name = "drpcServeCancelled"
	published := entryPoint(t, name)

	gw := jsport.NewGateway(jsport.WithEntryPoint(name))
	srv := drpc.NewServer(gw)
	echo.RegisterEchoServiceServer(srv, &echo.EchoServer{})

	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- gw.Serve(ctx, srv) }()
	t.Cleanup(func() { srv.Stop(); cancel() })

	entry := awaitEntryPoint(t, published)
	ch := js.Global().Get("MessageChannel").New()
	handOver(entry, ch.Get("port2"))

	conn := drpc.NewConn(jsport.New(ch.Get("port1")))
	stream, err := echo.NewEchoServiceClient(conn).Live(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// One round trip pins the call live on both sides before the cancellation.
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

	cancel()

	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve reported %v, want nil for ctx cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return when its ctx was cancelled")
	}
	if v := js.Global().Get(name); !v.IsUndefined() {
		t.Errorf("globalThis.%s is still %v: a host could hand a port to a server that stopped", name, v)
	}
	select {
	case err := <-recvErr:
		if got := status.Code(err); got != codes.Unavailable {
			t.Fatalf("got %v (%v), want Unavailable", got, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the abandoned peer was never told the server stopped serving (§4.5)")
	}
}

// Two servers, one name. The second must refuse, because the alternative is
// silent: a stolen entry point leaves the first server waiting for ports that
// now go somewhere else, and neither of them can tell.
func TestServeRefusesATakenEntryPoint(t *testing.T) {
	const name = "drpcServeTaken"
	published := entryPoint(t, name)

	gw := jsport.NewGateway(jsport.WithEntryPoint(name))
	srv := drpc.NewServer(gw)
	echo.RegisterEchoServiceServer(srv, &echo.EchoServer{})

	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- gw.Serve(ctx, srv) }()
	t.Cleanup(func() {
		srv.Stop()
		cancel()
		<-served
	})

	entry := awaitEntryPoint(t, published)

	other := jsport.NewGateway(jsport.WithEntryPoint(name))
	refused := make(chan error, 1)
	go func() { refused <- other.Serve(t.Context(), drpc.NewServer(other)) }()
	select {
	case err := <-refused:
		if err == nil {
			t.Fatal("the second Serve reported success on a name that was already published")
		}
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the refusal %v does not name %q", err, name)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the second Serve blocked instead of refusing a taken entry point")
	}

	// The name still belongs to the first server: the property is untouched,
	// and a port handed to it is served by the handlers registered there.
	if got := js.Global().Get(name); !got.Equal(entry) {
		t.Fatalf("globalThis.%s is no longer what the first server published", name)
	}
	ch := js.Global().Get("MessageChannel").New()
	handOver(entry, ch.Get("port2"))
	conn := drpc.NewConn(jsport.New(ch.Get("port1")))
	t.Cleanup(func() { conn.Close(nil) })

	res, err := echo.NewEchoServiceClient(conn).Once(t.Context(), echo.EchoRequest_builder{
		Message:       "abc",
		CircularShift: 1,
	}.Build())
	if err != nil {
		t.Fatalf("the first server lost its entry point: %v", err)
	}
	if got := res.GetMessage(); got != "bca" {
		t.Fatalf("got %q, want %q", got, "bca")
	}
}

// The name belongs to whoever holds the property, and catching the publish is
// what a host does with it rather than a lease on it: startWasmServer deletes
// the property the moment it has the entry point, and puts back whatever the
// page had under it. So Serve must unpublish only what is still its own — a
// blanket Delete here destroys a page global, or the accessor a second host is
// waiting on, and the symptom appears nowhere near this package.
func TestServeLeavesAReclaimedEntryPointAlone(t *testing.T) {
	const name = "drpcServeReclaimed"
	published := entryPoint(t, name)

	gw := jsport.NewGateway(jsport.WithEntryPoint(name))
	srv := drpc.NewServer(gw)
	echo.RegisterEchoServiceServer(srv, &echo.EchoServer{})

	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- gw.Serve(ctx, srv) }()
	t.Cleanup(func() { srv.Stop(); cancel() })

	awaitEntryPoint(t, published)

	// The host has the entry point; the name goes back to the page, exactly as
	// startWasmServer's release() restores what it found.
	sentinel := js.Global().Call("eval", `(() => () => {})()`)
	js.Global().Set(name, sentinel)

	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve reported %v, want nil for ctx cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return when its ctx was cancelled")
	}
	if got := js.Global().Get(name); !got.Equal(sentinel) {
		t.Errorf("globalThis.%s is %v: Serve unpublished a name that was no longer its own", name, got)
	}
}

// The entry point is JS-facing, so it is called by whatever the host feels
// like: nothing at all, a string, a stray null. Reading args[0] off any of them
// panics, and a panic in a js.Func takes the entire wasm instance down —
// server, peers and all — over what is only input the adapter cannot read
// (§4.2). It is dropped instead, and the next real port is still served.
//
// The sharp one is the object that is not a port. `drpcServe(ev)` in place of
// `drpcServe(ev.data.port)` is the mistake a host actually makes, and an event
// passes any test that asks only "is it an object": bound, it becomes a peer
// that can never receive anything, holding a parked goroutine and its js.Funcs
// for the life of the instance and reporting nothing at all. The duck test is
// what the adapter needs — a callable postMessage — so those are dropped too,
// and the goroutine count is the proof nothing was kept.
func TestServeIgnoresANonPort(t *testing.T) {
	const name = "drpcServeJunk"
	published := entryPoint(t, name)

	gw := jsport.NewGateway(jsport.WithEntryPoint(name))
	srv := drpc.NewServer(gw)
	echo.RegisterEchoServiceServer(srv, &echo.EchoServer{})

	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- gw.Serve(ctx, srv) }()
	t.Cleanup(func() {
		srv.Stop()
		cancel()
		<-served
	})

	entry := awaitEntryPoint(t, published)

	// Let the goroutines the setup started settle, so the count below is only
	// what the junk did.
	time.Sleep(100 * time.Millisecond)
	before := runtime.NumGoroutine()

	handOver(entry)                                              // called with no argument at all
	handOver(entry, nil)                                         // null
	handOver(entry, "not a port")                                // a string
	handOver(entry, 42)                                          // a number
	handOver(entry, js.Undefined())                              // explicit undefined
	handOver(entry, js.Global().Call("eval", `({})`))            // an object, but no postMessage
	handOver(entry, js.Global().Call("eval", `({ data: {} })`))  // the message event, not the port in it
	handOver(entry, js.Global().Call("eval", `({ ports: [] })`)) // and the same mistake spelled the other way

	// A settle loop rather than one sleep: a goroutine that is on its way out
	// must not read as a leak, and one that is parked forever never leaves.
	deadline := time.Now().Add(2 * time.Second)
	for {
		n := runtime.NumGoroutine()
		if n <= before {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d goroutine(s) parked on arguments that are not ports", n-before)
		}
		time.Sleep(20 * time.Millisecond)
	}

	ch := js.Global().Get("MessageChannel").New()
	handOver(entry, ch.Get("port2"))
	conn := drpc.NewConn(jsport.New(ch.Get("port1")))
	t.Cleanup(func() { conn.Close(nil) })

	res, err := echo.NewEchoServiceClient(conn).Once(t.Context(), echo.EchoRequest_builder{
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

// bytes wraps b as a Uint8Array, the message shape the wire contract names.
func bytes(b ...byte) js.Value {
	v := js.Global().Get("Uint8Array").New(len(b))
	js.CopyBytesToJS(v, b)
	return v
}

// Ports are shared: a page may post its own traffic down the same channel, and
// a runtime may deliver something that is not a view at all. None of it is an
// envelop and none of it is a teardown (PROTOCOL.md §4.2) — the channel
// survives junk in both directions.
//
// The sharp case is the last one. Protobuf keeps fields it does not know, so a
// message that is not ours at all can still decode to an Envelop carrying no
// frames — as can a later envelop extension. Only the *empty* message is the
// goodbye; anything else that decodes to no frames is input to drop, and
// reading it as EOF would let two stray bytes tear a live channel down.
func TestJunkIsIgnored(t *testing.T) {
	e := serve(t, nil, nil)

	for _, p := range []js.Value{e.p1, e.p2} {
		p.Call("postMessage", "not an envelop")
		p.Call("postMessage", js.Global().Get("Object").New())
		p.Call("postMessage", bytes(0xff, 0xff, 0xff, 0xff)) // a truncated varint
		p.Call("postMessage", bytes(0x18, 0x01))             // decodes: unknown field 3, no frames
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
	select {
	case err := <-e.served:
		t.Fatalf("junk tore the peer down: ServePeer returned %v", err)
	default:
	}
}

// Order under load, both directions at once: 64 requests down one bidi stream
// and their 64 answers back. A port preserves order and reliable mode drops
// nothing, so the answers must arrive as an exact sequence — this is also what
// exercises the queue, whose whole reason to be a slice is that it can never
// force a drop when flow control (§4.2.1) parks the sender instead.
func TestOrderUnderLoad(t *testing.T) {
	e := serve(t, nil, nil)

	const n = 64
	stream, err := e.client.Live(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// Send from a goroutine: past the initial window the sender parks until the
	// receiver drains, so sending and receiving must overlap.
	sendErr := make(chan error, 1)
	go func() {
		for i := range n {
			if err := stream.Send(echo.EchoRequest_builder{
				Message: strconv.Itoa(i), Repeat: 1,
			}.Build()); err != nil {
				sendErr <- err
				return
			}
		}
		sendErr <- stream.CloseSend()
	}()

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
	if err := <-sendErr; err != nil {
		t.Fatal(err)
	}

	want := make([]string, n)
	for i := range want {
		want[i] = strconv.Itoa(i)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// duckPorts builds two entangled ports out of plain objects. Everything a
// MessagePort has and these do not is a branch a MessageChannel can never
// reach: no addEventListener (so the on<type> properties are the wiring), no
// start, no close, and a postMessage that throws when handed a transfer list.
// It is JS source because the throw has to be a JS throw — a panic inside a
// js.Func would take the instance down instead of crossing back as an
// exception.
func duckPorts(t *testing.T) (js.Value, js.Value) {
	t.Helper()
	pair := js.Global().Call("eval", `(() => {
		const mk = () => ({
			onmessage: null,
			postMessage(data, transfer) {
				if (transfer !== undefined) throw new Error('this port refuses transfer lists')
				const peer = this.peer
				queueMicrotask(() => { if (peer.onmessage) peer.onmessage({ data }) })
			},
		})
		const a = mk(), b = mk()
		a.peer = b
		b.peer = a
		return [a, b]
	})()`)
	return pair.Index(0), pair.Index(1)
}

// A port is a duck type, and the adapter has to survive the thin end of it:
// the on<type> wiring, a plain re-post after a refused transfer list, and —
// because such a port has no close() and fires no close event — a channel
// whose only possible death signal is the goodbye itself (§4.5).
func TestHandRolledPort(t *testing.T) {
	p1, p2 := duckPorts(t)
	e := serveOn(t, p1, p2, nil, nil)

	stream, err := e.client.Live(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		msg := fmt.Sprintf("ping-%d", i)
		if err := stream.Send(echo.EchoRequest_builder{
			Message: msg, Repeat: 1,
		}.Build()); err != nil {
			t.Fatal(err)
		}
		res, err := stream.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if got := res.GetMessage(); got != msg {
			t.Fatalf("got %q, want %q", got, msg)
		}
	}

	// The thinnest end of all: a shim that fires the listener with no event at
	// all. A MessagePort never does, but a hand-rolled one is hand-rolled, and
	// there is nothing to read off an undefined event — reading it anyway
	// panics out of the js.Func and takes the whole instance with it, which is
	// the one thing worse than the dropped message §4.2 asks for. Driven from a
	// microtask because a js.Func invoked from Go re-enters the runtime.
	js.Global().Call("queueMicrotask", p2.Get("onmessage"))
	msg := "after the empty event"
	if err := stream.Send(echo.EchoRequest_builder{
		Message: msg, Repeat: 1,
	}.Build()); err != nil {
		t.Fatal(err)
	}
	res, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if got := res.GetMessage(); got != msg {
		t.Fatalf("got %q, want %q", got, msg)
	}

	e.conn.Close(nil)
	select {
	case err := <-e.served:
		if err != nil {
			t.Fatalf("ServePeer reported %v, want nil for a clean goodbye", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ServePeer did not see the goodbye (§4.5)")
	}
}

// WithTransfer(false) never builds a transfer list at all — the path for a
// port whose refusal the fallback cannot catch (a relay that silently drops
// transferred buffers). Structured clone copies instead, and the bytes are the
// same bytes.
func TestTransferDisabled(t *testing.T) {
	off := []jsport.Option{jsport.WithTransfer(false)}
	e := serve(t, off, off)

	msg := strings.Repeat("x", 64<<10)
	res, err := e.client.Once(t.Context(), echo.EchoRequest_builder{
		Message: msg,
	}.Build())
	if err != nil {
		t.Fatal(err)
	}
	if got := res.GetMessage(); got != msg {
		t.Fatalf("got %d bytes, want %d intact", len(got), len(msg))
	}
}

// Close is idempotent on both roles and from either direction — the pump's own
// exit path calls it, conn.Close calls it, and the host calls it when the wasm
// instance exits, all racing each other. It must also stay safe after the peer
// has already gone, and a call made afterwards must report the teardown's
// status rather than hang or panic.
func TestCloseIsIdempotent(t *testing.T) {
	e := serve(t, nil, nil)

	if _, err := e.client.Once(t.Context(), echo.EchoRequest_builder{
		Message: "abc",
	}.Build()); err != nil {
		t.Fatal(err)
	}

	if err := e.tp.Close(); err != nil {
		t.Fatalf("Close reported %v", err)
	}
	if err := e.tp.Close(); err != nil {
		t.Fatalf("second Close reported %v", err)
	}
	e.conn.Close(nil) // closes the transport a third time, through io.Closer
	e.gw.Close()
	e.gw.Close()

	select {
	case <-e.done:
	case <-time.After(2 * time.Second):
		t.Fatal("ServePeer did not return after both ends closed")
	}

	_, err := e.client.Once(t.Context(), echo.EchoRequest_builder{Message: "abc"}.Build())
	if got := status.Code(err); got != codes.Unavailable {
		t.Fatalf("call on a closed conn: got %v (%v), want Unavailable", got, err)
	}
}
