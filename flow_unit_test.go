package drpc

// White-box unit tests for the connection-window primitives of flow.go
// (PROTOCOL.md §4.2.1, reliable mode only), no transport: the sender's
// settle (confirm), the combined acquire over both windows (acquire2), and
// the receiver's physical ledger (peerFlowRx). Each pins one normative
// sentence the end-to-end tests of flow_test.go can only observe indirectly.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// epochT is the one peer incarnation the single-incarnation ledger tests
// return credit for.
const epochT = uint32(0xE)

func newPeerFlowRx(window uint32) *peerFlowRx {
	p := &peerFlowRx{}
	p.enable(window, 0)
	return p
}

// snapshot reads the ledger: outstanding, and the credit held back across
// every incarnation.
func (p *peerFlowRx) snapshot() (outstanding, pending uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, n := range p.pending {
		pending += n
	}
	return p.outstanding, pending
}

// pendingOf reads the credit held back for one incarnation.
func (p *peerFlowRx) pendingOf(epoch uint32) uint32 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pending[epoch]
}

func (f *flowSender) snapshot() (on bool, granted, sent int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.on, f.granted, f.sent
}

// ---------------------------------------------------------------------------
// peerFlowRx: the ledger. outstanding is what the peer has in our buffers —
// it never passes the window (admit refuses) and never underflows (a retire
// past it saturates rather than opening the bound).
// ---------------------------------------------------------------------------

// Pins §4.2.1 The receiver's ledger: "A receiver MUST NOT hold more than
// MaxPeerWindow buffered messages from one peer".
func TestPeerFlowRx_AdmitRefusesPastTheWindow(t *testing.T) {
	p := newPeerFlowRx(8)
	for i := range 8 {
		if !p.admit() {
			t.Fatalf("frame %d must be admitted: %d of 8 outstanding", i, i)
		}
	}
	if p.admit() {
		t.Fatal("the 9th frame would take outstanding past the window; admit must refuse")
	}
	if out, _ := p.snapshot(); out != 8 {
		t.Fatalf("a refused admit must charge nothing: outstanding = %d, want 8", out)
	}

	// Retiring more than was ever admitted saturates at zero: the bound is
	// enforced on outstanding, so it must never wrap into "unlimited".
	if g := p.retire(epochT, 20, false); g != 0 {
		t.Fatalf("retire(n, false) must credit nothing, got a grant of %d", g)
	}
	out, pend := p.snapshot()
	if out != 0 || pend != 0 {
		t.Fatalf("outstanding/pending = %d/%d, want 0/0", out, pend)
	}
	if !p.admit() {
		t.Fatal("room again after the retire; admit must accept")
	}
}

// Pins §4.2.1 The receiver's ledger: no credit for "data frames of a server
// incarnation the Conn has moved past".
func TestPeerFlowRx_RetireWithoutCreditGrantsNothing(t *testing.T) {
	p := newPeerFlowRx(8)
	for range 8 {
		p.admit()
	}
	// The frames of a server incarnation the client has moved past leave
	// the buffers but return no credit — no one is left to receive it.
	if g := p.retire(epochT, 8, false); g != 0 {
		t.Fatalf("retire(8, false) granted %d, want 0", g)
	}
	if _, pend := p.snapshot(); pend != 0 {
		t.Fatalf("pending = %d after an uncredited retire, want 0", pend)
	}
	// And a credited retire afterwards starts from zero pending: the
	// uncredited frames are not counted in later batching either.
	for range 4 {
		p.admit()
	}
	if g := p.retire(epochT, 3, true); g != 0 {
		t.Fatalf("3 pending of a window of 8, 1 outstanding: nothing due yet, got %d", g)
	}
	if g := p.retire(epochT, 1, true); g != 4 {
		t.Fatalf("4 pending = half the window: a grant of 4 is due, got %d", g)
	}
	out, pend := p.snapshot()
	if out != 0 || pend != 0 {
		t.Fatalf("outstanding/pending = %d/%d after the grant, want 0/0", out, pend)
	}
}

