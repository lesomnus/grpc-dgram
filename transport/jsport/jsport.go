//go:build js && wasm

// Package jsport runs drpc over a JS message port: one posted message carries
// one marshaled Envelop as a Uint8Array (PROTOCOL.md §4.1) — byte for byte
// the WebSocket wire, so a peer here and a peer behind transport/gorilla speak
// the same protocol.
//
// A "message port" is anything with postMessage(data) and a "message" event:
// both ends of a MessageChannel, a Worker seen from the main thread, and the
// dedicated worker global scope (self) seen from inside it. window.postMessage
// is deliberately not one — its second argument is targetOrigin, not a
// transfer list — so for an iframe, transfer a MessagePort through the window
// and hand that port here.
//
// The deployment this exists for is a Go drpc.Server compiled to GOOS=js
// GOARCH=wasm running inside the page, with the browser UI as its client: a
// page reload restarts the whole server. Nothing here knows about wasm
// though — it is a port, and both ends could equally be two TypeScript
// endpoints across a Worker boundary (see ts/src/transport/port, the twin of
// this package).
//
// A port neither loses, duplicates nor reorders, so Reliable() answers true
// unconditionally: the core runs with every protocol timer off (§10.6) and
// per-stream flow control on (§4.2.1). That leaves the adapter the one duty
// the protocol no longer covers — teardown (§4.5) — and there is no socket to
// die here, so death has to be said out loud. Two mechanisms, both required:
//
//   - The goodbye is an empty message. A 0-byte message is a marshaled Envelop
//     with zero frames, which the wire never otherwise carries (§4.1 says
//     1..n), so it is free to mean "this endpoint is going away". Close posts
//     one; a pump that reads an empty message treats it as EOF, exits, and the
//     §4.5 teardown runs. This is what WebSocket's close handshake is, and the
//     only reason a peer that goes away does not leave live calls hanging
//     forever — with timers off, nothing else would ever fail them. It has to
//     be the empty message and not merely one that decoded to no frames: a
//     stray protobuf sharing the port decodes to no frames too, and that is
//     input to drop, not a channel to tear down (see deliver).
//   - An explicit Close from the host, for the deaths a port cannot report: a
//     wasm instance that exited or panicked (go.run()'s promise resolving), a
//     terminated Worker, a page teardown. A "close" event on the port counts
//     as death too where the runtime fires one; "messageerror" does not — a
//     message that cannot be deserialized is malformed input, never a
//     teardown (§4.2).
//
// There is deliberately no keepalive: two endpoints in one process cannot be
// partitioned, and an unanswered ping would only measure how busy the peer is.
//
// postMessage applies no backpressure, and that is fine: in reliable mode
// per-stream flow control (§4.2.1) bounds what a conforming peer can put in
// flight, so the receive queue cannot grow without limit. A received message
// is therefore never dropped — in reliable mode a gap is a protocol error, not
// a lost datagram.
//
// Two js/wasm rules the host must respect: main must never return (a returned
// main kills the instance and every registered js.Func with it), and every
// js.Func must be released — Close does that for the ones registered here.
//
// The port carries no authentication of its own: it is exactly as trustworthy
// as the code holding its other end (PROTOCOL.md §15).
package jsport

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"syscall/js"

	drpc "github.com/lesomnus/grpc-dgram"
	"google.golang.org/grpc/peer"
)

// DefaultMaxMessageSize is 0 — unlimited, like WebSocket: structured clone has
// no protocol ceiling, so nothing needs refusing by default (PROTOCOL.md
// §4.4).
const DefaultMaxMessageSize = 0

// DefaultLabel is what Peer() reports for a port, which has no address of its
// own.
const DefaultLabel = "port"

// DefaultEntryPoint is the global (*Gateway).Serve publishes its entry point
// as, and the name ts/src/wasm's open() waits for. Both sides default to it,
// so a server started with Serve and a page started with that entry need no
// configuration at all.
const DefaultEntryPoint = "drpcServe"

type options struct {
	maxMessageSize int
	transfer       bool
	label          string
	entryPoint     string
}

type Option func(*options)

// WithMaxMessageSize sets the largest marshaled Envelop this endpoint will
// send, in bytes; 0 (the default) means unlimited. It bounds sends only;
// receives accept any message. Set it for a peer whose path caps message size
// (a Worker pool with its own framing, a relay) — an envelop past it is
// refused synchronously and the core fails the owning call with
// ResourceExhausted (PROTOCOL.md §4.4).
func WithMaxMessageSize(n int) Option {
	return func(o *options) { o.maxMessageSize = n }
}

