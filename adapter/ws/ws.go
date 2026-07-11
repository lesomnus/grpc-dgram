// Package ws runs drpc over WebSocket: one binary message carries one
// marshaled Envelop. The channel is reliable and ordered, so the core runs
// in reliable mode with every protocol timer and retransmission off
// (PROTOCOL.md §10.6) — leaving this adapter the two duties the protocol no
// longer covers:
//
//   - Teardown (§4.5): when the socket dies, Conn.Close /
//     Server.DisconnectPeer is the only mechanism that unblocks live calls.
//     ServeConn and ServePeer call it on every exit path — this is the point
//     of the adapter, not a nicety.
//   - Liveness (§10.6): death is detected by read errors plus a ping/pong
//     keepalive (WithKeepalive); a peer that makes no read progress within
//     the keepalive timeout is dead.
//
// Received frames are delivered synchronously from the read loop, so a full
// stream buffer blocks the read and propagates into TCP backpressure (§4.2).
//
// WebSocket fragments and reassembles internally, so no size logic is needed:
// the default send limit is unlimited (§4.4). WithMaxMessageSize bounds sends
// for deployments whose path (a proxy, a browser) caps message size.
//
// A ws:// wire is plaintext. Deploy wss:// (TLS) or stay on a trusted
// network — see PROTOCOL.md §15.
package ws

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	drpc "github.com/lesomnus/grpc-dgram"
	"google.golang.org/protobuf/proto"
)

// unblockNow is a read deadline in the past: setting it fails the pending
// blocking read so the read loop can observe ctx cancellation.
var unblockNow = time.Unix(1, 0)

// DefaultKeepaliveInterval is the ping cadence; DefaultKeepaliveTimeout is
// how long the peer may go without read progress (data or pong) before the
// connection is declared dead. The timeout leaves room for one lost ping
// round on a congested path.
const (
	DefaultKeepaliveInterval = 20 * time.Second
	DefaultKeepaliveTimeout  = 30 * time.Second
)

type options struct {
	maxMessageSize    int
	keepaliveInterval time.Duration
	keepaliveTimeout  time.Duration
}

type Option func(*options)

// WithMaxMessageSize sets the largest marshaled Envelop this endpoint will
// send, in bytes; 0 (the default) means unlimited — a reliable transport
// carries any size (PROTOCOL.md §4.4). It bounds sends only; receives accept
// any message.
func WithMaxMessageSize(n int) Option {
	return func(o *options) { o.maxMessageSize = n }
}

// WithKeepalive tunes liveness detection: a ping every interval, death when
// the peer makes no read progress within timeout. interval must be shorter
// than timeout or an idle connection dies spuriously. Non-positive values
// keep the defaults.
func WithKeepalive(interval, timeout time.Duration) Option {
	return func(o *options) {
		if interval > 0 {
			o.keepaliveInterval = interval
		}
		if timeout > 0 {
			o.keepaliveTimeout = timeout
		}
	}
}

func buildOptions(opts []Option) options {
	o := options{
		keepaliveInterval: DefaultKeepaliveInterval,
		keepaliveTimeout:  DefaultKeepaliveTimeout,
	}
	for _, f := range opts {
		f(&o)
	}
	return o
}

func marshal(e *drpc.Envelop, limit int) ([]byte, error) {
	data, err := proto.Marshal(e)
	if err != nil {
		return nil, err
	}
	if limit > 0 && len(data) > limit {
		return nil, fmt.Errorf("ws: %d-byte envelop over the %d-byte limit: %w",
			len(data), limit, drpc.ErrMessageTooLarge)
	}
	return data, nil
}

// sock serializes data writes: gorilla allows only one concurrent writer.
// (WriteControl is separately safe for concurrent use and bypasses this.)
type sock struct {
	c  *websocket.Conn
	mu sync.Mutex
}