// §4.2.1's starvation MUST applied to the connection window: with stuck
// consumers pinning most of it, pending can never reach half the window while
// the sender is out of credit — so a grant fires whenever outstanding +
// pending reaches the window, even for a single message.
// Pins §4.2.1 Cadence: "It MUST grant, whatever it holds back, whenever
// buffered + held back ≥ MaxPeerWindow ... at that edge the cost is one grant
// per message consumed".
func TestPeerFlowRx_StarvationClauseGrantsBelowHalfWindow(t *testing.T) {
	p := newPeerFlowRx(32)
	for range 32 {
		p.admit() // the peer has spent its whole window
	}
	// One message consumed on the healthy stream: pending*2 = 2 < 32, yet
	// outstanding + pending = 31 + 1 = 32 >= 32 — the sender is at zero
	// credit and everything it could wait for is this one message.
	if g := p.retire(epochT, 1, true); g != 1 {
		t.Fatalf("starvation clause: want a grant of 1, got %d", g)
	}
	// And it keeps firing one per message while the stuck consumers pin
	// the rest — bounded by the consumption rate, never silent: the sender
	// spends the one credit, the healthy consumer reads it, one comes back.
	for i := range 5 {
		if !p.admit() {
			t.Fatalf("round %d: the granted credit must be admissible", i)
		}
		if g := p.retire(epochT, 1, true); g != 1 {
			t.Fatalf("round %d: want a grant of 1, got %d", i, g)
		}
	}
	// With the sender at zero credit nothing is pending, and one message
	// consumed without a new arrival is credit the sender did not need yet
	// (31 + 1 < 32): batching resumes.
	if g := p.retire(epochT, 1, true); g != 0 {
		t.Fatalf("room again at the peer: no grant due, got %d", g)
	}

	// Without the clause's condition the half-window batching alone rules:
	// 16 outstanding, 1 pending → 17 < 32 and 2 < 32 → nothing due.
	q := newPeerFlowRx(32)
	for range 17 {
		q.admit()
	}
	if g := q.retire(epochT, 1, true); g != 0 {
		t.Fatalf("room remains at the peer (16 + 1 < 32): no grant due, got %d", g)
	}
}

// Pins §4.2.1 Cadence: "A receiver SHOULD batch: grant once the credit it
// holds back reaches half its window".
func TestPeerFlowRx_HalfWindowBatching(t *testing.T) {
	p := newPeerFlowRx(wConn)
	for range 600 {
		p.admit()
	}
	total := uint32(0)
	grants := 0
	for range 600 {
		if g := p.retire(epochT, 1, true); g != 0 {
			total += g
			grants++
		}
	}
	if grants != 1 || total != 512 {
		t.Fatalf("600 consumed against a window of 1024 batches into one grant of 512, got %d grants totalling %d", grants, total)
	}
	// The 88 left over wait for the next half window, as flowReceiver's
	// do: the peer has 1024 − 88 credits and is nowhere near starving.
	if _, pend := p.snapshot(); pend != 88 {
		t.Fatalf("pending = %d, want the 88 left after the 512 grant", pend)
	}
}

// Frames that were never admitted — RESET-drawn, unknown or tombstoned sid —
// return their credit without touching outstanding: the sender spent it, and
// a window that never gets it back is a permanent shrink (§10.6).
// Pins §4.2.1 The receiver's ledger: "The bound is enforced on what is
// buffered, never on what was granted ... and junk cannot desync the ledger".
func TestPeerFlowRx_UnadmittedReturnsCreditWithoutTouchingOutstanding(t *testing.T) {
	p := newPeerFlowRx(8)
	for range 3 {
		p.admit()
	}
	if g := p.unadmitted(epochT, 3); g != 0 {
		t.Fatalf("3 pending < half of 8 and 3 + 3 < 8: nothing due, got %d", g)
	}
	out, pend := p.snapshot()
	if out != 3 || pend != 3 {
		t.Fatalf("outstanding/pending = %d/%d, want 3/3 (unadmitted must not lower outstanding)", out, pend)
	}
	if g := p.unadmitted(epochT, 1); g != 4 {
		t.Fatalf("4 pending = half the window: grant 4, got %d", g)
	}
	if out, _ := p.snapshot(); out != 3 {
		t.Fatalf("outstanding = %d after the grant, want 3", out)
	}
}

// Pins §4.2.1 Unreliable mode: "no assumption, no ledger, no raise".
func TestPeerFlowRx_OffAdmitsEverythingAndGrantsNothing(t *testing.T) {
	p := newPeerFlowRx(0) // unreliable mode: no bound, no grants
	if p.active() {
		t.Fatal("window 0 must read as off")
	}
	for range 3000 {
		if !p.admit() {
			t.Fatal("off: admit must never refuse")
		}
	}
	if g := p.retire(epochT, 3000, true); g != 0 {
		t.Fatalf("off: retire must grant nothing, got %d", g)
	}
	if g := p.unadmitted(epochT, 3000); g != 0 {
		t.Fatalf("off: unadmitted must grant nothing, got %d", g)
	}
	out, pend := p.snapshot()
	if out != 0 || pend != 0 {
		t.Fatalf("off: nothing may be counted, outstanding/pending = %d/%d", out, pend)
	}
	if g := p.raise(); g != 0 {
		t.Fatalf("off: nothing to raise, got %d", g)
	}
}

