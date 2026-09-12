package udp_test

// A user-written Batcher, pinned.
//
// docs/TODO.md §1 says the batching policy belongs to the workload and the
// library's job is to provide the seam — nothing in this file is added to the
// library, and nothing in the library changes for it to compile. This test is
// the executable half of that claim: it builds a Batcher out of what is
// exported today (drpc.FrameHandler in, Transport.Send out) and pins the four
// properties the documentation rests on.
//
//  1. Embedding the adapter keeps every interface drpc.NewConn discovers
//     visible. A field-wrapper would hide them, and the worst failure is
//     silent (see TestBatcherKeepsTheDiscoveredInterfaces).
//  2. A k-frame Envelope the Batcher builds leaves as ONE datagram and arrives
//     as k frames, in order, through drpc.Unpack — the receive side has taken
//     1..n frames per datagram all along (PROTOCOL.md §4.1).
//  3. The §4.4 duty survives the Batcher: a frame that cannot fit the
//     transport's budget fails synchronously out of Handle, while a call still
//     owns it. Deferring that check to the flush does not merely lose the
//     error, it delivers it to the wrong call, and the core acts on it.
//  4. The buffer is serialised: the core calls Handle from many goroutines at
//     once (PROTOCOL.md §4.1), so the state a Batcher introduces at this seam
//     needs a lock the shipped adapters never needed.
//
// One duty this file cannot show is the one that only exists above a GATEWAY:
// the datagram's destination is read out of the ctx of the call that flushes
// it (udp.Gateway.Send -> drpc.PeerFromContext), while a server hands its tx a
// different per-peer ctx per peer. A batcher there may pack only frames whose
// ctx names the same peer. The Batcher below rides a connected socket, which
// has exactly one destination, so the question does not arise for it.
//
// The policy used here — flush every flushAt frames — is a stand-in, NOT a
// recommendation. It delays frames, which docs/batching-measurement.md argues
// no general-purpose default should do; it is merely the cheapest policy that
// makes a multi-frame datagram deterministic in a test. What a real Batcher
// does with fate-sharing (§4.1), a latency budget (§10.7) and its own frame
// mix is the workload's answer, not this file's.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	drpc "github.com/lesomnus/grpc-dgram"
	"github.com/lesomnus/grpc-dgram/internal/echo"
	"github.com/lesomnus/grpc-dgram/transport/udp"
	"google.golang.org/protobuf/proto"
)

// batcher is what a user writes: a FrameHandler that collects frames and hands
// them to the adapter as one Envelope.
//
// The adapter is EMBEDDED, not held in a field. Embedding promotes
// AttachConn, Close, Reliable and Peer, which is exactly the set drpc.NewConn
// discovers by type assertion; a field would hide all four and the Batcher
// would still compile and still satisfy drpc.FrameHandler (PROTOCOL.md §3
// and Appendix C put the duty normatively).
type batcher struct {
	*udp.Transport

	// max is the adapter's own send budget, not a second one: newBatcher
	// hands the same number to udp.WithMaxMessageSize. The Batcher sits below
	// the core and above the socket, so it is the last place that can weigh a
	// frame against that budget while the owning call is still on the stack.
	// A budget ABOVE the adapter's would be the bug PROTOCOL.md §4.4 names:
	// the overrun moves to the flush, where the error comes back out of some
	// other call's Handle and the core rewinds THAT call's seq and credit.
	max     int
	flushAt int

	// mu is mandatory, not decoration. The core calls Handle from every call
	// goroutine, from the retransmission/keepalive sweep and from the WINDOW
	// grant paths; the shipped adapters keep no state below Handle, and a
	// Batcher is the first thing at this seam that does (§4.1).
	mu      sync.Mutex
	pending []*drpc.Frame

	datagrams atomic.Int64 // envelopes actually handed to the adapter
}

func newBatcher(c net.Conn, flushAt int) *batcher {
	const max = udp.DefaultMaxMessageSize
	return &batcher{
		Transport: udp.New(c, udp.WithMaxMessageSize(max)),
		max:       max,
		flushAt:   flushAt,
	}
}

// envelopeSize is the marshaled length of the datagram these frames would make.
func envelopeSize(frames ...*drpc.Frame) int {
	e := &drpc.Envelope{}
	e.SetFrames(frames)
	return proto.Size(e)
}

