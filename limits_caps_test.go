package drpc_test

// limits_caps_test.go pins the §15 resource bounds behaviorally, driving a
// Server directly with crafted frames (PROTOCOL.md §15, §9.2, §9.4, §10.4):
//
//   - the tombstone ENTRY cap evicts the lowest sid and raises the container
//     floor — dedup survives at zero memory, only the replay is lost (§9.2,
//     §14 "no window in between");
//   - the tombstone BYTE cap degrades the oldest stored terminals to
//     key-only (§9.2);
//   - the live-call cap is per TRANSPORT PEER, counted across client epochs,
//     and its rejection is a tombstone-stored T{RESOURCE_EXHAUSTED} (§9.4,
//     §15);
//   - the per-peer aggregate reply budget (Limits.MaxRepliesPerRTI) caps all
//     volunteered replies within one RTI, on top of the per-object 1/RTI
//     limits (§15);
//   - the dead-container cap never evicts a container with live calls, and
//     the eviction residual (§14, §16 L2) is exactly re-execution;
//   - liveness expiry cancels handlers, suppresses the terminal, and leaves
//     a key-only tombstone (§10.4).

import (
	"bytes"
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	drpc "github.com/lesomnus/grpc-dgram"
	"github.com/lesomnus/grpc-dgram/internal/echo"
	"github.com/lesomnus/grpc-dgram/internal/x"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// lcServer is an unreliable-mode inject server (fastTiming) whose tx frames
// are captured with the peer attached per frame, so per-transport-peer caps
// are exercisable. Keepalives and stream probes (FlagPing) are cadence noise
// for every test here (§10.4, §10.5) and are filtered at capture.
type lcServer struct {
	srv *drpc.Server
	out chan *drpc.Frame
}

func newLcServer(t *testing.T, opts ...drpc.ServerOption) *lcServer {
	ls := &lcServer{out: make(chan *drpc.Frame, 256)}
	tx := drpc.FrameHandlerFunc(func(_ context.Context, f *drpc.Frame) error {
		if f.GetFlags()&drpc.FlagPing != 0 {
			return nil
		}
		ls.out <- proto.CloneOf(f)
		return nil
	})
	ls.srv = drpc.NewServer(tx, append([]drpc.ServerOption{
		drpc.WithReliable(false),
		drpc.WithTiming(fastTiming),
	}, opts...)...)
	echo.RegisterEchoServiceServer(ls.srv, &echo.EchoServer{})
	t.Cleanup(ls.srv.Stop)
	return ls
}

// handleAs injects f as if it arrived from the given transport peer.
func (ls *lcServer) handleAs(peer any, f *drpc.Frame) {
	_ = ls.srv.Handle(drpc.NewPeerContext(context.Background(), peer), f)
}

// recv returns the next captured frame within a real-time window. NOT for
// use inside a synctest bubble (use drain + synctest.Wait there).
func (ls *lcServer) recv(t *testing.T) *drpc.Frame {
	t.Helper()
	select {
	case f := <-ls.out:
		return f
	case <-time.After(500 * time.Millisecond):
		return nil
	}
}

// expectNone asserts silence over a real-time window spanning several RTIs
// (fastTiming RTI = 50ms). NOT for use inside a bubble.
func (ls *lcServer) expectNone(t *testing.T, msg string) {
	t.Helper()
	select {
	case f := <-ls.out:
		t.Fatalf("expected silence (%s), got: %v", msg, f)
	case <-time.After(250 * time.Millisecond):
	}
}

// drain empties the capture buffer without blocking.
func (ls *lcServer) drain() []*drpc.Frame {
	var fs []*drpc.Frame
	for {
		select {
		case f := <-ls.out:
			fs = append(fs, f)
		default:
			return fs
		}
	}
}

// lcOnceOpen builds a valid unary OPEN|CLOSE (seq 1, method, marshaled
// request — the wire shape of PROTOCOL.md §8).
func lcOnceOpen(epoch, sid uint32, msg string) *drpc.Frame {
	f := &drpc.Frame{}
	f.SetEpoch(epoch)
	f.SetSid(sid)
	f.SetSeq(1)
	f.SetFlags(drpc.FlagOpen | drpc.FlagClose)
	f.SetMethod(echo.EchoService_Once_FullMethodName)
	data, _ := proto.Marshal(echo.EchoRequest_builder{Message: msg}.Build())
	f.SetPayload(data)
	return f
}

// lcLiveOpen builds the eager, bare OPEN of a bidi call (§8). The Live
// handler blocks in Recv: the call stays live until torn down.
func lcLiveOpen(epoch, sid uint32) *drpc.Frame {
	f := &drpc.Frame{}
	f.SetEpoch(epoch)
	f.SetSid(sid)
	f.SetSeq(1)
	f.SetFlags(drpc.FlagOpen)
	f.SetMethod(echo.EchoService_Live_FullMethodName)
	return f
}

// lcData builds a data frame (flags 0, payload present).
func lcData(epoch, sid, seq uint32, payload []byte) *drpc.Frame {
	f := &drpc.Frame{}
	f.SetEpoch(epoch)
	f.SetSid(sid)
	f.SetSeq(seq)
	if payload == nil {
		payload = []byte{}
	}
	f.SetPayload(payload)
	return f
}

// lcUnaryCall runs one unary call to completion and returns its terminal T.
func lcUnaryCall(t *testing.T, ls *lcServer, peer any, epoch, sid uint32, msg string) *drpc.Frame {
	t.Helper()
	ls.handleAs(peer, lcOnceOpen(epoch, sid, msg))
	f := ls.recv(t)
	x.True(t, f != nil, "expected a terminal for sid ", sid)
	x.Equal(t, drpc.FlagClose, f.GetFlags())
	x.Equal(t, codes.OK, codes.Code(f.GetCode()))
	x.Equal(t, sid, f.GetSid())
	return f
}

// lcCountStreamExecs counts streaming-handler executions, the stream twin of
// countExecs (timeout_test.go).
func lcCountStreamExecs(n *atomic.Int32) drpc.ServerOption {
	return drpc.StreamInterceptor(func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		n.Add(1)
		return handler(srv, ss)
	})
}