// The raise: window − W_conn, once per incarnation, 0 ever after.
// Pins §4.2.1 Raise: "once per peer incarnation with a sid = 0 grant of
// MaxPeerWindow − W_conn ... A receiver at the floor sends none".
func TestPeerFlowRx_RaiseIsWindowMinusWConnOnce(t *testing.T) {
	p := newPeerFlowRx(2048)
	if g := p.raise(); g != 1024 {
		t.Fatalf("raise = %d, want 2048 − 1024", g)
	}
	if g := p.raise(); g != 0 {
		t.Fatalf("second raise = %d, want 0", g)
	}

	// At the floor there is nothing to raise by — and it still counts as
	// the incarnation's one raise.
	q := newPeerFlowRx(wConn)
	if g := q.raise(); g != 0 {
		t.Fatalf("window == W_conn: raise = %d, want 0", g)
	}
	if g := q.raise(); g != 0 {
		t.Fatalf("window == W_conn, again: raise = %d, want 0", g)
	}
}

// ---------------------------------------------------------------------------
// flowSender.confirm: the settle. 0 turns the window off and a later grant is
// dropped; > 0 keeps the credit as it stands, including a grant that raced
// ahead of the settle (observe would have replaced it).
// ---------------------------------------------------------------------------

// Pins §4.2.1 Settle: "window = 0 turns the connection window off toward that
// peer", and Grants: "A grant toward a window that is off is dropped".
func TestFlowSender_ConfirmZeroTurnsOffAndDropsGrants(t *testing.T) {
	var f flowSender
	f.assume(wConn)
	f.confirm(0)
	f.grant(5)
	on, granted, _ := f.snapshot()
	if on {
		t.Fatal("confirm(0): the peer does no flow control, the window must be off")
	}
	if granted != int64(wConn) {
		t.Fatalf("a grant to an off window must be dropped: granted = %d", granted)
	}
	// Off means unlimited: far past W_conn without a park.
	for i := range 3 * int(wConn) {
		if _, ok := f.tryAcquire(); !ok {
			t.Fatalf("send %d parked on an off window", i)
		}
	}
	// And the settle is once only: a later advertisement cannot turn it on.
	f.confirm(32)
	if on, _, _ := f.snapshot(); on {
		t.Fatal("a second confirm must be ignored")
	}
}

// Pins §4.2.1 Settle: "window > 0 confirms it: the credit stays as assumed,
// plus anything already granted on sid = 0, counted against what was already
// sent".
func TestFlowSender_ConfirmKeepsAnEarlyGrant(t *testing.T) {
	var f flowSender
	f.assume(wConn)
	f.grant(5) // the peer's raise, landed ahead of its advertisement
	f.confirm(32)
	on, granted, _ := f.snapshot()
	if !on {
		t.Fatal("confirm(32): the window must stay on")
	}
	if granted != int64(wConn)+5 {
		t.Fatalf("granted = %d, want W_conn + 5: confirm must keep what was granted, not replace it", granted)
	}
	// Contrast observe, which is authoritative and replaces (per-stream).
	var g flowSender
	g.assume(wConn)
	g.grant(5)
	g.observe(32)
	if _, granted, _ := g.snapshot(); granted != 32 {
		t.Fatalf("observe replaces: granted = %d, want 32", granted)
	}
}

// Pins §4.2.1 Grants: a sid-0 WINDOW "never enables" — and neither does an
// advertisement toward a sender that assumed nothing.
func TestFlowSender_ConfirmNeverEnablesAnUnassumedSender(t *testing.T) {
	var f flowSender // never assumed: unreliable mode, or a Conn without one
	f.confirm(32)
	if on, _, _ := f.snapshot(); on {
		t.Fatal("an advertisement is not a grant: an unassumed sender stays off")
	}
	f.grant(1000)
	if on, _, _ := f.snapshot(); on {
		t.Fatal("a grant never enables (§4.2.1)")
	}
	f.assume(wConn) // too late: the settle already happened
	if on, _, _ := f.snapshot(); on {
		t.Fatal("assume after the settle must be refused, as after observe")
	}
}

// Pins §4.2.1 Settle: a settle to off makes the sender unlimited — a park
// under the assumption ends with it.
func TestFlowSender_ConfirmWakesAParkedSender(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var f flowSender
		f.assume(1)
		if _, ok := f.tryAcquire(); !ok {
			t.Fatal("the one assumed credit")
		}
		res := make(chan error, 1)
		go func() {
			res <- acquire2(&f, nil, context.Background(), nil, 0, nil)
		}()
		synctest.Wait()
		select {
		case err := <-res:
			t.Fatalf("must be parked at zero credit, got %v", err)
		default:
		}
		f.confirm(0) // the peer does no flow control after all
		if err := <-res; err != nil {
			t.Fatalf("confirm(0) must unpark: %v", err)
		}
	})
}

