// Package pion runs drpc over pion WebRTC DataChannels: one channel message
// carries one marshaled Envelop, and the protocol mode is derived from the
// channel's own configuration — an ordered channel with no retransmit or
// lifetime cap is reliable, so the core runs with every timer off
// (PROTOCOL.md §10.6); any other configuration is unreliable and the full
// timer machinery is on. Same adapter, mode decided by the channel.
//
// The adapter takes an already-negotiated *webrtc.DataChannel; PeerConnection
// setup and signaling stay with the application.
//
// DataChannels are connection-oriented, so the §4.5 teardown duty applies:
// the attached client pump and ServePeer hook OnClose and call Conn.Close /
// Server.DisconnectPeer when the channel dies — in reliable mode the only
// unblocking mechanism. A PeerConnection can be severed without a close ever
// surfacing on the channel (the SCTP shutdown needs a live transport to
// travel over); applications should watch the PeerConnection state and close
// the channel — or the Conn/Server — themselves when it fails.
package pion

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	drpc "github.com/lesomnus/grpc-dgram"
	"github.com/pion/webrtc/v4"
)

type options struct {
	maxMessageSize    *int
	maxBufferedAmount *uint64
	sendStallTimeout  *time.Duration
	reliable          *bool
}

type Option func(*options)

// WithMaxMessageSize sets the largest marshaled Envelop this endpoint will
// send, in bytes; 0 removes the limit. It bounds sends only; receives accept
// any message. Unset, the limit follows the channel mode:
// DefaultMaxMessageSizeUnreliable or DefaultMaxMessageSizeReliable.
func WithMaxMessageSize(n int) Option {
	return func(o *options) { o.maxMessageSize = &n }
}

// WithMaxBufferedAmount sets the outbound high-water mark, in bytes: sends
// block while dc.BufferedAmount is at or above it (pion itself queues without
// limit); 0 never blocks. Default DefaultMaxBufferedAmount.
func WithMaxBufferedAmount(n uint64) Option {
	return func(o *options) { o.maxBufferedAmount = &n }
}

// WithSendStallTimeout bounds how long one send may wait in total — for the
// channel to open and at the buffered-amount mark — before the channel is
// declared dead (PROTOCOL.md §4.2); 0 waits on ctx alone. Default
// DefaultSendStallTimeout.
func WithSendStallTimeout(d time.Duration) Option {
	return func(o *options) { o.sendStallTimeout = &d }
}

// WithReliable declares the mode of every channel a Gateway will serve, for
// deployments where the server is built before the first channel arrives
// (see Gateway.Reliable). With true, ServePeer refuses unreliable channels —
// a lossy channel cannot run with timers off. New ignores this option: a
// Transport always derives the mode from its own channel.
func WithReliable(v bool) Option {
	return func(o *options) { o.reliable = &v }
}

func buildOptions(opts []Option) options {
	o := options{}
	for _, f := range opts {
		f(&o)
	}
	return o
}

// Transport is the client-side endpoint: one DataChannel talking to one
// server, so no peer key is needed (PROTOCOL.md §6.4). It is the tx handler
// for drpc.NewConn — implementing drpc.TransportInfo and drpc.ConnAttacher
// directly so neither is masked by a wrapper. drpc.NewConn attaches it and
// the drain pump starts by itself: no user goroutine, and conn.Close (or
// Close here) tears everything down, DataChannel included.
type Transport struct {
	ch       *channel
	attached atomic.Bool
	closer   sync.Once
}

// New wraps an already-negotiated DataChannel. It registers the channel
// handlers immediately and buffers inbound messages until the attached pump
// drains them — pion drops messages that arrive with no handler registered.
// For a remotely-announced channel, call New synchronously inside
// OnDataChannel: pion holds the channel's read loop until that callback
// returns, so handlers registered there observe every message, while a
// handler registered from a spawned goroutine races the read loop. The
// Transport owns the channel from here on: Close closes it, and closing the
// attached Conn does too.
func New(dc *webrtc.DataChannel, opts ...Option) *Transport {
	o := buildOptions(opts)
	return &Transport{ch: newChannel(dc, channelReliable(dc), o)}
}

