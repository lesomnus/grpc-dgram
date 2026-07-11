package drpc_test

// retx_test.go pins the control-frame retransmission MECHANICS of
// PROTOCOL.md §10.3 — cadence, byte identity, the one-shared-schedule rule,
// and the obligation-clear rules at tombstones — not just their end-to-end
// recovery outcomes (those live in timeout_test.go).
//
// Harness: a Conn over a mute wire. Every frame the Conn transmits is
// recorded (clone + fake-clock timestamp) and NOTHING is ever delivered
// back — the "server" answers only when a test injects a crafted frame via
// Conn.Handle. Every crafted server frame must echo the Conn's incarnation
// in peer_epoch or the §6.1 gate refuses it; RESET is exempt (§9.3) and
// instead must echo the client epoch in its epoch field.
//
// All tests run inside bubble (testing/synctest): time.Sleep advances the
// fake clock instantly, so recorded timestamps are exact. The Conn's sweeper
// observes retransmission deadlines on a coarse tick (min(RTI, Hold)/2 =
// 25ms for fastTiming), so cadence assertions allow one tick of lateness:
// each delta must land within [want, want+tick].

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	drpc "github.com/lesomnus/grpc-dgram"
	"github.com/lesomnus/grpc-dgram/internal/echo"
	"github.com/lesomnus/grpc-dgram/internal/x"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// retxRec is one recorded transmission: the frame and the fake-clock instant
// it hit the wire.
type retxRec struct {
	at time.Time
	f  *drpc.Frame
}

// retxWire is the mute tx: it records everything and delivers nothing.
type retxWire struct {
	mu   sync.Mutex
	recs []retxRec
}

func (w *retxWire) Handle(_ context.Context, f *drpc.Frame) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.recs = append(w.recs, retxRec{at: time.Now(), f: proto.CloneOf(f)})
	return nil
}

func (w *retxWire) recorded(match func(f *drpc.Frame) bool) []retxRec {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := []retxRec{}
	for _, r := range w.recs {
		if match(r.f) {
			out = append(out, r)
		}
	}
	return out
}

func newRetxConn() (*retxWire, *drpc.Conn) {
	w := &retxWire{}
	return w, drpc.NewConn(w, drpc.WithReliable(false), drpc.WithTiming(fastTiming))
}

func isOpenFrame(f *drpc.Frame) bool  { return f.GetFlags()&drpc.FlagOpen != 0 }
func isDataFrame(f *drpc.Frame) bool  { return f.GetFlags() == 0 && f.HasPayload() }
func isResetFrame(f *drpc.Frame) bool { return f.GetFlags()&drpc.FlagReset != 0 }

// retxSrvEpoch is the server incarnation the crafted frames impersonate.
const retxSrvEpoch uint32 = 0x5eed

// retxServerFrame is the base of every crafted server frame: peer_epoch MUST
// echo the client incarnation or Conn.Handle refuses it (PROTOCOL.md §6.1).
func retxServerFrame(cEpoch, sid uint32) *drpc.Frame {
	f := &drpc.Frame{}
	f.SetEpoch(retxSrvEpoch)
	f.SetPeerEpoch(cEpoch)
	f.SetSid(sid)
	return f
}

// retxH crafts the creation-ack header frame H: seq 1, no flags, no payload
// (PROTOCOL.md §7, §8).
func retxH(cEpoch, sid uint32) *drpc.Frame {
	f := retxServerFrame(cEpoch, sid)
	f.SetSeq(1)
	return f
}

// retxKeepalive crafts a peer keepalive PING sid=0 (PROTOCOL.md §10.4).
func retxKeepalive(cEpoch uint32) *drpc.Frame {
	f := retxServerFrame(cEpoch, 0)
	f.SetFlags(drpc.FlagPing)
	return f
}

// retxProbe crafts a stream probe PING sid≠0, seq 0 (PROTOCOL.md §10.5).
func retxProbe(cEpoch, sid uint32) *drpc.Frame {
	f := retxServerFrame(cEpoch, sid)
	f.SetFlags(drpc.FlagPing)
	return f
}

// retxT crafts a server terminal T for sid: CLOSE + code. Any server epoch
// matches a client tombstone (PROTOCOL.md §9.1-5b: sids are client-owned and
// never reused, so any terminal for the sid is a terminal of that call).
func retxT(cEpoch, sid uint32, code codes.Code) *drpc.Frame {
	f := retxServerFrame(cEpoch, sid)
	f.SetSeq(1)
	f.SetFlags(drpc.FlagClose)
	f.SetCode(uint32(code))
	return f
}

