package drpc_test

// ackrecovery_test.go pins three under-tested corners of the protocol:
//
//   - Creation-ack recovery (PROTOCOL.md §8 "Ack recovery", §10.7 row "Lost
//     creation ack H"): a duplicate OPEN on a live streaming call re-elicits
//     the stored H byte-identically, rate-limited to <= 1 per RTI.
//   - The delayed-RESET hold grace (§9.3): an unknown-sid data frame must NOT
//     kill a call whose OPEN is merely late — the RESET is scheduled for
//     T_hold and cancelled by the OPEN; without an OPEN it fires exactly once.
//   - The stream probe's tight bounds (§10.5, §10.7): an orphaned handler is
//     reclaimed within ~T_probe (not the 3xT_live backstop the existing
//     tests allow), while a healthy idle stream survives probes indefinitely.
//
// Injection tests run outside the bubble (real time, generous windows, plus
// stall guards where an assertion depends on two sends landing within one
// timer window); end-to-end tests run inside the synctest bubble.

import (
	"bytes"
	"context"
	"sync/atomic"
	"testing"
	"time"

	drpc "github.com/lesomnus/grpc-dgram"
	"github.com/lesomnus/grpc-dgram/internal/echo"
	"github.com/lesomnus/grpc-dgram/internal/x"
	"google.golang.org/protobuf/proto"
)

// isAckH matches a header frame H: no flags, no payload (PROTOCOL.md §7).
func isAckH(f *drpc.Frame) bool { return f.GetFlags() == 0 && !f.HasPayload() }

// plainOpen builds the eager CS/bidi OPEN of PROTOCOL.md §8: FlagOpen alone,
// seq 1, no payload — unlike openFrame, which builds the unary OPEN|CLOSE.
func plainOpen(epoch, sid uint32, method string) *drpc.Frame {
	f := &drpc.Frame{}
	f.SetEpoch(epoch)
	f.SetSid(sid)
	f.SetSeq(1)
	f.SetFlags(drpc.FlagOpen)
	f.SetMethod(method)
	return f
}

// awaitFrame returns the first emitted frame matching match within window,
// skipping keepalives/probes; any RESET while waiting fails the test.
func awaitFrame(t *testing.T, is *injectServer, window time.Duration, match func(*drpc.Frame) bool) *drpc.Frame {
	t.Helper()
	deadline := time.After(window)
	for {
		select {
		case f := <-is.out:
			if f.GetFlags() == drpc.FlagReset {
				t.Fatalf("unexpected RESET for sid %d", f.GetSid())
			}
			if match(f) {
				return f
			}
		case <-deadline:
			t.Fatal("expected frame not emitted within the window")
			return nil
		}
	}
}

// collectFrames drains everything the server emits during window.
func collectFrames(is *injectServer, window time.Duration) []*drpc.Frame {
	var out []*drpc.Frame
	deadline := time.After(window)
	for {
		select {
		case f := <-is.out:
			out = append(out, f)
		case <-deadline:
			return out
		}
	}
}

// ---------------------------------------------------------------------------
// §8 ack recovery: a duplicate OPEN (the client's §10.3 retransmission) on a
// live streaming call re-elicits the creation ack — the STORED first H,
// replayed byte-identically (same seq 1), never a freshly numbered frame.
// Reliable mode is out of scope here: there a duplicate OPEN is a broken
// transport and fails the call with INTERNAL (§10.6).
// ---------------------------------------------------------------------------

func TestAckRecovery_DupOpenElicitsByteIdenticalH(t *testing.T) {
	is := newInjectServerMode(t, false, drpc.WithTiming(fastTiming))

	open := plainOpen(1, 5, echo.EchoService_Live_FullMethodName)
	is.handle(proto.CloneOf(open))
	h1 := awaitFrame(t, is, 500*time.Millisecond, isAckH)
	x.Equal(t, 1, h1.GetSeq())
	b1, err := proto.Marshal(h1)
	x.NoError(t, err)

	// Past the 1/RTI replay limit, the same OPEN again (retransmissions are
	// byte-identical, §10.3) must draw an H again.
	time.Sleep(2 * fastTiming.Retransmit)
	is.handle(proto.CloneOf(open))
	h2 := awaitFrame(t, is, 500*time.Millisecond, isAckH)
	x.Equal(t, 1, h2.GetSeq(), "the stored H replays with its original seq")
	b2, err := proto.Marshal(h2)
	x.NoError(t, err)
	x.True(t, bytes.Equal(b1, b2), "H replay must be byte-identical to the stored creation ack")
}