// ---------------------------------------------------------------------------
// §9.2 / §15 entry cap: past MaxTombstones the lowest sid is evicted and the
// container floor rises. The evicted sid keeps KEY-ONLY semantics at zero
// memory: a duplicate OPEN is swallowed — never re-executed (§14 "no window
// in between"), never answered (the replay is what eviction costs).
// ---------------------------------------------------------------------------

func TestLimits_TombstoneEntryCapFloor(t *testing.T) {
	var execs atomic.Int32
	ls := newLcServer(t, drpc.WithLimits(drpc.Limits{MaxTombstones: 4}), countExecs(&execs))
	const peer = "peer-floor"
	const epoch uint32 = 0x1A

	// Six completed unary calls against a 4-entry cap: adding sids 5 and 6
	// evicts the lowest sids 1 and 2, raising the floor to 2.
	var t6 *drpc.Frame
	for sid := uint32(1); sid <= 6; sid++ {
		t6 = lcUnaryCall(t, ls, peer, epoch, sid, "m")
	}
	x.Equal(t, 6, int(execs.Load()))
	time.Sleep(100 * time.Millisecond) // let every finish() land its tombstone (> RTI too)

	// Duplicate OPEN for the evicted sid 1: below the floor it is validated
	// and swallowed — NO re-execution AND no reply of any kind (§9.2, §14).
	ls.handleAs(peer, lcOnceOpen(epoch, 1, "m"))
	ls.expectNone(t, "floor-covered duplicate OPEN must be silently deduped")
	x.Equal(t, 6, int(execs.Load()), "floor must dedup, not re-execute")

	// Control: a still-stored sid (6) replays its terminal byte-identically —
	// eviction took the LOWEST sids, not this one.
	ls.handleAs(peer, lcOnceOpen(epoch, 6, "m"))
	replay := ls.recv(t)
	x.True(t, replay != nil, "stored tombstone must replay")
	x.True(t, proto.Equal(t6, replay), "replay must be the stored terminal")
	x.Equal(t, 6, int(execs.Load()))
}

// ---------------------------------------------------------------------------
// §9.2 / §15 byte cap: past MaxTombstoneBytes the OLDEST stored terminals
// degrade to key-only — dedup preserved (no re-execution), replay lost —
// while newer terminals stay stored and replayable.
// ---------------------------------------------------------------------------