// retxReset crafts a conforming server RESET answering a client frame of the
// call: epoch echoes the offending frame's epoch — the CLIENT epoch — and
// peer_epoch re-echoes its peer_epoch, which is 0 on client frames
// (PROTOCOL.md §9.3). RESET is exempt from the §6.1 echo gate.
func retxReset(cEpoch, sid uint32) *drpc.Frame {
	f := &drpc.Frame{}
	f.SetFlags(drpc.FlagReset)
	f.SetEpoch(cEpoch)
	f.SetSid(sid)
	return f
}

// frameBytes marshals deterministically for byte-identity assertions.
func frameBytes(t *testing.T, f *drpc.Frame) []byte {
	t.Helper()
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(f)
	x.NoError(t, err)
	return b
}

// assertCadence pins the spacing of consecutive recorded transmissions:
// want[i] apart, allowing one sweep tick of lateness — the sweeper only
// observes retransmission deadlines on its coarse tick.
func assertCadence(t *testing.T, recs []retxRec, want ...time.Duration) {
	t.Helper()
	x.Equal(t, len(want)+1, len(recs), "number of transmissions")
	tick := min(fastTiming.Retransmit, fastTiming.Hold) / 2
	for i, w := range want {
		d := recs[i+1].at.Sub(recs[i].at)
		if d < w || d > w+tick {
			t.Fatalf("transmissions %d -> %d spaced %v apart, want within [%v, %v]", i, i+1, d, w, w+tick)
		}
	}
}

// assertSameBytes pins byte-identical retransmission (PROTOCOL.md §10.3):
// every record reuses the first one's seq and marshals to the same bytes.
func assertSameBytes(t *testing.T, recs []retxRec) {
	t.Helper()
	base := frameBytes(t, recs[0].f)
	for i, r := range recs[1:] {
		x.Equal(t, recs[0].f.GetSeq(), r.f.GetSeq(), "a retransmission must reuse the original seq")
		x.True(t, bytes.Equal(base, frameBytes(t, r.f)), "retransmission ", i+1, " must be byte-identical to the original")
	}
}

// sleepAlive advances the fake clock by total in step-sized slices, feeding
// peer liveness (PROTOCOL.md §10.4) with a keepalive PING after each slice:
// the mute server is healthy-but-silent, and T_live must not expire the call
// under test. Keepalives never touch per-stream retransmission state.
func sleepAlive(t *testing.T, conn *drpc.Conn, cEpoch uint32, total, step time.Duration) {
	t.Helper()
	for total > 0 {
		d := min(step, total)
		time.Sleep(d)
		total -= d
		x.NoError(t, conn.Handle(t.Context(), retxKeepalive(cEpoch)))
	}
	synctest.Wait()
}