// ---------------------------------------------------------------------------
// §8: the H replay is rate-limited to <= 1 per RTI per call — a dup-OPEN
// flood cannot turn the ack into an amplification vector.
// ---------------------------------------------------------------------------

func TestAckRecovery_HReplayRateLimitedPerRTI(t *testing.T) {
	is := newInjectServerMode(t, false, drpc.WithTiming(fastTiming))

	open := plainOpen(1, 7, echo.EchoService_Live_FullMethodName)
	is.handle(proto.CloneOf(open))
	_ = awaitFrame(t, is, 500*time.Millisecond, isAckH) // creation ack

	// Two duplicates back-to-back, well inside one RTI (50ms). handle is
	// synchronous; guard against a pathological scheduler stall widening the
	// gap past the window under measurement.
	before := time.Now()
	is.handle(proto.CloneOf(open))
	is.handle(proto.CloneOf(open))
	if stall := time.Since(before); stall >= fastTiming.Retransmit {
		t.Skipf("scheduler stalled %v >= RTI between the duplicates; rate limit not measurable", stall)
	}

	replays := 0
	for _, f := range collectFrames(is, 200*time.Millisecond) {
		x.NotEqual(t, drpc.FlagReset, f.GetFlags(), "no RESET may answer a dup OPEN on a live call")
		if isAckH(f) {
			replays++
		}
	}
	x.Equal(t, 1, replays, "exactly one H replay per RTI")
}

// ---------------------------------------------------------------------------
// §10.7 row "Lost creation ack H": recovery is in-band and prompt — the OPEN
// retransmission at RTI elicits the H replay, which stops the retransmission
// (<= next OPEN retransmission + RTI), and the call proceeds normally.
// ---------------------------------------------------------------------------

func TestAckRecovery_LostCreationAckRecoveredInBand(t *testing.T) {
	bubble(t, func(t *testing.T) {
		var opens atomic.Int32
		countOpens := func(next drpc.FrameHandler) drpc.FrameHandler {
			return drpc.FrameHandlerFunc(func(ctx context.Context, f *drpc.Frame) error {
				if f.GetFlags()&drpc.FlagOpen != 0 {
					opens.Add(1)
				}
				return next.Handle(ctx, f)
			})
		}
		// The creation ack is the first server header frame; drop it once.
		client, stop := unreliablePipe(countOpens, dropFirst(isAckH)).Use(t)
		defer stop()

		start := time.Now()
		up, err := client.Buff(t.Context())
		x.NoError(t, err) // eager OPEN out; its ack is eaten by the wire

		// Idle for several RTIs: the ONLY recovery available is the client's
		// OPEN retransmission (t=RTI) eliciting the §8 H replay. Had it not
		// arrived, the OPEN would still be retransmitting on the RTI-doubling
		// schedule (a third send lands at 3xRTI); exactly 2 pins recovery
		// within one round.
		time.Sleep(4 * fastTiming.Retransmit)
		x.Equal(t, 2, int(opens.Load()), "one OPEN retransmission, stopped by the H replay")

		// The call is fully functional after the recovery.
		err = up.Send(echo.EchoRequest_builder{
			Message:       "abc",
			Repeat:        1,
			CircularShift: 1,
		}.Build())
		x.NoError(t, err)
		batch, err := up.CloseAndRecv()
		x.NoError(t, err)
		x.Len(t, batch.GetItems(), 1)
		x.Equal(t, "bca", batch.GetItems()[0].GetMessage())

		// Recovery + completion sit way under T_call (and thus every bound).
		x.True(t, time.Since(start) < fastTiming.Call, "bounded well under T_call")
	})
}

// ---------------------------------------------------------------------------
// §9.3 hold grace — the reason the RESET is DELAYED at all: a data frame
// whose OPEN is merely reordered behind it must not kill the call. The frame
// itself is dropped, not buffered (its loss is within the §14 contract).
// ---------------------------------------------------------------------------