func TestLimits_TombstoneByteCapKeyOnly(t *testing.T) {
	var execs atomic.Int32
	// Each stored terminal is ~72 bytes on the wire (the cap counts the whole
	// frame — payload, trailer and status details all cost memory, §9.2): one
	// fits the 100-byte cap, two do not, so completing the second call
	// degrades the first to key-only.
	ls := newLcServer(t, drpc.WithLimits(drpc.Limits{MaxTombstoneBytes: 100}), countExecs(&execs))
	const peer = "peer-bytes"
	const epoch uint32 = 0x2B
	msg := strings.Repeat("a", 30)

	lcUnaryCall(t, ls, peer, epoch, 1, msg)
	t2 := lcUnaryCall(t, ls, peer, epoch, 2, msg)
	x.Equal(t, 2, int(execs.Load()))
	time.Sleep(100 * time.Millisecond) // finish() settled; > RTI past both Ts

	// Duplicate OPEN for sid 1: its terminal was shed by the byte cap, so
	// there is NO replay — but the key survived, so NO re-execution either.
	ls.handleAs(peer, lcOnceOpen(epoch, 1, msg))
	ls.expectNone(t, "key-only tombstone: replay lost, duplicate still swallowed")
	x.Equal(t, 2, int(execs.Load()), "byte cap must never cost at-most-once")

	// Duplicate OPEN for sid 2 (> RTI after its T): still stored, so the
	// byte-identical T is replayed (§9.2).
	ls.handleAs(peer, lcOnceOpen(epoch, 2, msg))
	replay := ls.recv(t)
	x.True(t, replay != nil, "stored tombstone must replay")
	x.True(t, proto.Equal(t2, replay), "replay must be the stored terminal")
	b1, err := proto.Marshal(t2)
	x.NoError(t, err)
	b2, err := proto.Marshal(replay)
	x.NoError(t, err)
	x.True(t, bytes.Equal(b1, b2), "replay must be byte-identical (§10.3)")
	x.Equal(t, 2, int(execs.Load()))
}

// ---------------------------------------------------------------------------
// §15 live-call cap: MaxLiveCalls counts live calls per TRANSPORT PEER,
// across client epochs — an epoch-spoofing peer gets no extra handlers. The
// over-cap OPEN draws T{RESOURCE_EXHAUSTED} (§9.4 cap rejection), while a
// different transport peer is admitted: the cap is per peer, not global.
// ---------------------------------------------------------------------------

func TestLimits_LiveCallCapAcrossEpochs(t *testing.T) {
	var sexecs atomic.Int32
	ls := newLcServer(t, drpc.WithLimits(drpc.Limits{MaxLiveCalls: 2}), lcCountStreamExecs(&sexecs))
	const peerA = "peer-cap-a"
	const peerB = "peer-cap-b"
	const epochA uint32 = 0x3A
	const epochB uint32 = 0x3B // a "fresh incarnation" of the same transport peer
	const epochC uint32 = 0x3C

	// Two live bidi calls under epoch A fill peerA's cap.
	for sid := uint32(1); sid <= 2; sid++ {
		ls.handleAs(peerA, lcLiveOpen(epochA, sid))
		h := ls.recv(t)
		x.True(t, h != nil, "expected creation ack")
		x.True(t, h.GetFlags() == 0 && !h.HasPayload(), "creation ack H (§8)")
	}

	// A third OPEN under a DIFFERENT client epoch of the SAME peer: the cap
	// is counted across epochs (§15) — refused with RESOURCE_EXHAUSTED.
	ls.handleAs(peerA, lcLiveOpen(epochB, 1))
	r := ls.recv(t)
	x.True(t, r != nil, "expected a terminal")
	x.Equal(t, drpc.FlagClose, r.GetFlags())
	x.Equal(t, codes.ResourceExhausted, codes.Code(r.GetCode()))
	x.Equal(t, epochB, r.GetPeerEpoch()) // names the rejected incarnation (§6.1)

	// A different transport peer is under ITS OWN cap: admitted.
	ls.handleAs(peerB, lcLiveOpen(epochC, 1))
	h := ls.recv(t)
	x.True(t, h != nil, "expected creation ack for the other peer")
	x.True(t, h.GetFlags() == 0 && !h.HasPayload(), "creation ack H")

	// Only the three admitted calls ever reached a handler.
	time.Sleep(100 * time.Millisecond)
	x.Equal(t, 3, int(sexecs.Load()), "the rejected OPEN must not spawn a handler")
}

// ---------------------------------------------------------------------------
// §9.4 cap rejection is tombstone-stored: a duplicate of the over-cap OPEN
// draws the byte-identical T{RESOURCE_EXHAUSTED} replay — one bounded answer,
// not silence, not a RESET, and never a handler execution.
// ---------------------------------------------------------------------------