// Pins §4.2.1 Restart: "the Conn MUST start its sender over — assumed at
// W_conn, unsettled, nothing sent".
func TestFlowSender_ReassumeStartsOver(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var f flowSender
		f.assume(wConn)
		f.confirm(32)
		for range wConn {
			f.tryAcquire() // the whole window, toward the old incarnation
		}
		res := make(chan error, 1)
		go func() {
			res <- acquire2(&f, nil, context.Background(), nil, 0, nil)
		}()
		synctest.Wait()
		select {
		case err := <-res:
			t.Fatalf("must be parked at zero credit, got %v", err)
		default:
		}

		// The new incarnation counts from zero: so does the sender, and
		// whoever was parked re-races on the fresh window.
		f.reassume(wConn)
		if err := <-res; err != nil {
			t.Fatalf("reassume must unpark: %v", err)
		}
		on, granted, sent := f.snapshot()
		if !on || granted != int64(wConn) || sent != 1 {
			t.Fatalf("on/granted/sent = %v/%d/%d, want true/W_conn/1: assumed at W_conn, nothing sent but the woken one", on, granted, sent)
		}

		// Unsettled again: the new incarnation's first advertisement settles
		// it — and 0 turns it off, as it would have the first time.
		f.confirm(0)
		if on, _, _ := f.snapshot(); on {
			t.Fatal("the settle must be due again after a reassume")
		}
	})
}

// Pins §4.2.1 Restart: the Conn must "treat the raise as due again" — the
// new incarnation's sender starts at W_conn and has never seen this window —
// while the ledger itself carries over.
func TestPeerFlowRx_RenewMakesTheRaiseDueAgain(t *testing.T) {
	p := newPeerFlowRx(2048)
	for range 3 {
		p.admit()
	}
	if g := p.raise(); g != 1024 {
		t.Fatalf("raise = %d, want 2048 − 1024", g)
	}
	if g := p.raise(); g != 0 {
		t.Fatalf("second raise = %d, want 0", g)
	}
	p.renew()
	if g := p.raise(); g != 1024 {
		t.Fatalf("raise after renew = %d, want 2048 − 1024 again", g)
	}
	if g := p.excess(); g != 1024 {
		t.Fatalf("excess = %d, want the amount without the latch (the server's per-container raise)", g)
	}
	if out, _ := p.snapshot(); out != 3 {
		t.Fatalf("outstanding = %d after renew, want 3: the dead incarnation's frames still count until they drain", out)
	}
}

// ---------------------------------------------------------------------------
// acquire2: one credit from each window, stream first, refund on a
// connection shortfall, one T_stall across both parks.
// ---------------------------------------------------------------------------

// Pins §4.2.1 Sending: "taken stream first: if the connection window is then
// short, the stream credit is refunded and the sender parks on the connection
// window".
func TestAcquire2_RefundsStreamCreditOnConnectionShortfall(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var stream, conn flowSender
		stream.assume(4)
		conn.assume(1)

		var stalls []bool
		onStall := func(peer bool) { stalls = append(stalls, peer) }

		if err := acquire2(&stream, &conn, context.Background(), nil, 0, onStall); err != nil {
			t.Fatal(err)
		}
		if _, _, sent := stream.snapshot(); sent != 1 {
			t.Fatalf("stream.sent = %d, want 1", sent)
		}
		if _, _, sent := conn.snapshot(); sent != 1 {
			t.Fatalf("conn.sent = %d, want 1", sent)
		}

		res := make(chan error, 1)
		go func() {
			res <- acquire2(&stream, &conn, context.Background(), nil, 0, onStall)
		}()
		synctest.Wait()
		select {
		case err := <-res:
			t.Fatalf("must be parked on the connection window, got %v", err)
		default:
		}
		// Parked on the connection window with NO stream credit held: the
		// stream credit it took was refunded before it parked.
		if _, _, sent := stream.snapshot(); sent != 1 {
			t.Fatalf("stream.sent = %d while parked, want 1 (refunded)", sent)
		}
		if len(stalls) != 1 || !stalls[0] {
			t.Fatalf("onStall calls = %v, want exactly one with peer = true", stalls)
		}

		conn.grant(1)
		if err := <-res; err != nil {
			t.Fatal(err)
		}
		if _, _, sent := stream.snapshot(); sent != 2 {
			t.Fatalf("stream.sent = %d after the grant, want 2", sent)
		}
		if _, _, sent := conn.snapshot(); sent != 2 {
			t.Fatalf("conn.sent = %d after the grant, want 2", sent)
		}
		if len(stalls) != 1 {
			t.Fatalf("onStall must fire once per acquire, got %d", len(stalls))
		}
	})
}