// write sends one binary message. The deadline matters because protocol
// timers are off: a peer that stops draining would otherwise block a write
// forever while it holds the mutex, wedging every call on this socket. A
// write that cannot progress within the keepalive timeout is transport death
// from the sender's side; the failed socket then also fails the read loop,
// which performs the §4.5 teardown.
func (s *sock) write(data []byte, timeout time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.c.SetWriteDeadline(time.Now().Add(timeout))
	return s.c.WriteMessage(websocket.BinaryMessage, data)
}

// serve pumps binary messages from c into h until ctx is done or the socket
// dies, and owns liveness: a ping every keepalive interval, death when no
// read progress (data or pong) happens within the keepalive timeout.
//
// Delivery is synchronous on purpose: in reliable mode Handle may block, and
// blocking the read loop is exactly what turns a full stream buffer into TCP
// backpressure (PROTOCOL.md §4.2). The deadline is re-armed before each
// read, so time spent blocked in Handle is not counted against the peer —
// but while blocked, this side answers no pings either, and a peer wedged
// beyond the peer's own keepalive timeout reads as dead. That is the bound
// on backpressure patience.
//
// Frame-level Handle errors mean malformed input and never tear down the
// channel (§4.2). Non-binary messages and unparseable envelops are ignored.
//
// Returns nil on ctx cancellation, local socket close, or an orderly close
// handshake; the fatal transport error otherwise.
func serve(ctx context.Context, c *websocket.Conn, o options, rxCtx context.Context, h drpc.FrameHandler) error {
	// extend grants another keepalive window. The mutex closes the race
	// between cancellation (whose past deadline must stick) and a concurrent
	// extension by the read loop or a pong.
	var mu sync.Mutex
	stopped := false
	extend := func() bool {
		mu.Lock()
		defer mu.Unlock()
		if stopped {
			return false
		}
		c.SetReadDeadline(time.Now().Add(o.keepaliveTimeout))
		return true
	}
	stop := context.AfterFunc(ctx, func() {
		mu.Lock()
		defer mu.Unlock()
		stopped = true
		c.SetReadDeadline(unblockNow)
	})
	defer stop()

	c.SetPongHandler(func(string) error {
		extend()
		return nil
	})

	// Pings keep the deadline of an idle connection moving. A failed ping
	// needs no handling: the read deadline is the arbiter of death.
	done := make(chan struct{})
	defer close(done)
	go func() {
		t := time.NewTicker(o.keepaliveInterval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				c.WriteControl(websocket.PingMessage, nil, time.Now().Add(o.keepaliveInterval))
			}
		}
	}()

	for {
		if !extend() {
			return nil
		}
		typ, data, err := c.ReadMessage()
		if err != nil {
			switch {
			case ctx.Err() != nil,
				errors.Is(err, net.ErrClosed),
				websocket.IsCloseError(err,
					websocket.CloseNormalClosure,
					websocket.CloseGoingAway,
					websocket.CloseNoStatusReceived):
				return nil
			}
			return err
		}
		if typ != websocket.BinaryMessage {
			continue
		}
		e := &drpc.Envelop{}
		if err := proto.Unmarshal(data, e); err != nil {
			continue
		}
		drpc.Unpack(rxCtx, e, h)
	}
}

// Transport is the client-side endpoint: one WebSocket talking to one
// server. It is the tx handler for drpc.NewConn — implementing
// drpc.TransportInfo directly so mode discovery is not masked by a wrapper.
type Transport struct {
	s sock
	o options
}

// New wraps an established client WebSocket (e.g. websocket.Dialer.Dial).
func New(c *websocket.Conn, opts ...Option) *Transport {
	return &Transport{s: sock{c: c}, o: buildOptions(opts)}
}

// Reliable reports true: WebSocket neither loses, duplicates, nor reorders.
// The core discovers this and disables all protocol timers (PROTOCOL.md
// §10.6), which is what makes ServeConn's teardown duty mandatory.
func (t *Transport) Reliable() bool { return true }

// Handle sends one frame as a single-frame envelop.
func (t *Transport) Handle(ctx context.Context, f *drpc.Frame) error {
	e := &drpc.Envelop{}
	e.SetFrames([]*drpc.Frame{f})
	return t.Send(ctx, e)
}

