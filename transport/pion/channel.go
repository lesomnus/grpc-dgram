package pion

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	drpc "github.com/lesomnus/grpc-dgram"
	"github.com/pion/webrtc/v4"
	"google.golang.org/protobuf/proto"
)

const (
	// DefaultMaxMessageSizeUnreliable keeps an envelop inside one SCTP packet
	// on the typical 1500-byte path MTU: a partially-reliable message that
	// SCTP fragments is lost whenever any one fragment is lost, multiplying
	// the effective loss rate (PROTOCOL.md §4.4).
	DefaultMaxMessageSizeUnreliable = 1200

	// DefaultMaxMessageSizeReliable is the classic DataChannel interop
	// ceiling: SCTP fragments and reassembles transparently, but 16 KiB is
	// the largest message every stack — browsers included — accepts without
	// end-of-record negotiation (PROTOCOL.md §4.4).
	DefaultMaxMessageSizeReliable = 16 * 1024
)

// DefaultMaxBufferedAmount bounds dc.BufferedAmount: pion queues outbound
// messages without limit, so sends block once this much is unacknowledged and
// resume on OnBufferedAmountLow (half the mark).
const DefaultMaxBufferedAmount = 1 << 20

// DefaultSendStallTimeout bounds how long one send may wait in total — for
// the channel to open and at the buffered-amount mark. In reliable mode the
// core runs no timers and does not bound the tx ctx, so the adapter must
// bound a stalled write itself (PROTOCOL.md §4.2): a channel that will not
// open or a peer that stops draining for this long is transport death, and
// the stall trips the same teardown as a channel error.
const DefaultSendStallTimeout = 30 * time.Second

// rxBufferSize bounds messages held between pion's read loop and the serve
// loop. It absorbs traffic arriving before the pump attaches; once
// full, OnMessage blocks the channel's read loop, which on a reliable channel
// is exactly SCTP flow control (PROTOCOL.md §4.2).
const rxBufferSize = 32

// channelReliable derives the protocol mode from the channel configuration —
// both ends observe the same parameters, negotiated or DCEP-announced.
// Ordered delivery with neither a retransmit cap nor a lifetime cap is full
// SCTP reliability: the core runs with every timer off (PROTOCOL.md §10.6).
// Any cap — even MaxRetransmits: 0 — or unordered delivery lets envelops
// vanish or arrive out of order: the loss profile the core's timer machinery
// exists for.
func channelReliable(dc *webrtc.DataChannel) bool {
	return dc.Ordered() && dc.MaxRetransmits() == nil && dc.MaxPacketLifeTime() == nil
}

// channel owns one DataChannel: it buffers inbound messages from the moment
// it is constructed (pion does not replay messages that arrive before a
// handler is registered) and gates outbound messages on channel open and on
// the buffered-amount mark.
// dcAddr names a DataChannel as a net.Addr, so peer.FromContext and
// grpc.Peer report something meaningful on a transport that has no socket
// address of its own: the channel's label and id.
type dcAddr struct{ label string }

func (a dcAddr) Network() string { return "webrtc" }
func (a dcAddr) String() string  { return "datachannel:" + a.label }

type channel struct {
	dc       *webrtc.DataChannel
	reliable bool
	max      int           // send limit in bytes; <= 0 is unlimited
	high     uint64        // BufferedAmount high-water mark; 0 disables blocking
	stall    time.Duration // max wait at the mark; <= 0 waits on ctx alone

	opened  chan struct{}
	dead    chan struct{}
	stopped chan struct{} // serve loop gone: drop instead of blocking pion
	rx      chan []byte

	openOnce sync.Once
	deadOnce sync.Once
	stopOnce sync.Once

	mu     sync.Mutex
	err    error         // first transport error observed, nil for clean close
	bufLow chan struct{} // broadcast: closed and replaced on OnBufferedAmountLow
}

func newChannel(dc *webrtc.DataChannel, reliable bool, o options) *channel {
	maxSize := DefaultMaxMessageSizeUnreliable
	if reliable {
		maxSize = DefaultMaxMessageSizeReliable
	}
	if o.maxMessageSize != nil {
		maxSize = *o.maxMessageSize
	}
	high := uint64(DefaultMaxBufferedAmount)
	if o.maxBufferedAmount != nil {
		high = *o.maxBufferedAmount
	}
	stall := time.Duration(DefaultSendStallTimeout)
	if o.sendStallTimeout != nil {
		stall = *o.sendStallTimeout
	}

	ch := &channel{
		dc:       dc,
		reliable: reliable,
		max:      maxSize,
		high:     high,
		stall:    stall,
		opened:   make(chan struct{}),
		dead:     make(chan struct{}),
		stopped:  make(chan struct{}),
		rx:       make(chan []byte, rxBufferSize),
		bufLow:   make(chan struct{}),
	}

	// pion invokes an OnOpen/OnClose handler immediately when the event
	// already happened, so registration order does not race the channel
	// state. OnError always precedes OnClose (both from the read loop).
	dc.OnOpen(func() { ch.openOnce.Do(func() { close(ch.opened) }) })
	dc.OnError(ch.fail)
	dc.OnClose(func() { ch.fail(nil) })
	if high > 0 {
		dc.SetBufferedAmountLowThreshold(high / 2)
		dc.OnBufferedAmountLow(ch.bufLowNotify)
	}
	dc.OnMessage(ch.onMessage)
	return ch
}