// A stream parked on its own window must not hold connection credit: the
// healthy stream beside it keeps going, and the stuck one's later resume
// still needs — and waits for — connection credit of its own.
// Pins §4.2.1 Sending: "A sender MUST NOT hold one window's credit while
// parked on the other".
func TestAcquire2_StuckStreamHoldsNoConnectionCredit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var stuck, healthy, conn flowSender
		stuck.assume(1)
		healthy.assume(8)
		conn.assume(2)

		if err := acquire2(&stuck, &conn, context.Background(), nil, 0, nil); err != nil {
			t.Fatal(err)
		}
		var peerStall bool
		res := make(chan error, 1)
		go func() {
			res <- acquire2(&stuck, &conn, context.Background(), nil, 0, func(peer bool) { peerStall = peer })
		}()
		synctest.Wait()
		if peerStall {
			t.Fatal("the stream window is the empty one; onStall(peer) must be false")
		}
		if _, _, sent := conn.snapshot(); sent != 1 {
			t.Fatalf("conn.sent = %d with a stream parked, want 1: no connection credit may be held while parked", sent)
		}

		// The healthy stream takes the last connection credit unhindered.
		if err := acquire2(&healthy, &conn, context.Background(), nil, 0, nil); err != nil {
			t.Fatalf("the healthy stream must not be blocked by the stuck one: %v", err)
		}

		// The stuck stream's own grant moves it to the connection park — it
		// must not have skipped the connection check by holding old credit.
		stuck.grant(1)
		synctest.Wait()
		select {
		case err := <-res:
			t.Fatalf("connection window is empty; must still be parked, got %v", err)
		default:
		}
		conn.grant(1)
		if err := <-res; err != nil {
			t.Fatal(err)
		}
		if _, _, sent := conn.snapshot(); sent != 3 {
			t.Fatalf("conn.sent = %d, want 3", sent)
		}
	})
}

// T_stall is one budget across both windows (§10.1): a park that starts on
// the stream window and moves to the connection window fails at T_stall from
// the FIRST park, and the error names the window it was starved on.
// Pins §4.2.1 Sending: "the same T_stall (§10.1), armed at the first park,
// measures the whole wait across both windows, and on expiry the call fails
// UNAVAILABLE naming the window it was parked on".
func TestAcquire2_SingleStallBudgetAcrossBothWindows(t *testing.T) {
	const stall = 4 * time.Second

	synctest.Test(t, func(t *testing.T) {
		var stream, conn flowSender
		stream.assume(1)
		conn.assume(1)
		acquire2(&stream, &conn, context.Background(), nil, 0, nil) // spend both

		start := time.Now()
		res := make(chan error, 1)
		go func() {
			res <- acquire2(&stream, &conn, context.Background(), nil, stall, nil)
		}()
		time.Sleep(stall / 2)
		stream.grant(1) // half-way: the stream window opens, the connection is still shut
		synctest.Wait()
		select {
		case err := <-res:
			t.Fatalf("still short on the connection window, got %v", err)
		default:
		}
		err := <-res
		if got := time.Since(start); got != stall {
			t.Fatalf("the park must end exactly at T_stall from the first park, took %v", got)
		}
		if status.Code(err) != codes.Unavailable {
			t.Fatalf("code = %v, want UNAVAILABLE: %v", status.Code(err), err)
		}
		if !strings.Contains(err.Error(), "connection credit") {
			t.Fatalf("the error must name the connection window: %v", err)
		}
		// The refunded stream credit is still there for the next send.
		if _, _, sent := stream.snapshot(); sent != 1 {
			t.Fatalf("stream.sent = %d after the failed send, want 1 (refunded)", sent)
		}
	})

	// The other way round: a stream-only park names the stream, in the
	// exact words the per-stream acquire has always used.
	synctest.Test(t, func(t *testing.T) {
		var stream, conn flowSender
		stream.assume(1)
		conn.assume(8)
		acquire2(&stream, &conn, context.Background(), nil, 0, nil)
		err := acquire2(&stream, &conn, context.Background(), nil, stall, nil)
		if status.Code(err) != codes.Unavailable {
			t.Fatalf("code = %v, want UNAVAILABLE: %v", status.Code(err), err)
		}
		if !strings.HasSuffix(err.Error(), "the peer granted no credit for "+stall.String()) {
			t.Fatalf("a stream stall keeps the per-stream wording: %v", err)
		}
		if _, _, sent := conn.snapshot(); sent != 1 {
			t.Fatalf("conn.sent = %d, want 1: no connection credit is taken for a send that parked on its stream", sent)
		}
	})
}

