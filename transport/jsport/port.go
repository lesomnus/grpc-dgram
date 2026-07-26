//go:build js && wasm

package jsport

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"syscall/js"

	drpc "github.com/lesomnus/grpc-dgram"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// errPeerGone is the death cause a "close" event reports: the entangled port
// was closed or collected without a goodbye, so the peer vanished rather than
// left. Named so live calls say why they failed (§4.5).
var errPeerGone = errors.New("jsport: the port's peer went away")

// portAddr names a message port as a net.Addr, so peer.FromContext and
// grpc.Peer report something real on a transport with no address of its own.
type portAddr struct{ label string }

func (a portAddr) Network() string { return "js" }
func (a portAddr) String() string  { return a.label }

func marshal(e *drpc.Envelop, limit int) ([]byte, error) {
	data, err := proto.MarshalOptions{Deterministic: true}.Marshal(e)
	if err != nil {
		return nil, err
	}
	if limit > 0 && len(data) > limit {
		return nil, fmt.Errorf("jsport: %d-byte envelop over the %d-byte limit: %w",
			len(data), limit, drpc.ErrMessageTooLarge)
	}
	return data, nil
}

type listener struct {
	typ string
	fn  js.Func
}

// msgPort owns one JS message port. It registers the port's listeners at
// construction and buffers from that instant — a message posted before a
// listener exists is lost, and a client that opens a call the moment it hands
// the port over is the normal case.
//
// The queue is why a listener and a pump are separate things. A js.FuncOf
// callback can block and still return correctly, but it does so by holding the
// JS thread: a 50 ms park inside the callback freezes the JS event loop for
// 50 ms, and with it every other consumer of that loop. So the "message"
// listener copies out, enqueues and returns, and the pump goroutine delivers
// into the core, where blocking is allowed and correct in reliable mode
// (PROTOCOL.md §4.2). The queue is a slice plus a wake channel rather than a
// fixed-size chan because a slow consumer must never be able to force a drop —
// §4.2 forbids one in reliable mode — and it needs no bound of its own: flow
// control (§4.2.1) already limits what a conforming peer can put in flight.
type msgPort struct {
	v     js.Value // the port itself
	u8    js.Value // the local realm's Uint8Array
	ab    js.Value // the local realm's ArrayBuffer, for isView
	max   int      // send limit in bytes; <= 0 is unlimited
	xfer  bool     // hand the buffer over instead of copying it
	label string

	// events reports whether the port has addEventListener; the on<type>
	// properties are the fallback, and the choice must be the same when the
	// listeners are registered and when they are removed.
	events    bool
	listeners []listener

	mu    sync.Mutex
	items [][]byte
	err   error // first death cause; nil for a clean goodbye or a local close
	gone  bool

	wake   chan struct{} // buffered 1: "the queue is non-empty", never blocks the callback
	dead   chan struct{} // closed once, by fail
	closer sync.Once
}

func newPort(v js.Value, o options) *msgPort {
	p := &msgPort{
		v:     v,
		u8:    js.Global().Get("Uint8Array"),
		ab:    js.Global().Get("ArrayBuffer"),
		max:   o.maxMessageSize,
		xfer:  o.transfer,
		label: o.label,
		wake:  make(chan struct{}, 1),
		dead:  make(chan struct{}),
	}
	p.events = v.Get("addEventListener").Type() == js.TypeFunction

	p.listen("message", p.onMessage)
	// messageerror means the runtime could not deserialize what arrived: a
	// malformed message, dropped, never a teardown (§4.2). The listener exists
	// to say exactly that — every event this port can raise is accounted for
	// here, and the next reader does not have to wonder whether this one was
	// forgotten.
	p.listen("messageerror", func(js.Value) {})
	// Newer runtimes fire "close" on a MessagePort when its entangled port is
	// closed or collected. That is death without a goodbye, and one of the two
	// signals that reach a pump blocked in reliable-mode backpressure (§4.5).
	p.listen("close", func(js.Value) { p.fail(errPeerGone) })

	// A MessagePort wired through addEventListener stays paused until start()
	// is called; getting this wrong is silence, not an error. The worker global
	// scope has no start, and a port wired through onmessage starts itself.
	if v.Get("start").Type() == js.TypeFunction {
		_ = p.call("start")
	}
	return p
}

// listen registers fn for one port event. addEventListener is preferred — it
// composes with whatever else already shares the port — and the on<type>
// property is the fallback for a value that has none (the worker global scope
// reached through a hand-rolled shim, a test double).
func (p *msgPort) listen(typ string, fn func(js.Value)) {
	f := js.FuncOf(func(_ js.Value, args []js.Value) any {
		ev := js.Undefined()
		if len(args) > 0 {
			ev = args[0]
		}
		fn(ev)
		return nil
	})
	p.listeners = append(p.listeners, listener{typ: typ, fn: f})
	if p.events {
		p.v.Call("addEventListener", typ, f)
		return
	}
	p.v.Set("on"+typ, f)
}