func TestDelayedReset_HoldGraceSparesLateOpen(t *testing.T) {
	is := newInjectServerMode(t, false, drpc.WithTiming(fastTiming))

	// A bidi call's frames arrive reordered: data (seq 2) first...
	early := &drpc.Frame{}
	early.SetEpoch(1)
	early.SetSid(5)
	early.SetSeq(2)
	data, err := proto.Marshal(echo.EchoRequest_builder{Message: "early", Repeat: 1}.Build())
	x.NoError(t, err)
	early.SetPayload(data)

	before := time.Now()
	is.handle(early) // unknown sid: RESET scheduled for T_hold, frame dropped

	// ...then the OPEN, well inside T_hold (both sends are synchronous and
	// back-to-back; guard the assumption anyway).
	is.handle(plainOpen(1, 5, echo.EchoService_Live_FullMethodName))
	if stall := time.Since(before); stall >= fastTiming.Hold {
		t.Skipf("scheduler stalled %v >= T_hold between data and OPEN; grace not measurable", stall)
	}

	// The OPEN created the call: creation ack H, and the pending RESET is
	// cancelled — silence (probes/keepalives aside) well past T_hold + tick.
	h := awaitFrame(t, is, 500*time.Millisecond, isAckH)
	x.Equal(t, 1, h.GetSeq())
	for _, f := range collectFrames(is, 300*time.Millisecond) {
		x.NotEqual(t, drpc.FlagReset, f.GetFlags(), "an OPEN within T_hold must cancel the delayed RESET")
	}

	// Dropped-not-buffered (§9.3): the early frame never reached the call, so
	// the live window never consumed seq 2 — a fresh seq-2 data frame is
	// accepted, and the handler's FIRST echo is this frame, not "early".
	again := &drpc.Frame{}
	again.SetEpoch(1)
	again.SetSid(5)
	again.SetSeq(2)
	data, err = proto.Marshal(echo.EchoRequest_builder{Message: "again", Repeat: 1}.Build())
	x.NoError(t, err)
	again.SetPayload(data)
	is.handle(again)

	reply := awaitFrame(t, is, 500*time.Millisecond, func(f *drpc.Frame) bool {
		return f.GetFlags() == 0 && f.HasPayload()
	})
	res := &echo.EchoResponse{}
	x.NoError(t, proto.Unmarshal(reply.GetPayload(), res))
	x.Equal(t, "again", res.GetMessage(), "the pre-OPEN frame was dropped, never buffered")
	x.Equal(t, 0, res.GetSequence(), "first message the handler ever saw")
}

// ---------------------------------------------------------------------------
// §9.3: when the OPEN never comes, the delayed RESET fires after T_hold —
// exactly one, echoing the offending frame's epoch, and nothing else.
// ---------------------------------------------------------------------------

func TestDelayedReset_FiresWhenOpenNeverComes(t *testing.T) {
	is := newInjectServerMode(t, false, drpc.WithTiming(fastTiming))

	orphan := &drpc.Frame{}
	orphan.SetEpoch(0xBEEF)
	orphan.SetSid(6)
	orphan.SetSeq(2)
	orphan.SetPayload([]byte{})
	is.handle(orphan)

	// Due at T_hold (50ms) + sweep tick (25ms) =~ 75ms; drain generously. No
	// call and no peer container exist, so the RESET must be the ONLY frame.
	frames := collectFrames(is, 300*time.Millisecond)
	x.Len(t, frames, 1, "exactly one delayed RESET, nothing else")
	r := frames[0]
	x.Equal(t, drpc.FlagReset, r.GetFlags())
	x.Equal(t, 0xBEEF, r.GetEpoch(), "RESET echoes the offender's epoch (§9.3 identity)")
	x.Equal(t, 6, r.GetSid())
}

// ---------------------------------------------------------------------------
// §10.5 / §10.7: an orphaned server handler — its client forgot the call and
// every abort is lost — is reclaimed within ~T_probe: the server's stream
// probe (PING sid!=0) hits the client's tombstone, which answers RESET, and
// the handler dies. This is the TIGHT bound; the existing vanished-client
// test only pins the 3xT_live backstop.
// ---------------------------------------------------------------------------

