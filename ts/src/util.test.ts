// The concurrency primitives (src/util.ts). The FIFO test regression-pins the
// audit finding that putBlocking could let a later putter steal a freed slot
// from an earlier parked one, reordering reliable-mode delivery (§14).
//
// The second half is the twin of Go's flow_unit_test.go: white-box tests of
// the connection-window primitives (PROTOCOL.md §4.2.1, reliable mode only),
// no transport — the sender's adoption of the peer's advertisement (observe)
// and its restart (reassume), the combined acquire over both windows
// (acquireBoth), and the receiver's physical ledger (PeerFlowRx). Each pins
// one normative sentence the end-to-end tests of test/flow.test.ts can only
// observe indirectly.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { acquireBoth, FlowSender, FrameQueue, Latch, PeerFlowRx, tryAcquireBoth, W_CONN, type FlowAcquireBoth } from './util'
import { frame, type Frame } from './wire'
import { tick } from './testing'

const F = (seq: number): Frame => frame({ seq })

describe('Latch', () => {
  it('is a one-shot broadcast with no lost wakeup (late waiters see it tripped)', async () => {
    const l = new Latch()
    expect(l.tripped).toBe(false)
    const early = l.wait()
    l.trip()
    l.trip() // idempotent
    expect(l.tripped).toBe(true)
    await early
    await l.wait() // a waiter registered after the trip still resolves
  })
})

describe('FrameQueue.putBlocking FIFO (§14)', () => {
  it('preserves call order when multiple putters block on a full buffer', async () => {
    const q = new FrameQueue(1)
    const done = new Latch()
    expect(q.tryPut(F(0))).toBe(true) // fill the single slot

    // Two putters block: with the pre-fix wake-all+race, F2 could win the slot
    // freed for F1; the FIFO chain forbids it.
    const p1 = q.putBlocking(F(1), done)
    const p2 = q.putBlocking(F(2), done)

    const taken: number[] = []
    taken.push(q.tryTake()!.seq) // remove F0 → frees the slot for the head putter
    expect(await p1).toBe(true)
    taken.push(q.tryTake()!.seq) // F1 (the head putter), not F2
    expect(await p2).toBe(true)
    taken.push(q.tryTake()!.seq) // F2

    expect(taken).toEqual([0, 1, 2])
  })

  it('a blocked putter returns true when the stream ends (done)', async () => {
    const q = new FrameQueue(1)
    const done = new Latch()
    q.tryPut(F(0))
    const p = q.putBlocking(F(1), done)
    done.trip() // the call finished; the frame is moot
    expect(await p).toBe(true)
  })

  it('a blocked putter returns false when its rx signal aborts (teardown)', async () => {
    const q = new FrameQueue(1)
    const done = new Latch()
    const ac = new AbortController()
    q.tryPut(F(0))
    const p = q.putBlocking(F(1), done, ac.signal)
    ac.abort()
    expect(await p).toBe(false) // fail-loud path: the frame is lost mid-delivery
  })
})

describe('FrameQueue.putDrop', () => {
  it('DropNewest keeps the buffered prefix and counts the drop', () => {
    const q = new FrameQueue(2)
    // The return value is what the caller reports as a 'dropped' event (§14):
    // how many frames THIS put cost, whether or not it was admitted.
    expect(q.putDrop(F(1), 0 /* Newest */)).toBe(0)
    expect(q.putDrop(F(2), 0)).toBe(0)
    expect(q.putDrop(F(3), 0)).toBe(1) // dropped
    expect([q.tryTake()!.seq, q.tryTake()!.seq]).toEqual([1, 2])
    expect(q.dropped).toBe(1)
  })

  it('DropOldest evicts the oldest to admit the newest', () => {
    const q = new FrameQueue(2)
    expect(q.putDrop(F(1), 1 /* Oldest */)).toBe(0)
    expect(q.putDrop(F(2), 1)).toBe(0)
    expect(q.putDrop(F(3), 1)).toBe(1) // evicts F1, admits F3: one frame lost
    expect([q.tryTake()!.seq, q.tryTake()!.seq]).toEqual([2, 3])
    expect(q.dropped).toBe(1)
  })
})

// ---------------------------------------------------------------------------
// flow control primitives (§4.2.1) — the twin of flow_unit_test.go
// ---------------------------------------------------------------------------

// epochT is the one peer incarnation the single-incarnation ledger tests
// return credit for.
const epochT = 0xe