// detach removes the listeners, closes the port, and releases every js.Func.
// The order is load-bearing in both directions: a js.Func the instance never
// releases leaks it for the life of the instance, and one released while JS can
// still reach it turns every later call into a console error and a silently
// lost message — so nothing is released until the port can no longer reach it.
//
// close() hands a MessagePort back to the runtime, and is skipped for one port
// only: a dedicated worker's own global scope, where close() TERMINATES the
// worker. A server hosted in a worker serves its page over exactly that port
// (and may serve more over ports transferred to it), so calling it would let
// one peer's §4.5 teardown kill the whole instance — every other peer, and
// whatever it was still doing. Ending a worker is the host's decision, taken
// after its endpoints have torn down; detaching is all this endpoint owes such
// a port. The TypeScript twin makes the same exception, for the same reason.
func (p *msgPort) detach() {
	rm := p.v.Get("removeEventListener").Type() == js.TypeFunction
	for _, l := range p.listeners {
		switch {
		case p.events && rm:
			_ = p.call("removeEventListener", l.typ, l.fn)
		case !p.events:
			p.v.Set("on"+l.typ, js.Null())
		}
	}
	if !p.v.Equal(js.Global()) && p.v.Get("close").Type() == js.TypeFunction {
		_ = p.call("close")
	}
	for _, l := range p.listeners {
		l.fn.Release()
	}
	p.listeners = nil
}

// onMessage runs on the JS thread: it copies out and returns. Everything past
// that — decoding, delivery, blocking in the core's flow control — belongs to
// the pump, because parking here parks the whole JS event loop.
func (p *msgPort) onMessage(ev js.Value) {
	if ev.Type() != js.TypeObject {
		// A hand-rolled port that fired the listener with no event at all —
		// listen materializes js.Undefined for exactly that. Reading a field
		// off it panics, and a panic on this path takes the whole instance
		// down; a message the adapter cannot read is dropped, never fatal
		// (§4.2).
		return
	}
	data, ok := p.bytesOf(ev.Get("data"))
	if !ok {
		// A string, or an object from something else sharing this port. Not an
		// envelop; ignored, never a teardown (§4.2).
		return
	}
	p.mu.Lock()
	if p.gone {
		// Nothing is queued once this endpoint is dying: the pump delivers
		// what arrived before the teardown began and then leaves, and the
		// calls this message could belong to are already failing (§4.5).
		p.mu.Unlock()
		return
	}
	p.items = append(p.items, data)
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default: // a wake is already pending; the pump drains the whole queue
	}
}

// pop takes the oldest queued message.
func (p *msgPort) pop() ([]byte, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.items) == 0 {
		return nil, false
	}
	data := p.items[0]
	p.items[0] = nil
	p.items = p.items[1:]
	return data, true
}

// bytesOf copies an inbound message into Go memory. A Uint8Array is the
// contract (§4.1); any other ArrayBufferView is accepted by wrapping its range,
// because a peer that posted an Int8Array or a DataView still posted our bytes.
// Anything else is not an envelop.
//
// The copy is the design, not a cost to optimize away: crossing the wasm
// boundary as marshaled bytes is 2.5-3x faster than building the equivalent JS
// object graph field by field — one memcpy beats thirty host calls.
func (p *msgPort) bytesOf(v js.Value) ([]byte, bool) {
	if v.Type() != js.TypeObject {
		return nil, false
	}
	src := v
	if !v.InstanceOf(p.u8) {
		if !p.ab.Call("isView", v).Bool() {
			return nil, false
		}
		src = p.u8.New(v.Get("buffer"), v.Get("byteOffset"), v.Get("byteLength"))
	}
	// The length comes from the view, not from its buffer: a view over part of
	// a larger buffer carries only its own range, and a zero-length one is the
	// goodbye (see deliver), which no widening may invent or erase.
	data := make([]byte, src.Get("length").Int())
	js.CopyBytesToGo(data, src)
	return data, true
}

// fail records the first death cause and trips dead. Called by the "close"
// listener, by a refused send, and by close itself — the teardown is the same
// either way; only the cause a live call reports differs.
func (p *msgPort) fail(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.gone {
		return
	}
	p.gone, p.err = true, err
	close(p.dead)
}

func (p *msgPort) deathErr() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

func (p *msgPort) dying() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.gone
}

// closedErr is what a send racing the teardown fails with. It is a status
// error so the race is invisible: the core passes a status error through
// unchanged (toStatusErr), so the send fails with the very code the §4.5
// teardown would have given the call a moment later.
func (p *msgPort) closedErr() error {
	if err := p.deathErr(); err != nil {
		return status.Errorf(codes.Unavailable, "jsport: port closed: %v", err)
	}
	return status.Error(codes.Unavailable, "jsport: port closed")
}

// call invokes a method on the port. js.Value.Call turns a JS exception into a
// Go panic, and the port is a foreign object that may refuse anything it likes:
// unrecovered, one refused postMessage would take the instance down instead of
// failing one call.
func (p *msgPort) call(name string, args ...any) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("jsport: %s: %v", name, r)
		}
	}()
	p.v.Call(name, args...)
	return nil
}