func TestLimits_LiveCallCapRejectionTombstoned(t *testing.T) {
	var sexecs atomic.Int32
	ls := newLcServer(t, drpc.WithLimits(drpc.Limits{MaxLiveCalls: 2}), lcCountStreamExecs(&sexecs))
	const peer = "peer-rej"
	const epochA uint32 = 0x4A
	const epochB uint32 = 0x4B

	for sid := uint32(1); sid <= 2; sid++ {
		ls.handleAs(peer, lcLiveOpen(epochA, sid))
		x.True(t, ls.recv(t) != nil, "expected creation ack")
	}

	ls.handleAs(peer, lcLiveOpen(epochB, 7))
	rej := ls.recv(t)
	x.True(t, rej != nil, "expected the rejection terminal")
	x.Equal(t, codes.ResourceExhausted, codes.Code(rej.GetCode()))

	// The duplicate, spaced > RTI (per-tombstone replay limit, §9.2), draws
	// the stored T back — byte-identical.
	time.Sleep(2 * fastTiming.Retransmit)
	ls.handleAs(peer, lcLiveOpen(epochB, 7))
	replay := ls.recv(t)
	x.True(t, replay != nil, "the rejection must be replayed from its tombstone (§9.4)")
	x.True(t, proto.Equal(rej, replay), "replay must be the stored rejection")
	b1, err := proto.Marshal(rej)
	x.NoError(t, err)
	b2, err := proto.Marshal(replay)
	x.NoError(t, err)
	x.True(t, bytes.Equal(b1, b2), "replay must be byte-identical")

	time.Sleep(100 * time.Millisecond)
	x.Equal(t, 2, int(sexecs.Load()), "neither the rejected OPEN nor its duplicate may execute")
}

// ---------------------------------------------------------------------------
// §15 aggregate reply budget: on top of the per-tombstone 1/RTI limit, one
// transport peer draws at most MaxRepliesPerRTI volunteered replies per RTI;
// denial is silence, and the budget refills after an RTI.
// ---------------------------------------------------------------------------

func TestLimits_AggregateReplyBudget(t *testing.T) {
	bubble(t, func(t *testing.T) {
		ls := newLcServer(t, drpc.WithLimits(drpc.Limits{MaxRepliesPerRTI: 3}))
		const peer = "peer-budget"
		const epoch uint32 = 0x5A

		// Five completed (tombstoned) unary calls, small responses well under
		// the byte cap: every tombstone holds a stored, replayable T.
		for sid := uint32(1); sid <= 5; sid++ {
			ls.handleAs(peer, lcOnceOpen(epoch, sid, "m"))
		}
		synctest.Wait() // handlers finished; Ts transmitted and tombstoned
		x.Equal(t, 5, len(ls.drain()))

		// One straggler per tombstoned sid, all within one RTI window. The
		// per-tombstone 1/RTI limit admits each (first replay per sid); the
		// AGGREGATE budget stops the flood at 3 replies (§15).
		for sid := uint32(1); sid <= 5; sid++ {
			ls.handleAs(peer, lcData(epoch, sid, 2, nil))
		}
		replays := ls.drain() // replay sends are synchronous with Handle
		x.Equal(t, 3, len(replays), "aggregate budget must cap replies per RTI")
		for i, f := range replays {
			x.Equal(t, uint32(i+1), f.GetSid()) // spent in arrival order
			x.Equal(t, drpc.FlagClose, f.GetFlags())
		}

		// After > RTI the window rolls and the budget refills: a straggler
		// for one of the silenced sids now draws its replay.
		time.Sleep(2 * fastTiming.Retransmit)
		ls.handleAs(peer, lcData(epoch, 4, 2, nil))
		refill := ls.drain()
		x.Equal(t, 1, len(refill), "budget must refill after an RTI")
		x.Equal(t, uint32(4), refill[0].GetSid())
	})
}

// ---------------------------------------------------------------------------
// §15 dead-container cap: MaxDeadPeers evicts only among containers with NO
// live calls — a live epoch's calls and dedup state survive. The eviction
// residual is pinned honestly: the evicted epoch's dedup is GONE, so a
// > cap-pressure-delayed duplicate OPEN re-executes — exactly the documented
// §14 residual / §16 L2 window (raise the cap to shrink it).
// ---------------------------------------------------------------------------