function newPeerFlowRx(window: number, evictCap = 0): PeerFlowRx {
  const p = new PeerFlowRx()
  p.enable(window, evictCap)
  return p
}

// snapshot reads the ledger: outstanding, and the credit held back across
// every incarnation. pendingOf reads one incarnation's. White-box, as the
// Go tests are.
function snapshot(p: PeerFlowRx): { outstanding: number; pending: number } {
  let pending = 0
  for (const n of p['pending'].values()) pending += n
  return { outstanding: p['outstanding'], pending }
}
const pendingOf = (p: PeerFlowRx, epoch: number): number => p['pending'].get(epoch) ?? 0

// settled reports whether a park has ended, without awaiting it.
function watch<T>(p: Promise<T>): { get settled(): boolean; result: Promise<T> } {
  let done = false
  const result = p.finally(() => {
    done = true
  })
  return {
    get settled() {
      return done
    },
    result,
  }
}

// PeerFlowRx: the ledger. outstanding is what the peer has in our buffers —
// it never passes the window (admit refuses) and never underflows (a retire
// past it saturates rather than opening the bound).
describe('PeerFlowRx (§4.2.1)', () => {
  // Pins §4.2.1 The receiver's ledger: "A receiver MUST NOT hold more than
  // MaxPeerWindow buffered messages from one peer".
  it('admit refuses past the window and a retire past zero saturates', () => {
    const p = newPeerFlowRx(8)
    for (let i = 0; i < 8; i++) expect(p.admit(), `frame ${i}: ${i} of 8 outstanding`).toBe(true)
    expect(p.admit(), 'the 9th would take outstanding past the window').toBe(false)
    expect(snapshot(p).outstanding, 'a refused admit charges nothing').toBe(8)

    // Retiring more than was ever admitted saturates at zero: the bound is
    // enforced on outstanding, so it must never wrap into "unlimited".
    expect(p.retire(epochT, 20, false)).toBe(0)
    expect(snapshot(p)).toEqual({ outstanding: 0, pending: 0 })
    expect(p.admit(), 'room again after the retire').toBe(true)
  })

  // Pins §4.2.1 The receiver's ledger: no credit for "data frames of a
  // server incarnation the Conn has moved past".
  it('retire(credit = false) grants nothing and counts toward no later batch', () => {
    const p = newPeerFlowRx(8)
    for (let i = 0; i < 8; i++) p.admit()
    expect(p.retire(epochT, 8, false)).toBe(0)
    expect(snapshot(p).pending).toBe(0)
    // A credited retire afterwards starts from zero pending: the uncredited
    // frames are not counted in later batching either.
    for (let i = 0; i < 4; i++) p.admit()
    expect(p.retire(epochT, 3, true), '3 pending of a window of 8, 1 outstanding: nothing due').toBe(0)
    expect(p.retire(epochT, 1, true), '4 pending = half the window').toBe(4)
    expect(snapshot(p)).toEqual({ outstanding: 0, pending: 0 })
  })

  // §4.2.1's starvation MUST applied to the connection window: with stuck
  // consumers pinning most of it, pending can never reach half the window
  // while the sender is out of credit — so a grant fires whenever
  // outstanding + pending reaches the window, even for a single message.
  // Pins §4.2.1 Cadence: "It MUST grant, whatever it holds back, whenever
  // buffered + held back ≥ MaxPeerWindow ... at that edge the cost is one
  // grant per message consumed".
  it('the starvation clause grants below half the window', () => {
    const p = newPeerFlowRx(32)
    for (let i = 0; i < 32; i++) p.admit() // the peer has spent its whole window
    // One message consumed on the healthy stream: pending*2 = 2 < 32, yet
    // outstanding + pending = 31 + 1 = 32 >= 32 — the sender is at zero
    // credit and everything it could wait for is this one message.
    expect(p.retire(epochT, 1, true)).toBe(1)
    // And it keeps firing one per message while the stuck consumers pin the
    // rest — bounded by the consumption rate, never silent.
    for (let i = 0; i < 5; i++) {
      expect(p.admit(), `round ${i}: the granted credit is admissible`).toBe(true)
      expect(p.retire(epochT, 1, true), `round ${i}`).toBe(1)
    }
    // With the sender at zero credit nothing is pending, and one message
    // consumed without a new arrival is credit the sender did not need yet
    // (31 + 1 < 32): batching resumes.
    expect(p.retire(epochT, 1, true)).toBe(0)

    // Without the clause's condition the half-window batching alone rules:
    // 16 outstanding, 1 pending → 17 < 32 and 2 < 32 → nothing due.
    const q = newPeerFlowRx(32)
    for (let i = 0; i < 17; i++) q.admit()
    expect(q.retire(epochT, 1, true)).toBe(0)
  })

  // Pins §4.2.1 Cadence: "A receiver SHOULD batch: grant once the credit it
  // holds back reaches half its window".
  it('batches at half the window', () => {
    const p = newPeerFlowRx(W_CONN)
    for (let i = 0; i < 600; i++) p.admit()
    let total = 0
    let grants = 0
    for (let i = 0; i < 600; i++) {
      const g = p.retire(epochT, 1, true)
      if (g !== 0) {
        total += g
        grants++
      }
    }
    expect([grants, total], '600 consumed against 1024 batches into one grant of 512').toEqual([1, 512])
    // The 88 left over wait for the next half window, as FlowReceiver's do:
    // the peer has 1024 − 88 credits and is nowhere near starving.
    expect(snapshot(p).pending).toBe(88)
  })

  // Frames that were never admitted — RESET-drawn, unknown or tombstoned sid
  // — return their credit without touching outstanding: the sender spent it,
  // and a window that never gets it back is a permanent shrink (§10.6).
  // Pins §4.2.1 The receiver's ledger: "The bound is enforced on what is
  // buffered, never on what was granted ... and junk cannot desync the
  // ledger".
  it('unadmitted returns credit without touching outstanding', () => {
    const p = newPeerFlowRx(8)
    for (let i = 0; i < 3; i++) p.admit()
    expect(p.unadmitted(epochT, 3), '3 pending < half of 8 and 3 + 3 < 8').toBe(0)
    expect(snapshot(p), 'unadmitted must not lower outstanding').toEqual({ outstanding: 3, pending: 3 })
    expect(p.unadmitted(epochT, 1), '4 pending = half the window').toBe(4)
    expect(snapshot(p).outstanding).toBe(3)
  })

  // The counters are uint32 on the wire (§7): a return past that saturates,
  // never wraps.
  it('held-back credit saturates at uint32', () => {
    const p = newPeerFlowRx(8)
    expect(p.unadmitted(epochT, 2 ** 40)).toBe(0xffff_ffff)
    expect(snapshot(p).pending).toBe(0)
  })

  // So does the window (Go's is a uint32 parameter): it is what the
  // advertisement puts on the wire and what the grant rule measures against,
  // and an unbounded one would do neither — no threshold is ever met against
  // Infinity.
  it('the window saturates at uint32, what the advertisement carries: the grant rule stays reachable', () => {
    const p = newPeerFlowRx(Infinity)
    expect(p.active).toBe(true)
    expect(p['window'], 'the ledger enforces exactly what the advertisement says').toBe(0xffff_ffff)
    expect(p.unadmitted(epochT, 2 ** 40), 'half the window is a reachable amount').toBe(0xffff_ffff)
  })

  // Pins §4.2.1 Unreliable mode: "no advertisement, no assumption, no
  // ledger".
  it('window 0 is off: admits everything, grants nothing, counts nothing', () => {
    const p = newPeerFlowRx(0) // unreliable mode: no bound, no grants
    expect(p.active).toBe(false)
    for (let i = 0; i < 3000; i++) expect(p.admit()).toBe(true)
    expect(p.retire(epochT, 3000, true)).toBe(0)
    expect(p.unadmitted(epochT, 3000)).toBe(0)
    expect(snapshot(p)).toEqual({ outstanding: 0, pending: 0 })
  })

  // Pins §4.2.1 Restart: the Conn must "drop grants naming the old epoch, and
  // stop returning credit for frames of the incarnation it moved past" —
  // what the ledger held back for the dead incarnation is dropped, while the
  // ledger itself carries over.
  it('renew drops the credit held back for the dead incarnation and keeps outstanding', () => {
    const p = newPeerFlowRx(2048)
    for (let i = 0; i < 3; i++) p.admit()
    expect(p.unadmitted(epochT, 5), 'held back, nothing due').toBe(0)
    expect(pendingOf(p, epochT)).toBe(5)
    p.renew()
    expect(pendingOf(p, epochT), "the dead incarnation's credit has no one to go to").toBe(0)
    expect(snapshot(p).outstanding, "the dead incarnation's frames still count until they drain").toBe(3)
  })

  // outstanding is one bound, pending is per incarnation, so a dead
  // incarnation's bulk return never carries a live one's credit away.
  // Pins §4.2.1 Cadence: "credit is held back and granted per incarnation:
  // each is granted exactly what its own frames returned".
  it('holds credit per incarnation', () => {
    const dead = 0xd
    const live = 0x1
    const p = newPeerFlowRx(W_CONN)
    // The dead incarnation left 300 in the buffers under a stuck consumer;
    // the live one sent 300 that were consumed promptly.
    for (let i = 0; i < 600; i++) p.admit()
    for (let i = 0; i < 300; i++) expect(p.retire(live, 1, true), '300 held, 300 buffered: nothing due').toBe(0)
    // The dead one's stuck call ends and its 300 are discarded in bulk: that
    // credit is its own — nothing of the live one's rides along, and none is
    // due (300 < 512, and the buffers have room).
    expect(p.retire(dead, 300, true)).toBe(0)
    expect(pendingOf(p, live)).toBe(300)
    expect(pendingOf(p, dead)).toBe(300)
    // The live one reaches its own half window and is granted exactly that
    // — 512, not 812.
    for (let i = 0; i < 212; i++) {
      p.admit()
      const g = p.retire(live, 1, true)
      expect(g, `message ${i}`).toBe(i === 211 ? W_CONN / 2 : 0)
    }
    expect(pendingOf(p, dead), 'still held for the dead one').toBe(300)

    // The starvation clause reads the shared outstanding: with the dead
    // one's frames pinning all but one slot, the live one's single consumed
    // message is granted at once — the window is full for everyone.
    const q = newPeerFlowRx(W_CONN)
    for (let i = 0; i < W_CONN; i++) q.admit()
    expect(q.retire(live, 1, true)).toBe(1)

    // renew (client: a new server incarnation) drops everything held back.
    q.retire(dead, 5, true)
    q.renew()
    expect(snapshot(q).pending, 'the old incarnation\'s credit has no one to go to').toBe(0)
  })

  // The eviction stash: a container the maxDeadPeers cap evicts leaves its
  // sender's position in the ledger, a grant addressed to it still lands,
  // and past the cap the oldest is dropped with the credit held back for it.
  // Pins §9.4 / §15: "the ledger keeps the evicted container's connection
  // sender ... so that the incarnation's next OPEN continues it".
  it('stash holds an evicted sender, bounded at evictCap, oldest first', () => {
    const p = newPeerFlowRx(4 * W_CONN, 2)
    // The server's sender: created off, adopted from the OPEN's
    // advertisement, lifted by the client's grants.
    const tx = new FlowSender()
    tx.observe(W_CONN)
    tx.grant(3 * W_CONN)
    for (let i = 0; i < 5; i++) tx.tryAcquire()
    p.unadmitted(1, 10) // held back for incarnation 1
    p.stash(1, tx.state())
    p.creditEvicted(1, 7) // the client returns credit while it is evicted
    // A RESET-drawn data frame it sent while evicted returns its credit to
    // the held position; one from an incarnation held nowhere returns
    // nothing and creates nothing.
    expect(p.unadmittedEvicted(1, 3), 'below half the window').toBe(0)
    expect(p.unadmittedEvicted(5, 3), 'held nowhere').toBe(0)
    expect(pendingOf(p, 5)).toBe(0)

    const held = p.unstash(1)
    expect(held, 'the position: window, credit and the advertisement latch').toEqual({ on: true, observed: true, granted: 4 * W_CONN + 7, sent: 5 })
    expect(pendingOf(p, 1), '10 + 3 while evicted').toBe(13)
    expect(p.unstash(1), 'a position is handed back once').toBeUndefined()

    // Past the cap (2) the oldest goes, held-back credit included.
    for (let epoch = 2; epoch <= 4; epoch++) {
      p.unadmitted(epoch, 1)
      p.stash(epoch, tx.state())
    }
    expect(p.unstash(2), 'the oldest of three on a cap of two').toBeUndefined()
    expect(pendingOf(p, 2)).toBe(0)
    for (let epoch = 3; epoch <= 4; epoch++) expect(p.unstash(epoch), `incarnation ${epoch}`).toBeDefined()

    // A grant never enables: a stashed sender that is off stays off.
    p.stash(9, new FlowSender().state())
    p.creditEvicted(9, 5)
    expect(p.unstash(9)).toMatchObject({ on: false, granted: 0 })

    // Enough RESET-drawn frames while evicted tip the batch: the grant is
    // due, and it is the held incarnation's.
    p.stash(10, tx.state())
    expect(p.unadmittedEvicted(10, 2 * W_CONN), 'half the window').toBe(2 * W_CONN)
    expect(pendingOf(p, 10)).toBe(0)
    expect(p.unstash(10), 'the grant does not drop the position').toBeDefined()

    // A ledger with no cap (the client's) holds nothing.
    const c = newPeerFlowRx(W_CONN)
    c.unadmitted(1, 3)
    c.stash(1, tx.state())
    expect(c.unstash(1)).toBeUndefined()
    expect(pendingOf(c, 1)).toBe(0)
  })

  // A re-stash of an incarnation still held keeps its place in the eviction
  // order, as Go's evictedOrder does.
  it('a re-stash keeps its eviction order', () => {
    const p = newPeerFlowRx(W_CONN, 2)
    const st = new FlowSender().state()
    p.stash(1, st)
    p.stash(2, st)
    p.stash(1, st) // still the oldest
    p.stash(3, st) // evicts 1, not 2
    expect(p.unstash(1)).toBeUndefined()
    expect(p.unstash(2)).toBeDefined()
    expect(p.unstash(3)).toBeDefined()
  })
})