// Pins §4.2.1 Sending (per-stream, applying to both): a park is "bounded by
// the call's own ctx/deadline, by the call ending".
func TestAcquire2_GivesUpOnDoneAndCtx(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var stream, conn flowSender
		stream.assume(8)
		conn.assume(1)
		acquire2(&stream, &conn, context.Background(), nil, 0, nil)

		done := make(chan struct{})
		res := make(chan error, 1)
		go func() { res <- acquire2(&stream, &conn, context.Background(), done, time.Hour, nil) }()
		synctest.Wait()
		close(done)
		if err := <-res; !errors.Is(err, errCallEnded) {
			t.Fatalf("call ended: err = %v, want errCallEnded", err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		go func() { res <- acquire2(&stream, &conn, ctx, nil, time.Hour, nil) }()
		synctest.Wait()
		cancel()
		if err := <-res; status.Code(err) != codes.Canceled {
			t.Fatalf("ctx cancelled: code = %v, want CANCELED (%v)", status.Code(err), err)
		}
		// Neither park left a credit behind on either window.
		if _, _, sent := stream.snapshot(); sent != 1 {
			t.Fatalf("stream.sent = %d, want 1", sent)
		}
		if _, _, sent := conn.snapshot(); sent != 1 {
			t.Fatalf("conn.sent = %d, want 1", sent)
		}
	})
}

// Without a connection window (unreliable mode) acquire2 is acquire.
// Pins §4.2.1 Unreliable mode: "has no connection window" — the per-stream
// acquire alone.
func TestAcquire2_NilConnIsPerStreamOnly(t *testing.T) {
	var stream flowSender
	stream.assume(2)
	for i := range 2 {
		if err := acquire2(&stream, nil, context.Background(), nil, 0, nil); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	err := acquire2(&stream, nil, context.Background(), nil, time.Millisecond, nil)
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("code = %v, want UNAVAILABLE: %v", status.Code(err), err)
	}
}

// Limits: the floor at W_conn, for the same reason the rx buffer is floored
// at W_init — a sender assumes it.
// Pins §4.2.1 Assumption: "MaxPeerWindow (§15) is floored at W_conn, for the
// same reason the rx buffer is floored at W_init".
func TestLimits_MaxPeerWindowFlooredAtWConn(t *testing.T) {
	for _, tc := range []struct{ in, want int }{
		{0, int(wConn)}, {-1, int(wConn)}, {100, int(wConn)}, {1024, 1024}, {2048, 2048},
	} {
		if got := (Limits{MaxPeerWindow: tc.in}).withDefaults().MaxPeerWindow; got != tc.want {
			t.Errorf("MaxPeerWindow %d → %d, want %d", tc.in, got, tc.want)
		}
	}
}

// The two peer-scope stall events count beside, not into, the per-stream
// ones.
// Pins §14: "flow-stall counters (per stream and per peer, §4.2.1)" — the two
// peer kinds count beside, not into, the per-stream pair.
func TestCounters_PeerFlowEvents(t *testing.T) {
	var c Counters
	c.ProtocolEvent(ProtocolEvent{Kind: EventPeerFlowStall, Sid: 3})
	c.ProtocolEvent(ProtocolEvent{Kind: EventPeerFlowResume, Sid: 3})
	c.ProtocolEvent(ProtocolEvent{Kind: EventPeerFlowResume, Sid: 3})
	s := c.Snapshot()
	if s.PeerFlowStall != 1 || s.PeerFlowResume != 2 {
		t.Fatalf("PeerFlowStall/Resume = %d/%d, want 1/2", s.PeerFlowStall, s.PeerFlowResume)
	}
	if s.FlowStall != 0 || s.FlowResume != 0 {
		t.Fatalf("the per-stream counters must be untouched: %d/%d", s.FlowStall, s.FlowResume)
	}
	if EventPeerFlowStall.String() != "peer-flow-stall" || EventPeerFlowResume.String() != "peer-flow-resume" {
		t.Fatalf("names: %q %q", EventPeerFlowStall, EventPeerFlowResume)
	}
}

// ---------------------------------------------------------------------------
// acquire2 on a call that has already ended: the fast path re-checks done
// after taking credit and refunds both windows. The connection window is
// shared by every call to the peer and cumulative, so a dead call's send
// must spend nothing on it.
// ---------------------------------------------------------------------------