func TestStreamProbe_OrphanHandlerReclaimedWithinTProbe(t *testing.T) {
	bubble(t, func(t *testing.T) {
		// The client's abort — and every §10.3 retransmission of it — is
		// blackholed forever: as far as the server can observe, the client
		// silently forgot the call. Probes and RESETs still pass, so the
		// reclaim can only come from the probe -> tombstone-RESET path.
		dropAborts := func(next drpc.FrameHandler) drpc.FrameHandler {
			return drpc.FrameHandlerFunc(func(ctx context.Context, f *drpc.Frame) error {
				if isTerminal(f) {
					return nil
				}
				return next.Handle(ctx, f)
			})
		}
		client, stop := unreliablePipe(dropAborts, nil).Use(t)
		defer stop()

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		stream, err := client.Live(ctx)
		x.NoError(t, err)
		x.NoError(t, stream.Send(echo.EchoRequest_builder{Message: "a", Repeat: 1}.Build()))
		_, err = stream.Recv()
		x.NoError(t, err) // the handler is live, blocked in Recv

		// The client forgets the call. Its tombstone (TTL_tomb 1500ms) is
		// still warm when the server's probe arrives at ~T_probe (200ms) and
		// draws the immediate RESET (§9.3) that kills the handler.
		cancel()

		done := make(chan struct{})
		go func() {
			client.server.GracefulStop()
			close(done)
		}()
		// T_probe + a sweep tick for the probe + the RESET round trip, with
		// slack: 400ms — deliberately under T_live (600ms), so passing proves
		// the probe path fired, not the liveness backstop.
		bound := fastTiming.Liveness/3 + 2*fastTiming.Retransmit + 100*time.Millisecond
		select {
		case <-done:
		case <-time.After(bound):
			t.Fatalf("orphaned handler not reclaimed within %v (~T_probe): probe->RESET path did not fire", bound)
		}
	})
}

// ---------------------------------------------------------------------------
// §10.5's core promise: a healthy idle stream is NEVER killed by silence.
// Unlike TestChar_HealthyIdleBidiNotKilled this also pins that stream probes
// actually flowed in BOTH directions (each side probes independently; a
// received probe is a no-op on a live stream and resets no idle clock).
// ---------------------------------------------------------------------------

func TestStreamProbe_HealthyIdleStreamSurvivesProbes(t *testing.T) {
	bubble(t, func(t *testing.T) {
		var probesC2S, probesS2C atomic.Int32
		countProbes := func(n *atomic.Int32) func(drpc.FrameHandler) drpc.FrameHandler {
			return func(next drpc.FrameHandler) drpc.FrameHandler {
				return drpc.FrameHandlerFunc(func(ctx context.Context, f *drpc.Frame) error {
					if f.GetFlags() == drpc.FlagPing && f.GetSid() != 0 {
						n.Add(1)
					}
					return next.Handle(ctx, f)
				})
			}
		}
		client, stop := unreliablePipe(countProbes(&probesC2S), countProbes(&probesS2C)).Use(t)
		defer stop()

		stream, err := client.Live(t.Context())
		x.NoError(t, err)
		x.NoError(t, stream.Send(echo.EchoRequest_builder{Message: "a", Repeat: 1}.Build()))
		_, err = stream.Recv()
		x.NoError(t, err)

		// Fully idle for 3 x T_probe (== T_live) with zero application
		// traffic: only probes and keepalives flow.
		time.Sleep(fastTiming.Liveness + 50*time.Millisecond)
		x.True(t, probesC2S.Load() >= 2, "client probed the idle stream, got ", probesC2S.Load())
		x.True(t, probesS2C.Load() >= 2, "server probed the idle stream, got ", probesS2C.Load())

		// Nothing died: the stream is still fully usable.
		x.NoError(t, stream.Send(echo.EchoRequest_builder{Message: "b", Repeat: 1}.Build()))
		res, err := stream.Recv()
		x.NoError(t, err)
		x.Equal(t, "b", res.GetMessage())

		// Clean shutdown so teardown is prompt.
		x.NoError(t, stream.CloseSend())
		for {
			if _, err := stream.Recv(); err != nil {
				break
			}
		}
	})
}