// AttachConn is called by drpc.NewConn: it starts the drain pump, which runs
// until the channel dies or Close is called, then performs the §4.5
// teardown — conn.Close with the cause — the only mechanism that unblocks
// live calls in reliable mode. Attach (i.e. drpc.NewConn) promptly after
// New: past rxBufferSize buffered messages the channel's read loop blocks
// waiting for the pump.
func (t *Transport) AttachConn(conn *drpc.Conn) {
	if !t.attached.CompareAndSwap(false, true) {
		panic("pion: transport already attached to a Conn")
	}
	go func() {
		_, err := t.ch.serve(context.Background(), conn)
		conn.Close(err)
	}()
}

// Close closes the DataChannel; its death path flushes what was already
// received, stops the pump, and fails any live calls. The death latch is
// tripped directly rather than through pion: a channel whose association
// never established (a failed dial) never fires OnClose/OnError, and the
// teardown must not depend on it. Idempotent.
func (t *Transport) Close() error {
	t.closer.Do(func() {
		t.ch.fail(nil)
		t.ch.dc.Close()
	})
	return nil
}

// Reliable reports the mode derived from the channel configuration; see
// channelReliable. drpc.NewConn reads it once at construction.
func (t *Transport) Reliable() bool { return t.ch.reliable }

// Handle sends one frame as a single-frame envelop.
func (t *Transport) Handle(ctx context.Context, f *drpc.Frame) error {
	e := &drpc.Envelop{}
	e.SetFrames([]*drpc.Frame{f})
	return t.Send(ctx, e)
}

// Send transmits one envelop as one channel message. It waits for the channel
// to open and applies backpressure per WithMaxBufferedAmount, both bounded by
// ctx; an envelop over the size limit is refused synchronously with an error
// wrapping drpc.ErrMessageTooLarge (PROTOCOL.md §4.4).
func (t *Transport) Send(ctx context.Context, e *drpc.Envelop) error {
	return t.ch.send(ctx, e)
}

// peerKey identifies one served channel within a Gateway. Keys are never
// reused, so a peer that reconnects on a fresh channel is a fresh peer.
type peerKey uint64

// Gateway is the server-side endpoint: one drpc.Server serving many peers,
// one DataChannel each. It is the tx handler for drpc.NewServer.
type Gateway struct {
	o    options
	next atomic.Uint64

	mu    sync.Mutex
	mode  *bool // latched channel mode; nil until first decided
	chans map[*webrtc.DataChannel]*gwChannel
	peers map[peerKey]*channel
}

type gwChannel struct {
	ch     *channel
	key    peerKey
	ok     bool // channel config matches the gateway mode
	served atomic.Bool
}

func NewGateway(opts ...Option) *Gateway {
	o := buildOptions(opts)
	return &Gateway{
		o:     o,
		mode:  o.reliable,
		chans: map[*webrtc.DataChannel]*gwChannel{},
		peers: map[peerKey]*channel{},
	}
}

// Reliable reports the mode of the channels this gateway serves.
// drpc.NewServer reads it once at construction — usually before any channel
// exists — so the gateway cannot derive the mode from traffic it has not
// seen: the mode is WithReliable if given, else the configuration of the
// first channel passed to Bind or ServePeer, else unreliable; it latches at
// whichever of Reliable, Bind, or ServePeer runs first. The unreliable
// machinery is correct on any channel (timers on a reliable channel are
// merely redundant), so an unreliable-latched gateway serves reliable
// channels too; the reverse is refused — timers off over a lossy channel
// would hang — so ServePeer rejects an unreliable channel once the gateway
// latched reliable. A server built before its first channel therefore runs
// unreliable and serves everything, unless WithReliable(true) says
// otherwise.
func (g *Gateway) Reliable() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.latchLocked(false)
}