// Pins §4.2.1 Sending: "a send that never reaches the wire — the adapter
// refused it (§4.4), or the call ended first — refunds both".
func TestAcquire2_DeadCallSpendsNothing(t *testing.T) {
	var stream, conn flowSender
	stream.assume(8)
	conn.assume(8)
	done := make(chan struct{})
	close(done)
	if err := acquire2(&stream, &conn, context.Background(), done, time.Hour, nil); !errors.Is(err, errCallEnded) {
		t.Fatalf("done already closed: err = %v, want errCallEnded", err)
	}
	if _, _, sent := stream.snapshot(); sent != 0 {
		t.Fatalf("stream.sent = %d, want 0", sent)
	}
	if _, _, sent := conn.snapshot(); sent != 0 {
		t.Fatalf("conn.sent = %d, want 0: a dead call must not spend the shared window", sent)
	}
	// Without a connection window too.
	if err := acquire2(&stream, nil, context.Background(), done, time.Hour, nil); !errors.Is(err, errCallEnded) {
		t.Fatalf("nil conn: err = %v, want errCallEnded", err)
	}
	if _, _, sent := stream.snapshot(); sent != 0 {
		t.Fatalf("stream.sent = %d, want 0", sent)
	}
}

// Pins §4.2.1 Sending / §14: a send short on both windows "is reported as a
// connection stall — the peer's whole budget is what it waits on, not one
// consumer"; short on the stream alone, it is a stream stall.
func TestAcquire2_ShortOnBothReportsTheConnectionWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const stall = time.Second
		var stream, conn flowSender
		stream.assume(1)
		conn.assume(1)
		acquire2(&stream, &conn, context.Background(), nil, 0, nil) // both spent

		stalls := make(chan bool, 1)
		res := make(chan error, 1)
		go func() {
			res <- acquire2(&stream, &conn, context.Background(), nil, stall, func(p bool) { stalls <- p })
		}()
		synctest.Wait()
		if peer := <-stalls; !peer {
			t.Fatal("short on both windows: onStall must report the connection one")
		}
		err := <-res
		if !strings.Contains(err.Error(), "connection credit") {
			t.Fatalf("expiry names the connection window by the same rule, got: %v", err)
		}
		if _, _, sent := stream.snapshot(); sent != 1 {
			t.Fatalf("stream.sent = %d, want 1: the park held nothing", sent)
		}

		// The connection window has credit, the stream does not: the stream.
		conn.grant(1)
		go func() {
			res <- acquire2(&stream, &conn, context.Background(), nil, stall, func(p bool) { stalls <- p })
		}()
		synctest.Wait()
		if peer := <-stalls; peer {
			t.Fatal("short on the stream alone: onStall must report the stream")
		}
		stream.grant(1)
		if err := <-res; err != nil {
			t.Fatalf("resumed on the stream grant: %v", err)
		}
		if _, _, sent := conn.snapshot(); sent != 2 {
			t.Fatalf("conn.sent = %d, want 2", sent)
		}
	})
}

// ---------------------------------------------------------------------------
// peerFlowRx across incarnations: outstanding is one bound, pending is per
// incarnation, so a dead incarnation's bulk return never carries a live one's
// credit away.
// ---------------------------------------------------------------------------

// Pins §4.2.1 Cadence: "credit is held back and granted per incarnation:
// each is granted exactly what its own frames returned".
func TestPeerFlowRx_CreditIsHeldPerIncarnation(t *testing.T) {
	const dead, live = uint32(0xD), uint32(0x1)
	p := newPeerFlowRx(wConn)
	// The dead incarnation left 300 in the buffers under a stuck consumer;
	// the live one sent 300 that were consumed promptly.
	for range 600 {
		p.admit()
	}
	for range 300 {
		if g := p.retire(live, 1, true); g != 0 {
			t.Fatalf("300 held for the live one, 300 buffered: nothing due, got %d", g)
		}
	}
	// The dead one's stuck call ends and its 300 are discarded in bulk: that
	// credit is its own — nothing of the live one's rides along, and none is
	// due (300 < 512, and the buffers have room).
	if g := p.retire(dead, 300, true); g != 0 {
		t.Fatalf("the dead incarnation's bulk return is its own: nothing due, got %d", g)
	}
	if got := p.pendingOf(live); got != 300 {
		t.Fatalf("live pending = %d, want its 300 untouched by the dead one's return", got)
	}
	if got := p.pendingOf(dead); got != 300 {
		t.Fatalf("dead pending = %d, want 300", got)
	}
	// The live one reaches its own half window and is granted exactly that
	// — 512, not 812.
	for i := range 212 {
		p.admit()
		g := p.retire(live, 1, true)
		if i < 211 && g != 0 {
			t.Fatalf("message %d: nothing due yet, got %d", i, g)
		}
		if i == 211 && g != wConn/2 {
			t.Fatalf("the live one's half window: grant = %d, want %d", g, wConn/2)
		}
	}
	if got := p.pendingOf(dead); got != 300 {
		t.Fatalf("dead pending = %d after the live one's grant, want 300 still", got)
	}

	// The starvation clause reads the shared outstanding: with the dead
	// one's frames pinning all but one slot, the live one's single consumed
	// message is granted at once — the window is full for everyone.
	q := newPeerFlowRx(wConn)
	for range wConn {
		q.admit()
	}
	if g := q.retire(live, 1, true); g != 1 {
		t.Fatalf("starvation clause across incarnations: want a grant of 1, got %d", g)
	}

	// renew (client: a new server incarnation) drops everything held back.
	q.retire(dead, 5, true)
	q.renew()
	if _, pend := q.snapshot(); pend != 0 {
		t.Fatalf("pending = %d after renew, want 0: the old incarnation's credit has no one to go to", pend)
	}
}