// FlowSender.observe: the advertisement. The first one heard from a peer
// incarnation is adopted — authoritative, replacing the assumption, 0 meaning
// off — and the rest are ignored; a grant never enables; reassume starts over
// for a new incarnation.
describe('FlowSender.observe / reassume (§4.2.1 Advertisement, Initial window, Restart)', () => {
  // Pins §4.2.1 Initial window: "The advertisement is authoritative and
  // replaces the assumption, counted against what the sender has already
  // sent ... a smaller window simply parks the sender until the receiver
  // drains".
  it('observe replaces the assumption, counted against what was already sent', () => {
    const f = new FlowSender()
    f.assume(W_CONN)
    for (let i = 0; i < 100; i++) expect(f.tryAcquire()).toBe(true)
    f.observe(2048) // the server's first H: a larger window
    expect(f.state()).toMatchObject({ on: true, observed: true, granted: 2048, sent: 100 })
    for (let i = 100; i < 2048; i++) expect(f.tryAcquire(), `send ${i}`).toBe(true)
    expect(f.tryAcquire(), 'the 2049th parks').toBe(false)

    const g = new FlowSender()
    g.assume(W_CONN)
    for (let i = 0; i < 100; i++) g.tryAcquire()
    g.observe(50) // a smaller one: already past it
    expect(g.empty(), 'parked until the receiver drains').toBe(true)
    g.grant(51)
    expect(g.tryAcquire()).toBe(true)
  })

  // Pins §4.2.1 Advertisement: "A conn_window of 0 (absent) on one of those
  // frames means 'this peer does no connection flow control': the sender's
  // connection window is then off", and Grants: "A grant toward a window
  // that is off is dropped".
  it('observe(0) turns the window off and drops later grants; a later advertisement cannot turn it on', () => {
    const f = new FlowSender()
    f.assume(W_CONN)
    f.observe(0)
    f.grant(5)
    expect(f.state()).toMatchObject({ on: false, observed: true })
    // Off means unlimited: far past W_conn without a park.
    for (let i = 0; i < 3 * W_CONN; i++) expect(f.tryAcquire(), `send ${i}`).toBe(true)
    expect(f.empty()).toBe(false)
    // Once only: the first advertisement wins, the rest are ignored.
    f.observe(32)
    expect(f.state().on).toBe(false)
  })

  // Pins §4.2.1 Advertisement: "A peer applies the first advertisement it
  // hears from a peer incarnation and ignores the rest".
  it('the first advertisement wins: a later one with another value is ignored', () => {
    const f = new FlowSender()
    f.observe(2048)
    f.observe(8192)
    f.observe(0)
    expect(f.state()).toMatchObject({ on: true, observed: true, granted: 2048 })
  })

  // Pins §4.2.1 Initial window: "the server's sender is created by an OPEN
  // that carries the advertisement (§9.4), so it never assumes" — an
  // unassumed sender is enabled by the advertisement alone — and Grants: a
  // sid-0 WINDOW "never enables".
  it("observe enables a sender that never assumed (the server's); a grant alone never does", () => {
    const f = new FlowSender() // the server's, created off by its container
    f.grant(1000)
    expect(f.state().on, 'a grant never enables').toBe(false)
    f.observe(2048) // the OPEN's advertisement
    expect(f.state()).toMatchObject({ on: true, observed: true, granted: 2048, sent: 0 })
    f.assume(W_CONN) // too late, and never the server's to do
    expect(f.state().granted, 'assume after the advertisement is refused').toBe(2048)
  })

  // Pins §4.2.1 Advertisement: an advertisement of 0 makes the sender
  // unlimited — a park under the assumption ends with it.
  it('observe(0) wakes a parked sender: the peer does no connection flow control after all', async () => {
    const f = new FlowSender()
    f.assume(1)
    expect(f.tryAcquire()).toBe(true)
    expect(f.empty()).toBe(true)
    const park = watch(acquireBoth(f, undefined, new Latch(), 0))
    await tick()
    expect(park.settled, 'parked at zero credit').toBe(false)
    f.observe(0)
    expect(await park.result).toBe('ok')
  })

  // Pins §4.2.1 Initial window: a larger advertisement wakes a sender parked
  // on the spent assumption, onto the advertised credit.
  it('a larger advertisement wakes a parked sender onto the advertised credit', async () => {
    const f = new FlowSender()
    f.assume(1)
    f.tryAcquire()
    const park = watch(acquireBoth(f, undefined, new Latch(), 0))
    await tick()
    expect(park.settled).toBe(false)
    f.observe(2)
    expect(await park.result).toBe('ok')
    expect(f.state()).toMatchObject({ granted: 2, sent: 2 })
  })

  // Pins §4.2.1 Restart: "the Conn MUST start its sender over — assumed at
  // W_conn, unadvertised, nothing sent".
  it('reassume starts over, unadvertised, and wakes the parked sender onto the fresh window', async () => {
    const f = new FlowSender()
    f.assume(W_CONN)
    f.observe(W_CONN)
    for (let i = 0; i < W_CONN; i++) f.tryAcquire() // the whole window, toward the old incarnation
    const park = watch(acquireBoth(f, undefined, new Latch(), 0))
    await tick()
    expect(park.settled).toBe(false)
    // The new incarnation counts from zero: so does the sender, and whoever
    // was parked re-races on the fresh window.
    f.reassume(W_CONN)
    expect(await park.result).toBe('ok')
    expect(f.state(), 'assumed at W_conn, unadvertised, nothing sent but the woken one').toMatchObject({ on: true, observed: false, granted: W_CONN, sent: 1 })
    // Unadvertised again: the new incarnation's first H or T is adopted —
    // and 0 turns it off, as it would have the first time.
    f.observe(0)
    expect(f.state().on).toBe(false)
  })

  it('state / restore carry a position across a fresh sender, latch included', () => {
    const f = new FlowSender()
    f.observe(W_CONN)
    f.grant(3)
    f.tryAcquire()
    const g = new FlowSender()
    g.restore(f.state())
    expect(g.state()).toEqual({ on: true, observed: true, granted: W_CONN + 3, sent: 1 })
    g.observe(0) // already advertised: a returning OPEN's value is ignored, the position stands
    expect(g.state().on).toBe(true)
  })
})