// Send transmits one envelop as one binary message.
func (t *Transport) Send(_ context.Context, e *drpc.Envelop) error {
	data, err := marshal(e, t.o.maxMessageSize)
	if err != nil {
		return err
	}
	return t.s.write(data, t.o.keepaliveTimeout)
}

// ServeConn delivers received frames to conn and runs the keepalive until
// ctx is done or the socket dies. The single peer needs no peer key
// (PROTOCOL.md §6.4). On every exit it calls conn.Close: with protocol
// timers off, this teardown is the only mechanism that unblocks live calls
// (§4.5). Returns nil on ctx cancellation or a clean close, the transport
// error otherwise.
func (t *Transport) ServeConn(ctx context.Context, conn *drpc.Conn) error {
	err := serve(ctx, t.s.c, t.o, ctx, conn)
	conn.Close(err)
	return err
}

// peerKey identifies one registered WebSocket. Deliberately opaque and
// process-local: remote addresses collide behind proxies, so identity is a
// fresh counter per ServePeer.
type peerKey uint64

// Gateway is the server-side endpoint: one registered WebSocket per peer.
// It is the tx handler for drpc.NewServer — implementing drpc.TransportInfo
// directly so mode discovery is not masked by a wrapper.
type Gateway struct {
	o    options
	next atomic.Uint64

	mu    sync.Mutex
	peers map[peerKey]*sock
}

// NewGateway builds a Gateway; connections join it via ServePeer.
func NewGateway(opts ...Option) *Gateway {
	return &Gateway{o: buildOptions(opts), peers: map[peerKey]*sock{}}
}

// Reliable reports true: WebSocket neither loses, duplicates, nor reorders.
// The core discovers this and disables all protocol timers (PROTOCOL.md
// §10.6), which is what makes ServePeer's teardown duty mandatory.
func (g *Gateway) Reliable() bool { return true }

// Handle sends one frame as a single-frame envelop to the peer named in ctx.
func (g *Gateway) Handle(ctx context.Context, f *drpc.Frame) error {
	e := &drpc.Envelop{}
	e.SetFrames([]*drpc.Frame{f})
	return g.Send(ctx, e)
}

// Send transmits one envelop as one binary message to the peer named in ctx.
func (g *Gateway) Send(ctx context.Context, e *drpc.Envelop) error {
	key, ok := drpc.PeerFromContext(ctx)
	if !ok {
		return errors.New("ws: no peer in context")
	}
	k, ok := key.(peerKey)
	if !ok {
		return fmt.Errorf("ws: foreign peer key %T", key)
	}
	g.mu.Lock()
	s := g.peers[k]
	g.mu.Unlock()
	if s == nil {
		return fmt.Errorf("ws: peer %d is disconnected", k)
	}
	data, err := marshal(e, g.o.maxMessageSize)
	if err != nil {
		return err
	}
	return s.write(data, g.o.keepaliveTimeout)
}

// ServePeer registers c under a fresh peer key, delivers received frames to
// srv with the key attached (PROTOCOL.md §6.4), and runs the keepalive. It
// blocks until ctx is done or the socket dies, then deregisters the peer and
// calls srv.DisconnectPeer: with protocol timers off, this teardown is the
// only mechanism that unblocks the peer's live calls (§4.5). Returns nil on
// ctx cancellation or a clean close, the transport error otherwise.
func (g *Gateway) ServePeer(ctx context.Context, srv *drpc.Server, c *websocket.Conn) error {
	key := peerKey(g.next.Add(1))
	s := &sock{c: c}
	g.mu.Lock()
	g.peers[key] = s
	g.mu.Unlock()

	err := serve(ctx, c, g.o, drpc.NewPeerContext(ctx, key), srv)

	g.mu.Lock()
	delete(g.peers, key)
	g.mu.Unlock()
	srv.DisconnectPeer(key, err)
	return err
}