// WithTransfer decides whether an outbound message's ArrayBuffer rides the
// postMessage transfer list instead of being copied by structured clone.
// Default true: the buffer is allocated per message here, so handing it over
// is safe and saves a copy. Disable it for a port that refuses transfer lists
// and reports it in a way the adapter's fallback cannot see (it retries a
// throw once, plainly).
func WithTransfer(v bool) Option {
	return func(o *options) { o.transfer = v }
}

// WithLabel names the port for Peer() and for the address handlers read back
// through peer.FromContext. Default DefaultLabel.
func WithLabel(s string) Option {
	return func(o *options) { o.label = s }
}

// WithEntryPoint names the global (*Gateway).Serve publishes its entry point
// as; default DefaultEntryPoint. The name is the entire handshake — the host
// waits for that property to appear on globalThis and calls it with a port —
// so changing it here means changing it in the host too. Change it when one
// realm runs two servers: Serve refuses a name that is already taken rather
// than steal it. Transport ignores it; a client is handed its port directly.
func WithEntryPoint(name string) Option {
	return func(o *options) { o.entryPoint = name }
}

func buildOptions(opts []Option) options {
	o := options{
		maxMessageSize: DefaultMaxMessageSize,
		transfer:       true,
		label:          DefaultLabel,
		entryPoint:     DefaultEntryPoint,
	}
	for _, f := range opts {
		f(&o)
	}
	return o
}

// Transport is the client-side endpoint: one port talking to one server, so no
// peer key is needed (PROTOCOL.md §6.4). It is the tx handler for
// drpc.NewConn — implementing drpc.TransportInfo and drpc.ConnAttacher
// directly so neither is masked by a wrapper. drpc.NewConn attaches it and the
// pump starts by itself: no user goroutine, and conn.Close (or Close here)
// tears everything down, port included.
type Transport struct {
	p        *msgPort
	attached atomic.Bool
}

// New wraps a JS message port. It registers the port's listeners and starts
// buffering immediately, so construct it on the same tick as the port itself
// — a port wired through onmessage drops whatever is posted before the handler
// is set (see Bind) — and attach it promptly (drpc.NewConn) so the queue
// starts draining. The Transport owns the port from here on: Close closes it,
// and closing the attached Conn does too.
func New(port js.Value, opts ...Option) *Transport {
	return &Transport{p: newPort(port, buildOptions(opts))}
}

// AttachConn is called by drpc.NewConn: it starts the pump, which runs until
// the peer says goodbye, the port dies, or Close is called, and on every exit
// performs the §4.5 teardown — conn.Close with the cause — the only mechanism
// that unblocks live calls in reliable mode. Closing the transport after the
// pump exits posts this side's own goodbye, so a peer that outlives us learns
// of it too.
func (t *Transport) AttachConn(conn *drpc.Conn) {
	if !t.attached.CompareAndSwap(false, true) {
		panic("jsport: transport already attached to a Conn")
	}
	go func() {
		// Nothing extra on the delivery ctx: one Conn is one channel, and its
		// mode was resolved at construction from Reliable() (§4.3).
		err := t.p.serve(context.Background(), context.Background(), conn)
		conn.Close(err)
		t.Close()
	}()
}

// Close posts the goodbye, detaches the port's listeners, closes the port and
// releases every js.Func this transport registered; the pump's exit then fails
// any live calls. Idempotent, which it must be: the pump's own exit path calls
// it, drpc.Conn.Close calls it as an io.Closer, and the application calls it to
// report a death the port itself never would.
func (t *Transport) Close() error {
	t.p.close()
	return nil
}

// Reliable reports true: a port neither loses, duplicates, nor reorders. The
// core discovers this and disables all protocol timers (PROTOCOL.md §10.6),
// which is what makes the pump's teardown duty mandatory.
func (t *Transport) Reliable() bool { return true }

// Peer names the remote end for grpc.Peer and peer.FromContext
// (drpc.TransportPeer). A port has no address, so it is named by its label.
func (t *Transport) Peer() *peer.Peer {
	return &peer.Peer{Addr: portAddr{label: t.p.label}}
}

// Handle sends one frame as a single-frame envelop.
func (t *Transport) Handle(ctx context.Context, f *drpc.Frame) error {
	e := &drpc.Envelop{}
	e.SetFrames([]*drpc.Frame{f})
	return t.Send(ctx, e)
}

// Send transmits one envelop as one posted message. An envelop over the size
// limit is refused synchronously with an error wrapping drpc.ErrMessageTooLarge
// (PROTOCOL.md §4.4); a send racing the teardown fails Unavailable.
func (t *Transport) Send(_ context.Context, e *drpc.Envelop) error {
	return t.p.send(e)
}

// peerKey identifies one served port within a Gateway. Deliberately opaque and
// never reused: a peer that reconnects on a fresh port is a fresh peer
// (PROTOCOL.md §6.4).
type peerKey uint64