// acquireBoth: one credit from each window, stream first, refund on a
// connection shortfall, one T_stall across both parks.
describe('acquireBoth (§4.2.1)', () => {
  const noStall = 0
  const latch = () => new Latch()

  // Pins §4.2.1 Sending: "taken stream first: if the connection window is
  // then short, the stream credit is refunded and the sender parks on the
  // connection window".
  it('refunds the stream credit on a connection shortfall', async () => {
    const stream = new FlowSender()
    const conn = new FlowSender()
    stream.assume(4)
    conn.assume(1)
    const stalls: boolean[] = []
    const onStall = (peer: boolean) => void stalls.push(peer)

    expect(await acquireBoth(stream, conn, latch(), noStall, undefined, onStall)).toBe('ok')
    expect(stream.state().sent).toBe(1)
    expect(conn.state().sent).toBe(1)

    const park = watch(acquireBoth(stream, conn, latch(), noStall, undefined, onStall))
    await tick()
    expect(park.settled, 'parked on the connection window').toBe(false)
    // Parked on the connection window with NO stream credit held: the
    // stream credit it took was refunded before it parked.
    expect(stream.state().sent, 'refunded').toBe(1)
    expect(stalls, 'exactly one, peer = true').toEqual([true])

    conn.grant(1)
    expect(await park.result).toBe('ok')
    expect(stream.state().sent).toBe(2)
    expect(conn.state().sent).toBe(2)
    expect(stalls, 'onStall fires once per acquire').toHaveLength(1)
  })

  // A stream parked on its own window must not hold connection credit: the
  // healthy stream beside it keeps going, and the stuck one's later resume
  // still needs — and waits for — connection credit of its own.
  // Pins §4.2.1 Sending: "A sender MUST NOT hold one window's credit while
  // parked on the other".
  it('a stuck stream holds no connection credit', async () => {
    const stuck = new FlowSender()
    const healthy = new FlowSender()
    const conn = new FlowSender()
    stuck.assume(1)
    healthy.assume(8)
    conn.assume(2)

    expect(await acquireBoth(stuck, conn, latch(), noStall)).toBe('ok')
    let peerStall: boolean | undefined
    const park = watch(
      acquireBoth(stuck, conn, latch(), noStall, undefined, (peer) => {
        peerStall = peer
      }),
    )
    await tick()
    expect(peerStall, 'the stream window is the empty one').toBe(false)
    expect(conn.state().sent, 'no connection credit may be held while parked').toBe(1)

    // The healthy stream takes the last connection credit unhindered.
    expect(await acquireBoth(healthy, conn, latch(), noStall)).toBe('ok')

    // The stuck stream's own grant moves it to the connection park — it
    // must not have skipped the connection check by holding old credit.
    stuck.grant(1)
    await tick()
    expect(park.settled, 'connection window is empty; must still be parked').toBe(false)
    conn.grant(1)
    expect(await park.result).toBe('ok')
    expect(conn.state().sent).toBe(3)
  })

  // Pins §4.2.1 Sending (per-stream, applying to both): a park is "bounded
  // by the call's own ctx/deadline, by the call ending".
  it('gives up on done and on the signal, holding nothing', async () => {
    const stream = new FlowSender()
    const conn = new FlowSender()
    stream.assume(8)
    conn.assume(1)
    await acquireBoth(stream, conn, latch(), noStall)

    const done = new Latch()
    const p1 = watch(acquireBoth(stream, conn, done, 3_600_000))
    await tick()
    expect(p1.settled).toBe(false)
    done.trip()
    expect(await p1.result).toBe('ended')

    const ac = new AbortController()
    const p2 = watch(acquireBoth(stream, conn, latch(), 3_600_000, ac.signal))
    await tick()
    expect(p2.settled).toBe(false)
    ac.abort()
    expect(await p2.result).toBe('aborted')

    // Neither park left a credit behind on either window.
    expect(stream.state().sent).toBe(1)
    expect(conn.state().sent).toBe(1)
  })

  // Pins §4.2.1 Sending: "a send that never reaches the wire — the adapter
  // refused it (§4.4), or the call ended first — refunds both". Here the
  // call is over before the credits are taken, so nothing is spent at all.
  it('a dead call spends nothing on either window', async () => {
    const stream = new FlowSender()
    const conn = new FlowSender()
    stream.assume(8)
    conn.assume(8)
    const done = new Latch()
    done.trip()
    expect(await acquireBoth(stream, conn, done, 3_600_000)).toBe('ended')
    expect(stream.state().sent).toBe(0)
    expect(conn.state().sent, 'a dead call must not spend the shared window').toBe(0)
    const ac = new AbortController()
    ac.abort()
    expect(await acquireBoth(stream, conn, latch(), 3_600_000, ac.signal)).toBe('aborted')
    expect(conn.state().sent).toBe(0)
    // Without a connection window too.
    expect(await acquireBoth(stream, undefined, done, 3_600_000)).toBe('ended')
    expect(stream.state().sent).toBe(0)
  })

  // Pins §4.2.1 Sending / §14: a send short on both windows "is reported as
  // a connection stall — the peer's whole budget is what it waits on, not
  // one consumer"; short on the stream alone, it is a stream stall.
  it('short on both reports the connection window; short on the stream alone, the stream', async () => {
    const stream = new FlowSender()
    const conn = new FlowSender()
    stream.assume(1)
    conn.assume(1)
    await acquireBoth(stream, conn, latch(), noStall) // both spent

    const stalls: boolean[] = []
    const onStall = (peer: boolean) => void stalls.push(peer)
    const p1 = watch(acquireBoth(stream, conn, latch(), noStall, undefined, onStall))
    await tick()
    expect(stalls, 'short on both: the connection one').toEqual([true])
    expect(stream.state().sent, 'the park held nothing').toBe(1)
    stream.grant(1)
    await tick()
    expect(p1.settled, 'still short on the connection window').toBe(false)
    conn.grant(1)
    expect(await p1.result).toBe('ok')

    // The connection window has credit, the stream does not: the stream.
    conn.grant(1)
    const p2 = watch(acquireBoth(stream, conn, latch(), noStall, undefined, onStall))
    await tick()
    expect(stalls).toEqual([true, false])
    stream.grant(1)
    expect(await p2.result).toBe('ok')
    expect(conn.state().sent).toBe(3)
  })

  it('tryAcquireBoth is the synchronous fast path: both credits, or nothing held', () => {
    const stream = new FlowSender()
    const conn = new FlowSender()
    stream.assume(2)
    conn.assume(1)
    expect(tryAcquireBoth(stream, conn)).toBe(true)
    expect(tryAcquireBoth(stream, conn), 'connection short').toBe(false)
    expect(stream.state().sent, 'the stream credit was refunded').toBe(1)
    expect(tryAcquireBoth(stream, undefined), 'no connection window: the stream alone').toBe(true)
    expect(tryAcquireBoth(stream, undefined), 'stream short').toBe(false)
  })

  describe('T_stall (§10.1)', () => {
    beforeEach(() => {
      vi.useFakeTimers()
    })
    afterEach(() => {
      vi.useRealTimers()
    })

    // T_stall is one budget across both windows: a park that starts on the
    // stream window and moves to the connection window fails at T_stall
    // from the FIRST park, and the outcome names the window it was starved
    // on. Pins §4.2.1 Sending: "the same T_stall (§10.1), armed at the first
    // park, measures the whole wait across both windows, and on expiry the
    // call fails UNAVAILABLE naming the window it was parked on".
    it('is one budget across both windows, naming the window it expired on', async () => {
      const stall = 4000
      const stream = new FlowSender()
      const conn = new FlowSender()
      stream.assume(1)
      conn.assume(1)
      await acquireBoth(stream, conn, latch(), noStall) // spend both

      const park = watch(acquireBoth(stream, conn, latch(), stall))
      await vi.advanceTimersByTimeAsync(stall / 2)
      stream.grant(1) // half-way: the stream window opens, the connection is still shut
      await tick()
      expect(park.settled, 'still short on the connection window').toBe(false)
      await vi.advanceTimersByTimeAsync(stall / 2 - 1)
      expect(park.settled, 'the park must end exactly at T_stall from the first park').toBe(false)
      await vi.advanceTimersByTimeAsync(1)
      expect(await park.result).toBe('peer-stalled')
      // The refunded stream credit is still there for the next send.
      expect(stream.state().sent, 'refunded').toBe(1)

      // The other way round: a stream-only park names the stream.
      const s2 = new FlowSender()
      const c2 = new FlowSender()
      s2.assume(1)
      c2.assume(8)
      await acquireBoth(s2, c2, latch(), noStall)
      const p2 = acquireBoth(s2, c2, latch(), stall)
      await vi.advanceTimersByTimeAsync(stall)
      expect(await p2).toBe('stalled')
      expect(c2.state().sent, 'no connection credit is taken for a send that parked on its stream').toBe(1)
    })

    // Without a connection window (unreliable mode) acquireBoth is acquire.
    // Pins §4.2.1 Unreliable mode: "has no connection window" — the
    // per-stream acquire alone.
    it('an undefined peer is the per-stream acquire', async () => {
      const stream = new FlowSender()
      stream.assume(2)
      for (let i = 0; i < 2; i++) expect(await acquireBoth(stream, undefined, latch(), noStall), `send ${i}`).toBe('ok')
      const p: Promise<FlowAcquireBoth> = acquireBoth(stream, undefined, latch(), 1)
      await vi.advanceTimersByTimeAsync(1)
      expect(await p).toBe('stalled')
    })

    // The timer is disarmed with the park, whichever way it ended.
    it('a grant ends the park and disarms T_stall', async () => {
      const stream = new FlowSender()
      const conn = new FlowSender()
      stream.assume(1)
      conn.assume(1)
      await acquireBoth(stream, conn, latch(), noStall)
      const park = acquireBoth(stream, conn, latch(), 1000)
      stream.grant(1)
      conn.grant(1)
      expect(await park).toBe('ok')
      expect(vi.getTimerCount()).toBe(0)
    })
  })
})