// fail records the first death cause and trips dead. Registered for both
// OnError and OnClose, whose handlers run on separate goroutines: a clean
// close may be observed before a racing error is recorded — the teardown is
// the same either way.
func (ch *channel) fail(err error) {
	ch.mu.Lock()
	if ch.err == nil {
		ch.err = err
	}
	ch.mu.Unlock()
	ch.deadOnce.Do(func() { close(ch.dead) })
}

func (ch *channel) deathErr() error {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	return ch.err
}

func (ch *channel) closedErr() error {
	if err := ch.deathErr(); err != nil {
		return fmt.Errorf("pion: data channel closed: %w", err)
	}
	return errors.New("pion: data channel closed")
}

// stop makes onMessage drop instead of block once no serve loop drains rx.
func (ch *channel) stop() {
	ch.stopOnce.Do(func() { close(ch.stopped) })
}

// onMessage runs on pion's per-channel read loop. Blocking here blocks that
// loop: on a reliable channel this is SCTP flow control propagating the
// core's backpressure (PROTOCOL.md §4.2); serve the channel promptly so the
// block ends.
func (ch *channel) onMessage(msg webrtc.DataChannelMessage) {
	select {
	case ch.rx <- msg.Data:
	case <-ch.stopped:
	case <-ch.dead:
	}
}

func (ch *channel) bufLowNotify() {
	ch.mu.Lock()
	close(ch.bufLow)
	ch.bufLow = make(chan struct{})
	ch.mu.Unlock()
}

func (ch *channel) bufLowWait() <-chan struct{} {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	return ch.bufLow
}

// send transmits one envelop as one channel message. It refuses an envelop
// over the size limit synchronously (PROTOCOL.md §4.4), waits for the channel
// to open, and blocks while BufferedAmount is at the high-water mark — each
// wait bounded by ctx and by channel death.
func (ch *channel) send(ctx context.Context, e *drpc.Envelop) error {
	data, err := proto.MarshalOptions{Deterministic: true}.Marshal(e)
	if err != nil {
		return err
	}
	if ch.max > 0 && len(data) > ch.max {
		return fmt.Errorf("pion: %d-byte envelop over the %d-byte limit: %w",
			len(data), ch.max, drpc.ErrMessageTooLarge)
	}

	// One stall budget bounds the whole send — the open-gate wait and the
	// buffered-amount wait: the core's abort path transmits with an
	// unbounded ctx (context.WithoutCancel), so ctx alone cannot be the
	// bound (PROTOCOL.md §4.2).
	var stalled <-chan time.Time
	if ch.stall > 0 {
		timer := time.NewTimer(ch.stall)
		defer timer.Stop()
		stalled = timer.C
	}

	select {
	case <-ch.opened:
	case <-stalled:
		err := fmt.Errorf("pion: send stalled: channel not open within %v", ch.stall)
		ch.fail(err)
		return err
	case <-ch.dead:
		return ch.closedErr()
	case <-ctx.Done():
		return ctx.Err()
	}
	for ch.high > 0 {
		// Acquire the broadcast channel before checking the amount: a
		// notification between the two is then observed, not missed.
		low := ch.bufLowWait()
		if ch.dc.BufferedAmount() < ch.high {
			break
		}
		select {
		case <-low:
		case <-stalled:
			// The peer stopped draining: transport death (PROTOCOL.md §4.2),
			// tripping the same teardown as a channel error.
			err := fmt.Errorf("pion: send stalled at the buffered-amount mark for %v", ch.stall)
			ch.fail(err)
			return err
		case <-ch.dead:
			return ch.closedErr()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return ch.dc.Send(data)
}

// serve pumps buffered messages into h until ctx is done or the channel
// dies; on death it flushes what was received first. err is the death cause
// (nil for a clean close or ctx cancellation). Exiting abandons the channel
// either way, so the caller owes the §4.5 teardown on every return.
func (ch *channel) serve(ctx context.Context, h drpc.FrameHandler) error {
	defer ch.stop()
	// Delivery ctx, cancelled on channel death: in reliable mode Handle may
	// block in backpressure (PROTOCOL.md §4.2), and while it does this loop
	// cannot observe ch.dead — death detected out-of-band (OnError/OnClose,
	// a send stall, Transport.Close) must cancel the ctx the blocked
	// delivery waits on, or the §4.5 teardown never runs. The death flush
	// below still delivers every frame that fits a stream buffer (the core
	// prefers delivery over a dead ctx); only deliveries that would have to
	// block fail their call instead.
	dctx, dcancel := context.WithCancel(ctx)
	defer dcancel()
	go func() {
		select {
		case <-ch.dead:
			dcancel()
		case <-dctx.Done():
		}
	}()
	for {
		select {
		case data := <-ch.rx:
			deliver(dctx, data, h)
		case <-ch.dead:
			for {
				select {
				case data := <-ch.rx:
					deliver(dctx, data, h)
				default:
					return ch.deathErr()
				}
			}
		case <-ctx.Done():
			return nil
		}
	}
}

// deliver unmarshals one message and hands its frames to h in order.
// Malformed messages are dropped — frame-level errors never tear down the
// channel (PROTOCOL.md §4.2). In reliable mode h may block, bounded by ctx;
// the bound rx queue then backs pressure up into pion's read loop.
func deliver(ctx context.Context, data []byte, h drpc.FrameHandler) {
	e := &drpc.Envelop{}
	if err := proto.Unmarshal(data, e); err != nil {
		return
	}
	drpc.Unpack(ctx, e, h)
}