// Handle is the only method the Batcher overrides.
func (b *batcher) Handle(ctx context.Context, f *drpc.Frame) error {
	// PROTOCOL.md §4.4: the core never fragments, and a message that cannot
	// fit the channel must fail the call that owns it. Handle is the last
	// moment at which that call is reachable, and deferring the check to the
	// flush does not merely lose the error: the flush runs under whichever
	// OTHER call's Handle happened to trigger it, so that call gets an
	// ErrMessageTooLarge for a frame that fit, and the core believes it —
	// undoRefused reclaims that call's seq and refunds its credit while the
	// frame that really overran is dropped in silence. So the budget check
	// happens here, synchronously, and b.max is the adapter's own budget.
	if n := envelopeSize(f); n > b.max {
		return fmt.Errorf("batcher: %d-byte frame over the %d-byte budget: %w", n, b.max, drpc.ErrMessageTooLarge)
	}

	b.mu.Lock()
	var batch []*drpc.Frame
	switch {
	case len(b.pending) > 0 && envelopeSize(append(append([]*drpc.Frame{}, b.pending...), f)...) > b.max:
		// f does not fit alongside what is already waiting: what is waiting
		// goes now, in order, and f opens the next envelope.
		batch, b.pending = b.pending, []*drpc.Frame{f}
	default:
		b.pending = append(b.pending, f)
		if len(b.pending) >= b.flushAt {
			batch, b.pending = b.pending, nil
		}
	}
	b.mu.Unlock()

	if batch == nil {
		return nil
	}
	e := &drpc.Envelope{}
	e.SetFrames(batch)
	b.datagrams.Add(1)
	// Transport.Send is the exported envelope-level seam (drpc.EnvelopeHandler,
	// frame.go): one Envelope of 1..n frames, one datagram.
	return b.Transport.Send(ctx, e)
}

func (b *batcher) pendingLen() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.pending)
}

// dataFrame is a plausible client data frame: the shape of what the core hands
// a tx, which is all the Batcher ever sees.
func dataFrame(sid uint32, seq uint32, payload []byte) *drpc.Frame {
	f := &drpc.Frame{}
	f.SetEpoch(0x0BADCAFE)
	f.SetSid(sid)
	f.SetSeq(seq)
	f.SetPayload(payload)
	return f
}

// dialBatcher gives back a Batcher over a connected socket and the socket that
// plays the peer, so a test can read what actually left.
func dialBatcher(t *testing.T, flushAt int) (*batcher, net.PacketConn) {
	t.Helper()

	peer, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { peer.Close() })

	c, err := net.Dial("udp", peer.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	b := newBatcher(c, flushAt)
	t.Cleanup(func() { b.Close() })
	return b, peer
}

// fieldBatcher is the same Batcher written the tempting way: the adapter in a
// field instead of embedded. It satisfies drpc.FrameHandler, so it compiles
// and NewConn accepts it — and it is missing everything NewConn discovers.
// It exists only to keep the contrast in TestBatcherKeepsTheDiscoveredInterfaces
// honest.
type fieldBatcher struct {
	tx *udp.Transport
}

func (b *fieldBatcher) Handle(ctx context.Context, f *drpc.Frame) error {
	e := &drpc.Envelope{}
	e.SetFrames([]*drpc.Frame{f})
	return b.tx.Send(ctx, e)
}

// TestBatcherKeepsTheDiscoveredInterfaces pins the reason a Batcher must embed
// the adapter. drpc.NewConn does not ask for these; it type-asserts the tx it
// is handed, so a Batcher that hides one loses the behaviour with no error at
// all, at construction or afterwards.
func TestBatcherKeepsTheDiscoveredInterfaces(t *testing.T) {
	b, _ := dialBatcher(t, 2)

	tx := any(b)
	// The seam itself (conn.go, server.go): without it the Batcher is not a
	// transport and NewConn will not take it — the one failure here that is
	// loud, because it is a compile error at the call site.
	if _, ok := tx.(drpc.FrameHandler); !ok {
		t.Error("drpc.FrameHandler: the Batcher is not a transport at all")
	}
	// conn.go:146. AttachConn is what starts the adapter's receive pump. Hide
	// it and NewConn simply never calls it: the endpoint sends fine and
	// receives NOTHING, forever, with no error anywhere — every call dies of
	// its deadline instead. This is the silent failure the embedding exists
	// to prevent.
	if _, ok := tx.(drpc.ConnAttacher); !ok {
		t.Error("drpc.ConnAttacher: the receive pump would never start and the endpoint would receive nothing, silently")
	}
	// conn.go:385. Conn.Close closes a tx that is an io.Closer. Hide it and
	// conn.Close(nil) leaves the socket open and the pump goroutine alive —
	// a leak per connection.
	if _, ok := tx.(io.Closer); !ok {
		t.Error("io.Closer: conn.Close would leak the socket and the receive goroutine")
	}
	// timing.go:98 (resolveMode). Reliable() is how the mode is discovered.
	// Hide it and this UDP endpoint is taken for the default — unreliable —
	// which happens to be right here but is wrong for any reliable adapter
	// wrapped the same way: no timers, no retransmission, over a channel that
	// loses.
	if _, ok := tx.(drpc.TransportInfo); !ok {
		t.Error("drpc.TransportInfo: the transport mode would be guessed instead of discovered")
	}
	// conn.go:106. Peer() names the remote end for grpc.Peer and
	// peer.FromContext. Hide it and those go empty (§6.4).
	if _, ok := tx.(drpc.TransportPeer); !ok {
		t.Error("drpc.TransportPeer: grpc.Peer and peer.FromContext would name nothing")
	}

	// The contrast, so none of the above is a claim about a hypothetical: the
	// same Batcher with the adapter in a FIELD keeps only the seam it declares
	// itself, and loses the other four without a word from the compiler or
	// from NewConn.
	field := any(&fieldBatcher{tx: b.Transport})
	if _, ok := field.(drpc.FrameHandler); !ok {
		t.Error("a field-wrapper should still be a drpc.FrameHandler")
	}
	for _, c := range []struct {
		name string
		ok   bool
	}{
		{"drpc.ConnAttacher", func() bool { _, ok := field.(drpc.ConnAttacher); return ok }()},
		{"io.Closer", func() bool { _, ok := field.(io.Closer); return ok }()},
		{"drpc.TransportInfo", func() bool { _, ok := field.(drpc.TransportInfo); return ok }()},
		{"drpc.TransportPeer", func() bool { _, ok := field.(drpc.TransportPeer); return ok }()},
	} {
		if c.ok {
			t.Errorf("a field-wrapper unexpectedly exposes %s; the embedding above would then be optional", c.name)
		}
	}
}