func TestLimits_DeadPeerCapSparesLive(t *testing.T) {
	var execs atomic.Int32
	ls := newLcServer(t, drpc.WithLimits(drpc.Limits{MaxDeadPeers: 1}), countExecs(&execs))
	const peer = "peer-dead"
	const epochA uint32 = 0x6A
	const epochB uint32 = 0x6B
	const epochC uint32 = 0x6C

	// Epoch A: one completed call — its container is finished (no live
	// calls), holding only dedup state.
	lcUnaryCall(t, ls, peer, epochA, 1, "a")
	x.Equal(t, 1, int(execs.Load()))

	// Epoch B: a LIVE bidi call.
	ls.handleAs(peer, lcLiveOpen(epochB, 1))
	h := ls.recv(t)
	x.True(t, h != nil && h.GetFlags() == 0, "creation ack expected")

	// Epoch C: a fresh container. The cap (1) fires among the no-live-call
	// containers of this peer only: A is evicted, B is untouchable (§15).
	lcUnaryCall(t, ls, peer, epochC, 5, "c")
	x.Equal(t, 2, int(execs.Load()))

	// The live call under epoch B still works end to end.
	req, err := proto.Marshal(echo.EchoRequest_builder{Message: "still-live", Repeat: 1}.Build())
	x.NoError(t, err)
	ls.handleAs(peer, lcData(epochB, 1, 2, req))
	res := ls.recv(t)
	x.True(t, res != nil, "the live call must still answer")
	x.True(t, res.GetFlags() == 0 && res.HasPayload(), "expected an echo data frame")
	x.Equal(t, epochB, res.GetPeerEpoch())
	got := &echo.EchoResponse{}
	x.NoError(t, proto.Unmarshal(res.GetPayload(), got))
	x.Equal(t, "still-live", got.GetMessage())

	// Epoch A's dedup went with its container: a duplicate OPEN for its sid
	// is admitted and RE-EXECUTES. This is the documented residual of the
	// container cap (§14 "eviction ... degrades to re-execution", §16 L2) —
	// pinned here so a future change that silently widens or closes it is
	// caught either way.
	ls.handleAs(peer, lcOnceOpen(epochA, 1, "a"))
	tr := ls.recv(t)
	x.True(t, tr != nil, "the re-admitted OPEN completes as a fresh call")
	x.Equal(t, codes.OK, codes.Code(tr.GetCode()))
	x.Equal(t, 3, int(execs.Load()), "container eviction residual: duplicate re-executes (§16 L2)")
}

// ---------------------------------------------------------------------------
// §10.4 liveness expiry: when all client traffic stops for T_live, the
// server declares the peer lost — the handler ctx is canceled, NO terminal
// T is emitted (suppressTerm: nothing listens), and the call's tombstone is
// key-only: a late straggler is validated and silently dropped.
// ---------------------------------------------------------------------------

func TestLimits_LivenessExpirySideEffects(t *testing.T) {
	bubble(t, func(t *testing.T) {
		handlerErr := make(chan error, 1)
		ls := newLcServer(t, drpc.StreamInterceptor(func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
			err := handler(srv, ss)
			handlerErr <- err
			return err
		}))
		const peer = "peer-live"
		const epoch uint32 = 0x7A

		ls.handleAs(peer, lcLiveOpen(epoch, 1))
		synctest.Wait()
		hs := ls.drain()
		x.Equal(t, 1, len(hs))
		x.True(t, hs[0].GetFlags() == 0 && !hs[0].HasPayload(), "creation ack H")

		// All client traffic stops. Within T_live (600ms fake time) the sweep
		// expires the peer and cancels the handler; the Live handler unwinds
		// out of Recv with the liveness cause. (The server's keepalive PINGs
		// while the call lived are filtered at capture.)
		var err error
		select {
		case err = <-handlerErr:
		case <-time.After(3 * fastTiming.Liveness):
			t.Fatal("handler not canceled within the liveness bound (§10.4)")
		}
		x.Equal(t, codes.Unavailable, status.Code(err), "cause: peer lost")

		// No terminal T for the lost call — the terminal is suppressed
		// (§10.4 "no T is sent") and the tombstone degrades to key-only.
		synctest.Wait() // let the (suppressed) teardown finish
		x.Equal(t, 0, len(ls.drain()), "no terminal may be emitted for a lost peer's call")

		// A late straggler for the dead call hits the key-only tombstone:
		// validated, deduped, silently dropped — no replay, no RESET (§9.2).
		time.Sleep(2 * fastTiming.Retransmit)
		ls.handleAs(peer, lcData(epoch, 1, 2, nil))
		synctest.Wait()
		x.Equal(t, 0, len(ls.drain()), "key-only tombstone: straggler draws silence")
	})
}