type gwPort struct {
	p      *msgPort
	key    peerKey
	served atomic.Bool
}

// Gateway is the server-side endpoint: one drpc.Server serving many peers, one
// port each — a wasm instance answering the page's main thread and a handful
// of Workers at once. It is the tx handler for drpc.NewServer, implementing
// drpc.TransportInfo directly so mode discovery is not masked by a wrapper:
// every port peer is reliable, so unlike the WebRTC gateway there is a single
// answer to advertise.
type Gateway struct {
	o    options
	next atomic.Uint64

	mu    sync.Mutex
	peers map[peerKey]*gwPort
}

// NewGateway builds a Gateway; ports join it via Bind and ServePeer.
func NewGateway(opts ...Option) *Gateway {
	return &Gateway{o: buildOptions(opts), peers: map[peerKey]*gwPort{}}
}

// Reliable reports true: a port neither loses, duplicates, nor reorders. The
// core discovers this and disables all protocol timers (PROTOCOL.md §10.6),
// which is what makes ServePeer's teardown duty mandatory.
func (g *Gateway) Reliable() bool { return true }

// Bind registers the gateway's listeners on port and starts buffering its
// inbound messages; it is idempotent per port. Call it as soon as the port
// exists: a MessagePort holds what arrives until start() and loses nothing,
// but a port wired through onmessage — the worker global scope, a hand-rolled
// shim — drops every message posted before the handler is set, and a client
// that opens a call the instant it hands the port over is the normal case.
// ServePeer binds implicitly, so Bind is only needed when serving starts
// later:
//
//	gw.Bind(port)
//	go gw.ServePeer(ctx, srv, port)
func (g *Gateway) Bind(port js.Value) { g.bind(port) }

func (g *Gateway) bind(v js.Value) *gwPort {
	g.mu.Lock()
	defer g.mu.Unlock()
	// js.Value is not comparable, so identity is Equal over the bound set —
	// one linear scan per connection, never per message.
	for _, b := range g.peers {
		if b.p.v.Equal(v) {
			return b
		}
	}
	b := &gwPort{p: newPort(v, g.o), key: peerKey(g.next.Add(1))}
	g.peers[b.key] = b
	return b
}

func (g *Gateway) drop(b *gwPort) {
	g.mu.Lock()
	delete(g.peers, b.key)
	g.mu.Unlock()
	// Serving stopped, so this peer is abandoned — say goodbye. The port
	// reports nothing by itself, so a peer left unnotified would keep its
	// calls open against a server that no longer listens (§4.5).
	b.p.close()
}

// ServePeer delivers port's frames to srv under a fresh peer key — annotated
// reliable, so the peer runs with every timer off (PROTOCOL.md §4.3, §10.6) —
// and blocks until the peer says goodbye, the port dies, or ctx is done. On
// EVERY exit it performs the §4.5 teardown duty — srv.DisconnectPeer with the
// cause — and deregisters the peer: exiting abandons the port (the key is
// never reused), so the peer's live calls and state must die with it whether
// the port died or the caller cancelled ctx. Returns nil on a clean goodbye or
// ctx cancellation, the death cause otherwise. Each port is served at most
// once.
func (g *Gateway) ServePeer(ctx context.Context, srv *drpc.Server, port js.Value) error {
	b := g.bind(port)
	if !b.served.CompareAndSwap(false, true) {
		return errors.New("jsport: port already served")
	}
	defer g.drop(b)

	rxCtx := drpc.NewReliableContext(drpc.NewPeerContext(ctx, b.key), true)
	// The peer key is opaque (one port = one peer); handlers reading the
	// standard peer.FromContext get the port's label instead.
	rxCtx = peer.NewContext(rxCtx, &peer.Peer{Addr: portAddr{label: b.p.label}})
	err := b.p.serve(ctx, rxCtx, srv)
	srv.DisconnectPeer(b.key, err)
	return err
}