func TestControlRetransmission(t *testing.T) {
	var (
		rti      = fastTiming.Retransmit   // 50ms
		probeCap = fastTiming.Liveness / 3 // T_probe = 200ms: the backoff cap
	)

	t.Run("OPEN retransmits byte-identically at RTI doubling capped at T_probe", func(t *testing.T) {
		// §10.3: control events are retransmitted byte-identically (same
		// seq) at RTI, doubling per attempt, capped at T_probe.
		bubble(t, func(t *testing.T) {
			w, conn := newRetxConn()
			defer conn.Close(nil)
			client := echo.NewEchoServiceClient(conn)

			_, err := client.Buff(t.Context()) // CS: eager OPEN (§8); no reply ever
			x.NoError(t, err)
			synctest.Wait()
			opens := w.recorded(isOpenFrame)
			x.Equal(t, 1, len(opens))
			epoch := opens[0].f.GetEpoch()

			// 800ms against a healthy-but-silent server: RTI, 2×RTI, then
			// the T_probe cap — retransmissions at +50, +150, +350, +550,
			// +750 of fake time.
			sleepAlive(t, conn, epoch, 800*time.Millisecond, 100*time.Millisecond)

			opens = w.recorded(isOpenFrame)
			assertCadence(t, opens, rti, 2*rti, probeCap, probeCap, probeCap)
			x.Equal(t, uint32(1), opens[0].f.GetSeq(), "an OPEN's seq MUST be 1 (§8)")
			assertSameBytes(t, opens)
		})
	})

	t.Run("first server frame stops OPEN retransmission", func(t *testing.T) {
		// §10.3 table: the CS/bidi OPEN obligation ends at the first server
		// frame for the sid — here the §8 creation ack H.
		bubble(t, func(t *testing.T) {
			w, conn := newRetxConn()
			defer conn.Close(nil)
			client := echo.NewEchoServiceClient(conn)

			_, err := client.Buff(t.Context())
			x.NoError(t, err)
			synctest.Wait()
			open := w.recorded(isOpenFrame)[0].f
			epoch, sid := open.GetEpoch(), open.GetSid()

			time.Sleep(60 * time.Millisecond) // past the first retransmission
			synctest.Wait()
			x.Equal(t, 2, len(w.recorded(isOpenFrame)))

			// The creation ack lands: any server frame for the sid.
			x.NoError(t, conn.Handle(t.Context(), retxH(epoch, sid)))

			// No OPEN reappears over more than 4×T_probe of kept-alive
			// silence.
			sleepAlive(t, conn, epoch, 900*time.Millisecond, 100*time.Millisecond)
			x.Equal(t, 2, len(w.recorded(isOpenFrame)), "the first server frame must stop OPEN retransmission")
		})
	})

	t.Run("a stream probe is not a first server frame", func(t *testing.T) {
		// §7: a probe (PING sid≠0) never counts as the "first server frame"
		// of §10.3 — the OPEN schedule must run as if the probes were not
		// there. The call is live, so the probe is a no-op, not a RESET.
		bubble(t, func(t *testing.T) {
			w, conn := newRetxConn()
			defer conn.Close(nil)
			client := echo.NewEchoServiceClient(conn)

			_, err := client.Buff(t.Context())
			x.NoError(t, err)
			synctest.Wait()
			open := w.recorded(isOpenFrame)[0].f
			epoch, sid := open.GetEpoch(), open.GetSid()

			time.Sleep(60 * time.Millisecond) // past the first retransmission
			synctest.Wait()
			x.Equal(t, 2, len(w.recorded(isOpenFrame)))

			// Probe the live call every 100ms. Probes refresh peer liveness
			// (§9.1: validated) — so no keepalives are needed — but must not
			// stop the retransmission.
			for range 8 {
				x.NoError(t, conn.Handle(t.Context(), retxProbe(epoch, sid)))
				time.Sleep(100 * time.Millisecond)
			}
			synctest.Wait()

			opens := w.recorded(isOpenFrame)
			assertCadence(t, opens, rti, 2*rti, probeCap, probeCap, probeCap)
			assertSameBytes(t, opens)
			x.Equal(t, 0, len(w.recorded(isResetFrame)), "a probe of a live call is a no-op, not a RESET")
		})
	})

	t.Run("half-close restarts the one shared schedule at RTI", func(t *testing.T) {
		// §10.3: a stream keeps ONE retransmission schedule. Arming a new
		// control event (the half-close) while the OPEN obligation still
		// stands restarts it at RTI for everything the stream owes — and the
		// half-close does NOT stop the OPEN retransmission (§10.3 table).
		bubble(t, func(t *testing.T) {
			w, conn := newRetxConn()
			defer conn.Close(nil)
			client := echo.NewEchoServiceClient(conn)

			up, err := client.Buff(t.Context())
			x.NoError(t, err)
			synctest.Wait()
			epoch := w.recorded(isOpenFrame)[0].f.GetEpoch()

			// Let the OPEN back off to the cap: retransmissions at +50,
			// +150, +350; the next slot under the old schedule would be at
			// +550, a full T_probe away.
			time.Sleep(400 * time.Millisecond)
			synctest.Wait()
			opens := w.recorded(isOpenFrame)
			assertCadence(t, opens, rti, 2*rti, probeCap)

			x.NoError(t, up.CloseSend())
			sleepAlive(t, conn, epoch, 400*time.Millisecond, 100*time.Millisecond)

			// The CLOSE consumes its own seq and retransmits on a fresh RTI
			// schedule — NOT at the backed-off cap the OPEN had reached.
			closes := w.recorded(isHalfClose)
			x.Equal(t, uint32(2), closes[0].f.GetSeq(), "the half-close consumes its own seq")
			assertCadence(t, closes, rti, 2*rti, probeCap)
			assertSameBytes(t, closes)

			// Every retransmission round carries BOTH obligations: the
			// backed-off OPEN rides the restarted schedule.
			lateOpens := w.recorded(isOpenFrame)[len(opens):]
			x.Equal(t, len(closes)-1, len(lateOpens), "each retx round after the half-close must carry the OPEN too")
			for i, r := range lateOpens {
				x.True(t, r.at.Equal(closes[i+1].at), "round ", i+1, ": OPEN and CLOSE must ride the same schedule")
			}
			assertSameBytes(t, append([]retxRec{opens[0]}, lateOpens...))
		})
	})

	t.Run("abort obligation outlives the call on its tombstone", func(t *testing.T) {
		// §10.3: the abort is local-immediate — the caller unblocks at once —
		// but the CLOSE{code} keeps retransmitting at RTI doubling under the
		// tombstone's obligation until a matching T (any server epoch,
		// §9.1-5b) clears it.
		bubble(t, func(t *testing.T) {
			w, conn := newRetxConn()
			defer conn.Close(nil)
			client := echo.NewEchoServiceClient(conn)

			ctx, cancel := context.WithTimeout(t.Context(), 2*rti)
			defer cancel()
			_, err := client.Once(ctx, &echo.EchoRequest{})
			x.Equal(t, codes.DeadlineExceeded, status.Code(err))
			synctest.Wait()

			open := w.recorded(isOpenFrame)[0].f
			aborts := w.recorded(isTerminal)
			x.True(t, len(aborts) >= 1, "the deadline abort must be emitted")
			x.Equal(t, uint32(codes.DeadlineExceeded), aborts[0].f.GetCode())

			// The call is gone and the caller has returned; the tombstone
			// keeps retransmitting: +RTI, +2×RTI, then the cap.
			time.Sleep(450 * time.Millisecond)
			synctest.Wait()
			aborts = w.recorded(isTerminal)
			// Key the series on the frame the tombstone owns: the last
			// record is necessarily a retransmission of exactly that frame.
			obSeq := aborts[len(aborts)-1].f.GetSeq()
			series := w.recorded(func(f *drpc.Frame) bool { return isTerminal(f) && f.GetSeq() == obSeq })
			assertCadence(t, series, rti, 2*rti, probeCap)
			assertSameBytes(t, series)

			// A matching terminal — CLOSE with a code, under ANY server
			// epoch — clears the obligation at the tombstone.
			x.NoError(t, conn.Handle(t.Context(), retxT(open.GetEpoch(), open.GetSid(), codes.Canceled)))
			n := len(w.recorded(isTerminal))
			time.Sleep(800 * time.Millisecond)
			synctest.Wait()
			x.Equal(t, n, len(w.recorded(isTerminal)), "the matching T must clear the abort obligation")
		})
	})

	t.Run("RESET clears the abort obligation", func(t *testing.T) {
		// §10.3, §9.1-2: a RESET matching a tombstoned call clears its
		// pending abort retransmission.
		bubble(t, func(t *testing.T) {
			w, conn := newRetxConn()
			defer conn.Close(nil)
			client := echo.NewEchoServiceClient(conn)

			ctx, cancel := context.WithCancel(t.Context())
			_, err := client.Live(ctx)
			x.NoError(t, err)
			synctest.Wait()
			open := w.recorded(isOpenFrame)[0].f

			time.Sleep(2 * rti)
			cancel() // user cancel: abort CLOSE{CANCELLED}, local-immediate
			synctest.Wait()
			aborts := w.recorded(isTerminal)
			x.Equal(t, 1, len(aborts))
			x.Equal(t, uint32(codes.Canceled), aborts[0].f.GetCode())

			// Two retransmission rounds under the tombstone's obligation...
			time.Sleep(350 * time.Millisecond)
			synctest.Wait()
			aborts = w.recorded(isTerminal)
			assertCadence(t, aborts, rti, 2*rti)
			assertSameBytes(t, aborts)

			// ...until a RESET for the sid. A conforming server echoes the
			// offending frame's epoch (the CLIENT epoch) and its peer_epoch,
			// which is 0 on client frames; RESET bypasses the §6.1 gate.
			x.NoError(t, conn.Handle(t.Context(), retxReset(open.GetEpoch(), open.GetSid())))
			time.Sleep(800 * time.Millisecond)
			synctest.Wait()
			x.Equal(t, len(aborts), len(w.recorded(isTerminal)), "the RESET must clear the abort obligation")
		})
	})

	t.Run("data frames are never retransmitted", func(t *testing.T) {
		// §10.3 first line: data frames are never retransmitted — only the
		// control events are, and the OPEN schedule running alongside proves
		// the machinery was live while the data frames stayed put.
		bubble(t, func(t *testing.T) {
			w, conn := newRetxConn()
			defer conn.Close(nil)
			client := echo.NewEchoServiceClient(conn)

			up, err := client.Live(t.Context()) // bidi: eager OPEN (§8)
			x.NoError(t, err)
			x.NoError(t, up.Send(echo.EchoRequest_builder{Message: "a"}.Build()))
			x.NoError(t, up.Send(echo.EchoRequest_builder{Message: "b"}.Build()))
			synctest.Wait()

			time.Sleep(500 * time.Millisecond)
			synctest.Wait()

			assertCadence(t, w.recorded(isOpenFrame), rti, 2*rti, probeCap)
			for _, seq := range []uint32{2, 3} {
				data := w.recorded(func(f *drpc.Frame) bool { return isDataFrame(f) && f.GetSeq() == seq })
				x.Equal(t, 1, len(data), "data frame seq ", seq, " must appear exactly once")
			}
			x.Equal(t, 2, len(w.recorded(isDataFrame)), "no other data frame may appear")
		})
	})
}