// TestBatcherSendsOneDatagramPerBatch is the property the whole argument rests
// on: k frames the Batcher collects reach the wire as one datagram, and the
// receive side — unchanged — takes all k out of it in order. The saving
// measured in docs/batching-measurement.md is exactly the k−1 writes and the
// k−1 IP/UDP headers this avoids.
func TestBatcherSendsOneDatagramPerBatch(t *testing.T) {
	const k = 3
	b, peer := dialBatcher(t, k)

	for i := uint32(1); i <= k; i++ {
		if err := b.Handle(t.Context(), dataFrame(i, i*10, []byte{byte(i)})); err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
	}
	if got := b.datagrams.Load(); got != 1 {
		t.Fatalf("envelopes sent: got %d, want 1", got)
	}

	buf := make([]byte, 2048)
	if err := peer.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	n, _, err := peer.ReadFrom(buf)
	if err != nil {
		t.Fatal(err)
	}
	e := &drpc.Envelope{}
	if err := proto.Unmarshal(buf[:n], e); err != nil {
		t.Fatalf("the datagram is not an Envelope: %v", err)
	}

	// drpc.Unpack is what an adapter runs on the receive path; driving it here
	// is the proof that a batched datagram needs no receive-side change.
	var got [][2]uint32
	if err := drpc.Unpack(t.Context(), e, drpc.FrameHandlerFunc(func(_ context.Context, f *drpc.Frame) error {
		got = append(got, [2]uint32{f.GetSid(), f.GetSeq()})
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	want := [][2]uint32{{1, 10}, {2, 20}, {3, 30}}
	if len(got) != len(want) {
		t.Fatalf("frames delivered from one %d-byte datagram: got %d, want %d", n, len(got), len(want))
	}
	for i := range want {
		// Order is not decoration: within one envelope the frames of a stream
		// are delivered in the order they were packed (§4.1).
		if got[i] != want[i] {
			t.Fatalf("frame %d: got sid/seq %v, want %v", i, got[i], want[i])
		}
	}
	t.Logf("one %d-byte datagram carried %d frames", n, len(got))

	// And nothing else left: the k frames cost one write, not k.
	if err := peer.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if n, _, err := peer.ReadFrom(buf); err == nil {
		t.Fatalf("a second datagram of %d bytes was sent; the batch should have been one", n)
	}
}

// TestBatcherFailsAnOversizeFrameSynchronously pins the §4.4 duty through the
// Batcher. The core maps drpc.ErrMessageTooLarge to ResourceExhausted on the
// call that produced the frame, which only works while that call is still
// waiting on Handle — so the check cannot be deferred to the flush.
func TestBatcherFailsAnOversizeFrameSynchronously(t *testing.T) {
	b, peer := dialBatcher(t, 2)

	huge := dataFrame(1, 1, make([]byte, 2*udp.DefaultMaxMessageSize))
	err := b.Handle(t.Context(), huge)
	if !errors.Is(err, drpc.ErrMessageTooLarge) {
		t.Fatalf("got %v, want an error wrapping drpc.ErrMessageTooLarge", err)
	}
	// Failed, not buffered: a frame parked for a later flush would take the
	// error away from its owning call and turn a ResourceExhausted into a
	// call that hangs until its deadline.
	if got := b.pendingLen(); got != 0 {
		t.Fatalf("frames left pending after the refusal: got %d, want 0", got)
	}
	if got := b.datagrams.Load(); got != 0 {
		t.Fatalf("envelopes sent: got %d, want 0", got)
	}

	// A frame that does fit still flows: the refusal is per message, and the
	// transport is not broken by it (§4.4).
	for i := uint32(1); i <= 2; i++ {
		if err := b.Handle(t.Context(), dataFrame(i, i, []byte("ok"))); err != nil {
			t.Fatal(err)
		}
	}
	buf := make([]byte, 2048)
	if err := peer.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	n, _, err := peer.ReadFrom(buf)
	if err != nil {
		t.Fatal(err)
	}
	e := &drpc.Envelope{}
	if err := proto.Unmarshal(buf[:n], e); err != nil {
		t.Fatal(err)
	}
	if got := len(e.GetFrames()); got != 2 {
		t.Fatalf("frames in the datagram after the refusal: got %d, want 2", got)
	}
}

// TestBatcherSerialisesConcurrentHandles pins the duty a single-goroutine test
// cannot see. The core transmits from every call's goroutine, from the
// retransmission/keepalive sweep and from the WINDOW grant paths, so the
// buffer a Batcher introduces is shared mutable state — the one thing no
// shipped adapter has below Handle. Without b.mu this test loses or duplicates
// buffered frames, and reports a data race under -race.
func TestBatcherSerialisesConcurrentHandles(t *testing.T) {
	const (
		flushAt = 4
		senders = 32 // a multiple of flushAt, so no partial batch is left over
	)
	b, peer := dialBatcher(t, flushAt)
	ctx := t.Context()

	var wg sync.WaitGroup
	for i := uint32(1); i <= senders; i++ {
		wg.Go(func() {
			if err := b.Handle(ctx, dataFrame(i, i, []byte("x"))); err != nil {
				t.Errorf("frame %d: %v", i, err)
			}
		})
	}
	wg.Wait()

	if got := b.pendingLen(); got != 0 {
		t.Fatalf("frames left pending: got %d, want 0", got)
	}
	if got, want := b.datagrams.Load(), int64(senders/flushAt); got != want {
		t.Fatalf("datagrams sent: got %d, want %d", got, want)
	}

	// Every frame handed in left exactly once: none lost to a torn append,
	// none packed into two envelopes.
	seen := map[uint32]int{}
	buf := make([]byte, 2048)
	for range senders / flushAt {
		if err := peer.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
			t.Fatal(err)
		}
		n, _, err := peer.ReadFrom(buf)
		if err != nil {
			t.Fatal(err)
		}
		e := &drpc.Envelope{}
		if err := proto.Unmarshal(buf[:n], e); err != nil {
			t.Fatalf("the datagram is not an Envelope: %v", err)
		}
		for _, f := range e.GetFrames() {
			seen[f.GetSid()]++
		}
	}
	for i := uint32(1); i <= senders; i++ {
		if seen[i] != 1 {
			t.Errorf("frame sid=%d arrived %d times, want 1", i, seen[i])
		}
	}
}

// TestBatcherCarriesRealCalls runs real RPCs over a Conn whose transport is
// the Batcher — the shape a user actually gets. It is what proves the promoted
// AttachConn is not merely present but effective: if it were hidden, no
// response would ever be read and every sub-call below would die of its
// deadline.
//
// flushAt is 1 here, so the Batcher adds no delay of its own: a Batcher that
// waits for a partner needs a second frame to exist, and inventing one is a
// policy question the library refuses to answer for the user. The multi-frame
// datagram itself is pinned above, on the wire, where it can be checked
// deterministically.
func TestBatcherCarriesRealCalls(t *testing.T) {
	addr := serveEcho(t)

	c, err := net.Dial("udp", addr.addr)
	if err != nil {
		t.Fatal(err)
	}
	b := newBatcher(c, 1)
	conn := drpc.NewConn(b, drpc.WithTiming(timing))
	t.Cleanup(func() { conn.Close(nil) })
	client := echo.NewEchoServiceClient(conn)

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

	stream, err := client.Many(t.Context(), echo.EchoRequest_builder{
		Message:       "abc",
		CircularShift: 1,
		Repeat:        3,
	}.Build())
	if err != nil {
		t.Fatal(err)
	}
	for {
		_, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}

	// Every frame this endpoint sent went out through the Batcher.
	if got := b.datagrams.Load(); got == 0 {
		t.Fatal("the Batcher sent nothing; the calls cannot have gone through it")
	}
}