// post hands one message to the port. The bytes are copied into a fresh
// Uint8Array; with transfer enabled its buffer rides the postMessage transfer
// list, which is safe precisely because we allocated it, and skips the
// structured-clone copy. A port that refuses a transfer list must still work,
// so the throw is caught and the message re-posted plainly — postMessage
// detaches the buffer only after serializing successfully, so the one that
// threw is still intact.
func (p *msgPort) post(data []byte, transfer bool) error {
	buf := p.u8.New(len(data))
	js.CopyBytesToJS(buf, data)
	if transfer {
		if err := p.call("postMessage", buf, []any{buf.Get("buffer")}); err == nil {
			return nil
		}
	}
	return p.call("postMessage", buf)
}

// send transmits one envelop as one posted message. An envelop over the size
// limit is refused synchronously with an error wrapping drpc.ErrMessageTooLarge
// (PROTOCOL.md §4.4) and the port stays up.
func (p *msgPort) send(e *drpc.Envelop) error {
	data, err := marshal(e, p.max)
	if err != nil {
		return err
	}
	if p.dying() {
		return p.closedErr()
	}
	if err := p.post(data, p.xfer); err != nil {
		// The port refused the message: it is gone. Same teardown as a close
		// event, so a peer whose port died silently still fails its calls.
		p.fail(err)
		return p.closedErr()
	}
	return nil
}

// goodbye posts the empty envelop that means "this endpoint is going away"
// (§4.1: the wire never otherwise carries a zero-frame envelop). Best effort —
// a port whose peer is already gone has nobody left to tell. No transfer list:
// a zero-length buffer saves nothing and every runtime accepts the plain form.
func (p *msgPort) goodbye() { _ = p.post(nil, false) }

// close says goodbye, then detaches. Idempotent, and safe from either role:
// the goodbye goes out before the death latch shuts sends, so the peer learns
// of a death the port itself would never have reported.
func (p *msgPort) close() {
	p.closer.Do(func() {
		p.goodbye()
		p.fail(nil)
		p.detach()
	})
}

// serve pumps queued messages into h until the peer says goodbye, the port
// dies, or ctx is done; on death it delivers what was already received first.
// Returns nil for a clean goodbye or ctx cancellation, the death cause
// otherwise. Exiting abandons the port either way, so the caller owes the §4.5
// teardown on every return.
//
// rxCtx carries what the core must know about the sender (the peer key and the
// mode, §4.3/§6.4); it is delivered under a child cancelled on death. That link
// is the point of a death signal that lives outside this loop: in reliable mode
// Handle may block in flow-controlled backpressure (§4.2), and while it does,
// this loop observes nothing — so a "close" event, a refused send or Close must
// reach the blocked delivery, or the §4.5 teardown never runs. The death drain
// below still delivers every frame that fits a stream buffer (the core prefers
// delivery over a dead ctx); only a delivery that would have to block fails its
// call instead.
func (p *msgPort) serve(ctx context.Context, rxCtx context.Context, h drpc.FrameHandler) error {
	dctx, dcancel := context.WithCancel(rxCtx)
	defer dcancel()
	go func() {
		select {
		case <-p.dead:
			dcancel()
		case <-dctx.Done():
		}
	}()

	for {
		// Checked before the queue, not only in the select below: a peer that
		// keeps the queue non-empty — stateless control frames are not bounded
		// by flow control — would otherwise defer cancellation indefinitely,
		// and ServePeer's caller expects ctx to stop it.
		if ctx.Err() != nil {
			return nil
		}
		if data, ok := p.pop(); ok {
			if deliver(dctx, data, h) {
				return nil
			}
			continue
		}
		select {
		case <-p.wake:
		case <-p.dead:
			for {
				data, ok := p.pop()
				if !ok {
					return p.deathErr()
				}
				if deliver(dctx, data, h) {
					return nil
				}
			}
		case <-ctx.Done():
			return nil
		}
	}
}

// deliver decodes one message and hands its frames to h in order, reporting
// whether it was the peer's goodbye. A message that does not decode is dropped:
// frame-level errors never tear down the channel (§4.2).
//
// The goodbye is 0 BYTES, not merely a message that decoded to no frames.
// proto.Unmarshal keeps fields it does not know — a later envelop extension,
// another library's protobuf sharing the port, two bytes of anyone's junk — so
// plenty of messages decode to zero frames, and reading any of them as EOF
// would tear a healthy channel down over input §4.2 says to drop. Only the
// empty message can be the close frame: an envelop carries 1..n frames (§4.1)
// and marshaling one with none is exactly 0 bytes, in TypeScript too — the
// twin adapter makes the same check, and the two halves must agree on the byte
// sequence that ends a connection.
func deliver(ctx context.Context, data []byte, h drpc.FrameHandler) bool {
	if len(data) == 0 {
		return true
	}
	e := &drpc.Envelop{}
	if err := proto.Unmarshal(data, e); err != nil {
		return false
	}
	// An envelop that decoded to no frames is delivered as no frames, i.e.
	// dropped like the malformed message it is.
	drpc.Unpack(ctx, e, h)
	return false
}