// Serve publishes the JS entry point, then serves every port the host hands to
// it until ctx is done. It is the whole server side of a wasm instance:
//
//	gw := jsport.NewGateway()
//	srv := drpc.NewServer(gw)
//	pb.RegisterEchoServiceServer(srv, &myHandler{})
//	log.Fatal(gw.Serve(context.Background(), srv)) // blocks, so main never returns
//
// Publishing the entry point IS the readiness signal, and it is the only one:
// js.Global().Set reaches JS as Reflect.set(globalThis, name, fn), which
// triggers an accessor property, so a host that defines one before go.run() is
// woken by the assignment itself — no ready callback, no second name, nothing
// to poll (ts/src/wasm's open() is that host). Which is why
// nothing may be published before the server can serve: call Serve after every
// service is registered (PROTOCOL.md §13), and the host may open a call on the
// very tick it hands the port over.
//
// The published value is one function taking one port. Each call binds the port
// synchronously — before the callback returns, so a client that posts on that
// same tick loses nothing — and serves it on its own goroutine, because
// ServePeer blocks until the peer goes away and a js.FuncOf holds the JS event
// loop for exactly as long as it blocks. One port is one peer (§6.4), so
// calling it again with another port serves a second peer off the same srv. A
// call whose argument is not a port — nothing at all, a string, a stray null,
// or the message event rather than the port inside it (see isPort) — is
// ignored: args[0] unguarded panics, and a panic takes the whole instance down
// over what is only input the adapter cannot read (§4.2).
//
// Returns an error, having published nothing, if globalThis[name] is already
// set — stealing another server's entry point would leave both of them waiting
// for ports that never arrive.
//
// On ctx cancellation it unpublishes the global, releases the js.Func — in that
// order, because a func released while JS can still reach it turns the next
// call into a console error and a peer that is never served — and then closes
// the gateway, so every served peer gets the goodbye and, on its own goroutine,
// the §4.5 teardown that follows from it. Returns nil.
//
// Unpublishing removes the property only while it still holds what this Serve
// put there. Catching the publish is what a host does WITH the name, not a
// lease on it: open() takes the property straight back off globalThis and puts
// whatever the page had under it back — so deleting the name
// unconditionally here would destroy something that is no longer ours, and the
// symptom (a page global, or another instance's accessor, gone) would point
// nowhere near this line.
func (g *Gateway) Serve(ctx context.Context, srv *drpc.Server) error {
	name := g.o.entryPoint
	if v := js.Global().Get(name); !v.IsUndefined() {
		return fmt.Errorf("jsport: globalThis.%s is already set", name)
	}

	fn := js.FuncOf(func(_ js.Value, args []js.Value) any {
		if len(args) == 0 || !isPort(args[0]) {
			// Called with nothing, or with something that is not a port:
			// dropped like a message the adapter cannot read, because reading
			// args[0] anyway panics the instance (§4.2).
			return nil
		}
		port := args[0]
		g.Bind(port)
		go func() { _ = g.ServePeer(ctx, srv, port) }()
		return nil
	})
	js.Global().Set(name, fn)

	<-ctx.Done()
	// Unpublish, then release: a js.Func released while JS can still reach it
	// turns the next call into a console error and a port that is never served.
	if js.Global().Get(name).Equal(fn.Value) {
		js.Global().Delete(name)
	}
	fn.Release()
	g.Close()
	return nil
}

// isPort is the duck test the entry point owes its caller: an object with a
// callable postMessage, which is the whole of what the adapter needs and the
// same shape ts/src/transport/port's PortLike names. "An object" alone is not
// enough, and the difference is not academic — `drpcServe(ev)` instead of
// `drpcServe(ev.data.port)` is the mistake a host actually makes, and an event
// is an object. Bound as a port it would be a peer that can never receive
// anything, with a goroutine parked on it and its js.Funcs held for the life of
// the instance, reporting nothing at all; refused here it is one ignored call,
// like every other argument the adapter cannot read (§4.2).
func isPort(v js.Value) bool {
	return v.Type() == js.TypeObject && v.Get("postMessage").Type() == js.TypeFunction
}

// Handle sends one frame as a single-frame envelop to the peer named in ctx.
func (g *Gateway) Handle(ctx context.Context, f *drpc.Frame) error {
	e := &drpc.Envelop{}
	e.SetFrames([]*drpc.Frame{f})
	return g.Send(ctx, e)
}

// Send transmits one envelop as one posted message to the peer named in ctx.
func (g *Gateway) Send(ctx context.Context, e *drpc.Envelop) error {
	key, ok := drpc.PeerFromContext(ctx)
	if !ok {
		return errors.New("jsport: no peer in context")
	}
	k, ok := key.(peerKey)
	if !ok {
		return fmt.Errorf("jsport: foreign peer key %T", key)
	}
	g.mu.Lock()
	b := g.peers[k]
	g.mu.Unlock()
	if b == nil {
		return fmt.Errorf("jsport: peer %d is disconnected", k)
	}
	return b.p.send(e)
}

// Close tears every bound port down — each gets the goodbye, so every peer
// learns the server is gone — and each ServePeer then exits through its own
// §4.5 teardown. This is the hook for a wasm instance that is about to leave:
// call it, and no peer is left with calls that can never end.
func (g *Gateway) Close() {
	g.mu.Lock()
	bs := make([]*gwPort, 0, len(g.peers))
	for _, b := range g.peers {
		bs = append(bs, b)
	}
	g.mu.Unlock()
	for _, b := range bs {
		b.p.close()
	}
}