func (g *Gateway) latchLocked(v bool) bool {
	if g.mode == nil {
		g.mode = &v
	}
	return *g.mode
}

// Bind registers the gateway's handlers on dc and starts buffering its
// inbound messages; it is idempotent per channel. For a remotely-announced
// channel it MUST run synchronously inside OnDataChannel — pion holds the
// channel's read loop until that callback returns, so handlers registered
// there observe every message, while ServePeer spawned from the callback
// races it. ServePeer, which must not run inside OnDataChannel (it blocks,
// and with it pion's accept loop), binds implicitly for channels created
// locally:
//
//	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
//		gw.Bind(dc)
//		go gw.ServePeer(ctx, srv, dc)
//	})
func (g *Gateway) Bind(dc *webrtc.DataChannel) { g.bind(dc) }

func (g *Gateway) bind(dc *webrtc.DataChannel) *gwChannel {
	g.mu.Lock()
	defer g.mu.Unlock()
	if b, ok := g.chans[dc]; ok {
		return b
	}
	r := channelReliable(dc)
	m := g.latchLocked(r)
	b := &gwChannel{
		ch:  newChannel(dc, m, g.o),
		key: peerKey(g.next.Add(1)),
		// Only one combination is unservable: a lossy channel under a server
		// whose timers are off. A reliable channel under an unreliable-latched
		// gateway merely runs redundant timers (see Reliable).
		ok: r || !m,
	}
	g.chans[dc] = b
	g.peers[b.key] = b.ch
	return b
}

func (g *Gateway) drop(dc *webrtc.DataChannel, b *gwChannel) {
	// Trip the death latch ourselves: gated sends must unblock even when
	// pion never fires OnClose (never-established association).
	b.ch.fail(nil)
	b.ch.stop()
	g.mu.Lock()
	delete(g.chans, dc)
	delete(g.peers, b.key)
	g.mu.Unlock()
}

// ServePeer delivers dc's frames to srv under a fresh peer key until ctx is
// done or the channel dies. On channel death it performs the §4.5 teardown
// duty — srv.DisconnectPeer with the cause — deregisters the peer, and
// returns that cause; it returns nil on a clean close or on ctx cancellation.
// It refuses a channel whose configuration contradicts the gateway mode (see
// Reliable) and serves each channel at most once.
func (g *Gateway) ServePeer(ctx context.Context, srv *drpc.Server, dc *webrtc.DataChannel) error {
	b := g.bind(dc)
	if !b.ok {
		g.drop(dc, b)
		return fmt.Errorf("pion: %s channel on a gateway locked %s",
			modeName(channelReliable(dc)), modeName(g.Reliable()))
	}
	if !b.served.CompareAndSwap(false, true) {
		return errors.New("pion: channel already served")
	}
	defer g.drop(dc, b)

	died, err := b.ch.serve(drpc.NewPeerContext(ctx, b.key), srv)
	if died {
		srv.DisconnectPeer(b.key, err)
	}
	return err
}

// Handle sends one frame as a single-frame envelop to the peer named in ctx.
func (g *Gateway) Handle(ctx context.Context, f *drpc.Frame) error {
	e := &drpc.Envelop{}
	e.SetFrames([]*drpc.Frame{f})
	return g.Send(ctx, e)
}

// Send transmits one envelop as one channel message to the peer named in
// ctx, with the same gating as Transport.Send.
func (g *Gateway) Send(ctx context.Context, e *drpc.Envelop) error {
	key, ok := drpc.PeerFromContext(ctx)
	if !ok {
		return errors.New("pion: no peer in context")
	}
	k, ok := key.(peerKey)
	if !ok {
		return fmt.Errorf("pion: foreign peer key %T", key)
	}
	g.mu.Lock()
	ch := g.peers[k]
	g.mu.Unlock()
	if ch == nil {
		return fmt.Errorf("pion: peer %d is gone", k)
	}
	return ch.send(ctx, e)
}

func modeName(reliable bool) string {
	if reliable {
		return "reliable"
	}
	return "unreliable"
}