// ---------------------------------------------------------------------------
// The eviction stash: a container the MaxDeadPeers cap evicts leaves its
// sender's position in the ledger, a grant addressed to it still lands, and
// past the cap the oldest is dropped with the credit held back for it.
// ---------------------------------------------------------------------------

// Pins §9.4 / §15: "the ledger keeps the evicted container's connection
// sender ... so that the incarnation's next OPEN continues it".
func TestPeerFlowRx_StashHoldsAnEvictedSender(t *testing.T) {
	p := &peerFlowRx{}
	p.enable(4*wConn, 2)
	var tx flowSender
	tx.assume(wConn)
	tx.confirm(32)
	tx.grant(3 * wConn)
	for range 5 {
		tx.tryAcquire()
	}
	p.unadmitted(1, 10) // held back for incarnation 1
	p.stash(1, tx.state(), true)
	p.creditEvicted(1, 7) // the client returns credit while it is evicted
	// A RESET-drawn data frame it sent while evicted returns its credit to
	// the held position; one from an incarnation held nowhere returns
	// nothing and creates nothing.
	if g := p.unadmittedEvicted(1, 3); g != 0 {
		t.Fatalf("unadmittedEvicted(1, 3) = %d, want 0: below half the window", g)
	}
	if g := p.unadmittedEvicted(5, 3); g != 0 || p.pendingOf(5) != 0 {
		t.Fatalf("unadmittedEvicted(5, 3) = %d, pending %d; want nothing: held nowhere", g, p.pendingOf(5))
	}

	st, raised, ok := p.unstash(1)
	if !ok || !raised {
		t.Fatalf("unstash(1) = ok %v, raised %v; want the held position, raised", ok, raised)
	}
	if !st.on || !st.observed || st.granted != int64(4*wConn+7) || st.sent != 5 {
		t.Fatalf("state = %+v; want on, settled, granted 4096+7, sent 5", st)
	}
	if got := p.pendingOf(1); got != 13 {
		t.Fatalf("pending held for the evicted incarnation = %d, want 10 + 3 while evicted", got)
	}
	if _, _, ok := p.unstash(1); ok {
		t.Fatal("unstash hands a position back once")
	}

	// Past the cap (2) the oldest goes, held-back credit included.
	for epoch := uint32(2); epoch <= 4; epoch++ {
		p.unadmitted(epoch, 1)
		p.stash(epoch, tx.state(), false)
	}
	if _, _, ok := p.unstash(2); ok {
		t.Fatal("the oldest of three on a cap of two must be gone")
	}
	if got := p.pendingOf(2); got != 0 {
		t.Fatalf("pending of the dropped incarnation = %d, want 0", got)
	}
	for epoch := uint32(3); epoch <= 4; epoch++ {
		if _, _, ok := p.unstash(epoch); !ok {
			t.Fatalf("incarnation %d must still be held", epoch)
		}
	}

	// A grant never enables: a stashed sender that is off stays off.
	var off flowSender
	p.stash(9, off.state(), false)
	p.creditEvicted(9, 5)
	if st, _, _ := p.unstash(9); st.on || st.granted != 0 {
		t.Fatalf("state = %+v; want off with nothing granted", st)
	}

	// Enough RESET-drawn frames while evicted tip the batch: the grant is
	// due, and it is the held incarnation's.
	p.stash(10, tx.state(), false)
	if g := p.unadmittedEvicted(10, 2*wConn); g != 2*wConn {
		t.Fatalf("unadmittedEvicted(10, half the window) = %d, want %d", g, 2*wConn)
	}
	if got := p.pendingOf(10); got != 0 {
		t.Fatalf("pending of the granted incarnation = %d, want 0", got)
	}
	if _, _, ok := p.unstash(10); !ok {
		t.Fatal("the grant does not drop the position")
	}

	// A ledger with no cap (the client's) holds nothing.
	c := newPeerFlowRx(wConn)
	c.unadmitted(1, 3)
	c.stash(1, tx.state(), false)
	if _, _, ok := c.unstash(1); ok {
		t.Fatal("cap 0: nothing is held")
	}
	if got := c.pendingOf(1); got != 0 {
		t.Fatalf("cap 0: pending = %d, want 0", got)
	}
}
