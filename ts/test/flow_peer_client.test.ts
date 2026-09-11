// The CLIENT half of the connection window (PROTOCOL.md §4.2.1, reliable
// mode only) — the TS twin of the Go flow_peer_client_test.go coverage,
// against a scripted server on the far end of the Conn's tx, so every rule can
// be observed without a TS server that grants on sid 0 yet:
//
//   - a sender assumes W_CONN per Conn and parks — on the connection window,
//     not the stream window — once it has spent it, bounded by T_stall;
//   - only a sid-0 WINDOW from the server incarnation the Conn is locked to
//     credits it; a foreign one is dropped in silence, never RESET;
//   - only a streaming call's creation-ack H settles the window: a unary T
//     never does, and an H with window 0 turns it off;
//   - a new server incarnation starts the sender over (§10.6);
//   - the raise rides right behind the first OPEN, once per incarnation;
//   - an overrun fails the offending call INTERNAL and nothing else;
//   - every data frame received returns exactly one credit on sid 0:
//     consumed, discarded with its call, or never buffered;
//   - a send that never reaches the wire refunds the window (§4.2.1, §4.4),
//     the call that ended after the credit was taken included — and a park
//     holds no connection credit;
//   - a sender short on both windows parks once, under one T_stall, and
//     names the connection window;
//   - a sid-0 grant the Conn holds no sender for credits nothing, locks
//     nothing and creates nothing;
//   - Conn.close unparks a sender waiting on connection credit;
//   - the stall is a PEER stall while parked, with the call's sid and method
//     (§14; the client half of the Go stats_test.go coverage).

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { Conn, type ClientStream, type ConnOptions } from '../src/conn'
import type { FrameHandler } from '../src/seam'
import { Counters, type ProtocolEvent, type ProtocolEventKind, type ProtocolStats } from '../src/stats'
import { Code, MessageTooLargeError, type StatusError } from '../src/status'
import { W_CONN } from '../src/util'
import { FlagClose, FlagWindow, frame, isData, isOpen, isReset, isTerminal, shapeOf, type Frame } from '../src/wire'
import { echo, tick, wireClone, type TestReq, type TestRes } from '../src/testing'

const enc = (v: unknown) => new TextEncoder().encode(JSON.stringify(v))

const SRV_A = 0xa11ce
const SRV_B = 0xb0b
const STALL_MS = 2000

// isPeerGrant recognizes a connection grant: WINDOW on sid 0, seq 0, no
// payload (§4.2.1, §7).
const isPeerGrant = (f: Frame): boolean => shapeOf(f) === FlagWindow && f.sid === 0 && f.seq === 0 && f.payload === undefined

function peerGrants(frames: readonly Frame[]): { n: number; total: number } {
  const gs = frames.filter(isPeerGrant)
  return { n: gs.length, total: gs.reduce((a, f) => a + f.window, 0) }
}

// afterFirstOpen returns the frame the client sent right behind its first
// OPEN, if any.
function afterFirstOpen(frames: readonly Frame[]): Frame | undefined {
  const i = frames.findIndex(isOpen)
  return i < 0 ? undefined : frames[i + 1]
}

class EventLog {
  readonly evs: ProtocolEvent[] = []
  readonly observe = (ev: ProtocolEvent): void => {
    this.evs.push(ev)
  }
  of(kind: ProtocolEventKind): ProtocolEvent[] {
    return this.evs.filter((e) => e.kind === kind)
  }
  first(kind: ProtocolEventKind): ProtocolEvent {
    const ev = this.evs.find((e) => e.kind === kind)
    if (ev === undefined) throw new Error(`no ${kind} event; saw ${JSON.stringify(this.evs.map((e) => e.kind))}`)
    return ev
  }
}

// PeerSrv is a scripted server on the far end of a Conn's tx. It records
// everything the client sends, answers every OPEN synchronously — a
// creation-ack H carrying its advertisement for a streaming call, a T for a
// unary one — and lets a test inject sequenced data frames or sid-0 grants
// under whichever server incarnation it currently is.
class PeerSrv implements FrameHandler {
  conn!: Conn
  epoch: number // the incarnation answering now; restart changes it
  clientEpoch = 0 // learned from the first OPEN
  muted = false // answer no OPEN: the test injects the ack itself
  refuseOver: number | undefined // payload bytes past which the adapter refuses (§4.4)
  readonly tx: Frame[] = []
  private readonly seq = new Map<number, number>() // last server seq per sid

  constructor(
    epoch: number,
    public window: number, // advertised on every H; a test may change it between calls
  ) {
    this.epoch = epoch
  }

  async handle(f: Frame): Promise<void> {
    if (this.refuseOver !== undefined && (f.payload?.length ?? 0) > this.refuseOver) throw new MessageTooLargeError()
    const g = wireClone(f)
    this.tx.push(g)
    const open = isOpen(g)
    if (open) this.clientEpoch = g.epoch
    if (!open || this.muted) return
    if (g.method === echo.once.path) {
      // A unary call ends in its T: no H, no window (§8).
      const t = this.frame(g.sid, FlagClose)
      t.code = Code.OK
      t.payload = enc({ text: 'ok' })
      await this.conn.handle(t, {})
      return
    }
    const h = this.frame(g.sid, 0)
    h.window = this.window
    await this.conn.handle(h, {})
  }

  // frame builds the next sequenced server frame on sid under the current
  // incarnation, echoing the client one as every server frame must (§6.1).
  frame(sid: number, flags: number): Frame {
    const seq = (this.seq.get(sid) ?? 0) + 1
    this.seq.set(sid, seq)
    return frame({ epoch: this.epoch, peerEpoch: this.clientEpoch, sid, seq, flags })
  }

  // data injects one data frame on sid.
  data(sid: number): Promise<void> {
    const f = this.frame(sid, 0)
    f.payload = enc({ text: 'd' })
    return this.conn.handle(f, {})
  }

  // grant injects a connection grant under the given server incarnation,
  // addressed at the given client one (§4.2.1).
  grant(epoch: number, peerEpoch: number, n: number): Promise<void> {
    return this.conn.handle(frame({ epoch, peerEpoch, flags: FlagWindow, window: n }), {})
  }

  restart(epoch: number): void {
    this.epoch = epoch
  }

  // lastOpen returns the sid of the most recent OPEN the client sent.
  lastOpen(): number {
    for (let i = this.tx.length - 1; i >= 0; i--) if (isOpen(this.tx[i]!)) return this.tx[i]!.sid
    return 0
  }
}

// clientFixture wires a Conn to a PeerSrv. srvWindow is the per-stream window
// the fake advertises on its H — large, so that the STREAM window never binds
// and every park below is the connection window's.
function clientFixture(srvEpoch: number, srvWindow: number, opts: ConnOptions = {}) {
  const srv = new PeerSrv(srvEpoch, srvWindow)
  const counters = new Counters()
  const log = new EventLog()
  const conn = new Conn(srv, { reliable: true, timing: { stallMs: STALL_MS }, protocolStats: [counters.observe, log.observe], ...opts })
  srv.conn = conn
  return { srv, conn, counters, log }
}

type CountStream = ClientStream<TestReq, TestRes>

async function sendN(stream: CountStream, n: number): Promise<void> {
  for (let i = 0; i < n; i++) await stream.send({ text: 'm' })
}

// park starts one more send and reports how it ends; `settled` says whether
// it has, so a test can assert that it is STILL parked.
function park(stream: CountStream) {
  const p = { settled: false, err: undefined as unknown, done: Promise.resolve() }
  p.done = stream
    .send({ text: 'm' })
    .catch((e: unknown) => {
      p.err = e
    })
    .finally(() => {
      p.settled = true
    })
  return p
}

beforeEach(() => {
  vi.useFakeTimers()
})
afterEach(() => {
  vi.useRealTimers()
})

// ---------------------------------------------------------------------------
// §4.2.1 sending: W_CONN is spent across the Conn, then the sender parks on
// the CONNECTION window — its stream window still has credit — and fails
// UNAVAILABLE at T_stall naming that window. One budget, not two.
// ---------------------------------------------------------------------------

describe('the sender assumes W_CONN per Conn (§4.2.1 Assumption)', () => {
  // Pins "Every sender assumes W_conn = 1024 messages (§10.1, Appendix B)
  // toward each peer incarnation from the moment it holds state for it".
  it('parks on the connection window after W_CONN messages and fails UNAVAILABLE exactly at T_stall, naming it', async () => {
    const { conn, counters } = clientFixture(SRV_A, 4096)
    const stream = conn.newStream(echo.count, {})
    await sendN(stream, W_CONN)
    expect(counters.snapshot().peerFlowStall, 'W_CONN messages go out unparked').toBe(0)

    const p = park(stream)
    await vi.advanceTimersByTimeAsync(STALL_MS - 1)
    expect(p.settled, 'the park must not end early').toBe(false)
    await vi.advanceTimersByTimeAsync(2)
    await p.done
    const err = p.err as StatusError
    expect(err.code).toBe(Code.UNAVAILABLE)
    expect(err.desc, 'the error must name the window that starved it').toContain('connection credit')
    expect(counters.snapshot()).toMatchObject({ peerFlowStall: 1, flowStall: 0 })
    conn.close()
  })
})

// ---------------------------------------------------------------------------
// §4.2.1 sending, one budget: a sender short on BOTH windows parks once, and
// the same T_stall measures the whole wait — not one per window. It names
// the connection window: the remedy differs ("raise maxPeerWindow or find
// the other slow consumer", not "this consumer stopped").
// ---------------------------------------------------------------------------

describe('one T_stall budget across both windows (§4.2.1 Sending, §10.1)', () => {
  // Pins "One park, one bound: the same T_stall (§10.1), armed at the first
  // park, measures the whole wait across both windows, and on expiry the
  // call fails UNAVAILABLE naming the window it was parked on".
  it('a sender short on both windows parks once and names the connection window', async () => {
    const { srv, conn, counters } = clientFixture(SRV_A, 4096)
    const a = conn.newStream(echo.count, {})
    await sendN(a, W_CONN - 1) // one connection credit left
    srv.window = 1 // the next call's stream window is one message
    const b = conn.newStream(echo.count, {})
    await tick()
    await b.send({ text: 'm' }) // the last credit of both windows

    const p = park(b)
    await tick()
    expect(counters.snapshot(), 'short on both: the connection window is the one named').toMatchObject({ peerFlowStall: 1, flowStall: 0 })
    await vi.advanceTimersByTimeAsync(STALL_MS - 1)
    expect(p.settled, 'one budget, not two: still parked short of T_stall').toBe(false)
    await vi.advanceTimersByTimeAsync(2)
    await p.done
    const err = p.err as StatusError
    expect(err.code).toBe(Code.UNAVAILABLE)
    expect(err.desc).toContain('connection credit')
    expect(counters.snapshot(), 'one stall, no resume').toMatchObject({ peerFlowStall: 1, flowStall: 0, peerFlowResume: 0, flowResume: 0 })
    conn.close()
  })
})

// ---------------------------------------------------------------------------
// §4.2.1 / §9.1 grants: a sid-0 WINDOW credits the connection window only
// when it comes from the server incarnation the Conn is locked to and names
// this client incarnation. Anything else is dropped in silence — no RESET,
// no credit.
// ---------------------------------------------------------------------------

describe('sid-0 grants (§4.2.1 Grants)', () => {
  // Pins: a receiver applies a sid-0 WINDOW only "when it holds a connection
  // sender for the incarnation the frame names — the client when the frame's
  // epoch is the server incarnation the Conn is locked to and its peer_epoch
  // the Conn's own epoch".
  it('only the locked server incarnation, naming this client, credits the window; the rest are dropped in silence', async () => {
    const { srv, conn, counters } = clientFixture(SRV_A, 4096)
    const stream = conn.newStream(echo.count, {})
    await sendN(stream, W_CONN)
    const p = park(stream)
    await tick()
    expect(counters.snapshot().peerFlowStall, 'the next send parks').toBe(1)
    const me = srv.clientEpoch

    // Another client incarnation's grant: not ours, not answered (§9.1).
    await srv.grant(SRV_A, (me + 1) >>> 0, 1)
    await tick()
    expect(p.settled, 'foreign peer_epoch: still parked').toBe(false)
    expect(srv.tx.filter(isReset), 'a foreign sid-0 grant draws no RESET').toHaveLength(0)

    // Another server incarnation's grant: not the one we count against.
    await srv.grant(SRV_B, me, 1)
    await tick()
    expect(p.settled, 'foreign server epoch: still parked').toBe(false)
    expect(srv.tx.filter(isReset)).toHaveLength(0)

    // The real one.
    await srv.grant(SRV_A, me, 1)
    await p.done
    expect(p.err).toBeUndefined()
    expect(counters.snapshot().peerFlowResume).toBe(1)
    conn.close()
  })

  // Pins: a sid-0 WINDOW the receiver holds no connection sender for is
  // dropped "never validated (§9.1), never answered with a RESET (§9.3),
  // never creating state" — on the Conn no lock and no credit (the
  // server_internal_test.go twin).
  it('before the Conn is locked to a server: no credit, no lock, silence', async () => {
    const { srv, conn } = clientFixture(SRV_A, 4096)
    // Names this incarnation, but the Conn is locked to no server yet: there
    // is no sender counted against that epoch to credit.
    await srv.grant(SRV_A, conn.epoch, 100)
    await tick()
    expect(conn.connTx.state(), 'a grant from an unlocked epoch credits nothing').toMatchObject({ on: true, granted: W_CONN, sent: 0 })
    expect((conn as unknown as { srvEpochSet: boolean }).srvEpochSet, 'only an accepted call frame locks the Conn').toBe(false)
    expect(srv.tx, 'answered with silence').toHaveLength(0)
    conn.close()
  })

  // Pins "A receiver applies it only in reliable mode".
  it('is ignored in unreliable mode, and this side never grants on sid 0 there either', async () => {
    const srv = new PeerSrv(SRV_A, 0)
    const counters = new Counters()
    const conn = new Conn(srv, { reliable: false, timing: { callMs: 300, livenessMs: 450, retransmitMs: 50, tombstoneMs: 1000, holdMs: 50 }, protocolStats: counters.observe })
    srv.conn = conn
    const stream = conn.newStream(echo.count, {})
    await tick()
    await srv.grant(SRV_A, srv.clientEpoch, 1)
    // Well past what a (wrongly) adopted one-message window would allow.
    await sendN(stream, 40)
    expect(counters.snapshot().peerFlowStall).toBe(0)
    expect(srv.tx.filter(isReset), 'dropped in silence').toHaveLength(0)
    expect(srv.tx.filter(isPeerGrant), 'this side never grants on sid 0 either').toHaveLength(0)
    stream.closeSend()
    conn.close()
  })
})

// ---------------------------------------------------------------------------
// §4.2.1 settle: only a streaming call's creation-ack H settles the
// connection window. A unary T carries no window and must not switch it off
// while the server enforces; an H that advertises 0 does switch it off.
// ---------------------------------------------------------------------------

describe('settle (§4.2.1 Settle)', () => {
  // Pins "A unary T and a SendHeader-flushed H carry no window (§7, §8) and
  // MUST NOT settle; a unary-first Conn stays assumed until its first
  // streaming ack".
  it('a unary T never settles: the window is still on afterwards', async () => {
    const { conn, counters } = clientFixture(SRV_A, 4096)
    // The Conn's first server frame is a unary T (window 0).
    expect(await conn.invoke(echo.once, { text: 'x' })).toEqual({ text: 'ok' })

    const stream = conn.newStream(echo.count, {})
    await sendN(stream, W_CONN)
    const p = park(stream)
    await tick()
    expect(counters.snapshot().peerFlowStall, 'the window is still on: the T settled nothing').toBe(1)
    conn.close()
    await p.done
  })

  // Pins "window = 0 turns the connection window off toward that peer, as it
  // turns the stream's off".
  it('a streaming ack with window 0 turns it off: the peer does no flow control', async () => {
    const { conn, counters } = clientFixture(SRV_A, 0)
    const stream = conn.newStream(echo.count, {})
    await sendN(stream, 2 * W_CONN)
    expect(counters.snapshot()).toMatchObject({ peerFlowStall: 0, flowStall: 0 })
    conn.close()
  })

  // Pins "A grant never enables flow control by itself" and "A grant toward
  // a window that is off is dropped the same way".
  it('a grant never enables a window the peer turned off', async () => {
    const { srv, conn, counters } = clientFixture(SRV_A, 0)
    const stream = conn.newStream(echo.count, {})
    await sendN(stream, 2 * W_CONN)
    await srv.grant(SRV_A, srv.clientEpoch, 1) // dropped: never enables
    await sendN(stream, 2 * W_CONN)
    expect(counters.snapshot(), 'still no window binds').toMatchObject({ peerFlowStall: 0, flowStall: 0 })
    expect(conn.connTx.state().on).toBe(false)
    conn.close()
  })
})

// ---------------------------------------------------------------------------
// §4.2.1 / §10.6 restart: a new server incarnation counts from zero, so the
// Conn starts its sender over when a call first accepts a frame from it — a
// cumulative count would park the honest new server's client forever. Grants
// of the dead incarnation are dropped from then on.
// ---------------------------------------------------------------------------

describe('restart on a surviving channel (§4.2.1 Restart)', () => {
  // Pins "When it first hears a different server epoch ... the Conn MUST
  // start its sender over — assumed at W_conn, unsettled, nothing sent — ...
  // [and] drop grants naming the old epoch".
  it('a new server incarnation starts the sender over, and the dead one can no longer credit it', async () => {
    const { srv, conn, counters } = clientFixture(SRV_A, 4096)
    const old = conn.newStream(echo.count, {})
    await sendN(old, 1000)

    srv.restart(SRV_B)
    const fresh = conn.newStream(echo.count, {}) // its H comes from the new incarnation
    await sendN(fresh, W_CONN)
    expect(counters.snapshot().peerFlowStall, 'a fresh W_CONN against the new server').toBe(0)

    const p = park(fresh)
    await tick()
    expect(counters.snapshot().peerFlowStall).toBe(1)

    await srv.grant(SRV_A, srv.clientEpoch, 1) // the dead incarnation
    await tick()
    expect(p.settled, 'a grant from the old incarnation must be dropped').toBe(false)
    await srv.grant(SRV_B, srv.clientEpoch, 1)
    await p.done
    expect(p.err).toBeUndefined()
    conn.close()
  })
})

// ---------------------------------------------------------------------------
// §4.2.1 raise: a receiver whose maxPeerWindow exceeds W_CONN lifts the
// server's assumption once, with a sid-0 grant of the difference right behind
// its first OPEN — the eager one or the piggybacked one — and again for a new
// server incarnation. At the floor there is nothing to raise by.
// ---------------------------------------------------------------------------

describe('the raise (§4.2.1 Raise)', () => {
  const limits = { limits: { maxPeerWindow: 2048 } }

  // Pins "the client right behind its first OPEN ... and, when it hears a new
  // server incarnation, at once"; "A receiver at the floor sends none".
  it('rides right behind the first eager OPEN, once per Conn', async () => {
    const { srv, conn } = clientFixture(SRV_A, 4096, limits)
    conn.newStream(echo.count, {})
    await tick()
    conn.newStream(echo.count, {})
    await tick()

    const raise = afterFirstOpen(srv.tx)
    expect(raise !== undefined && isPeerGrant(raise), 'the raise rides right behind the first OPEN').toBe(true)
    expect(raise!.window).toBe(2048 - W_CONN)
    expect(raise!.epoch).toBe(srv.clientEpoch)
    expect(srv.tx.filter(isPeerGrant), 'once per Conn').toHaveLength(1)
    conn.close()
  })

  it('rides right behind the first piggybacked OPEN too', async () => {
    const { srv, conn } = clientFixture(SRV_A, 4096, limits)
    await conn.invoke(echo.once, { text: 'x' })
    await tick()
    const raise = afterFirstOpen(srv.tx)
    expect(raise !== undefined && isPeerGrant(raise)).toBe(true)
    expect(raise!.window).toBe(2048 - W_CONN)
    conn.close()
  })

  it('at the floor there is nothing to raise by', async () => {
    const { srv, conn } = clientFixture(SRV_A, 4096, { limits: { maxPeerWindow: 100 } })
    conn.newStream(echo.count, {})
    await tick()
    expect(srv.tx.filter(isPeerGrant), 'maxPeerWindow is floored at W_CONN').toHaveLength(0)
    conn.close()
  })

  it('is due again for a new server incarnation', async () => {
    const { srv, conn } = clientFixture(SRV_A, 4096, limits)
    conn.newStream(echo.count, {})
    await tick()
    srv.restart(SRV_B)
    conn.newStream(echo.count, {})
    await tick()
    const { n, total } = peerGrants(srv.tx)
    expect(n, "the new incarnation's sender starts at W_CONN too").toBe(2)
    expect(total).toBe(2 * (2048 - W_CONN))
    conn.close()
  })

  // Pins "the client right behind its first OPEN": the raise belongs to the
  // OPEN, not to the call's life — the server's container for this
  // incarnation exists from that OPEN on, cancelled call or not, and a
  // second call is owed none.
  it("survives the first call's cancellation: it rode the OPEN, once", async () => {
    const { srv, conn } = clientFixture(SRV_A, 4096, limits)
    const ac = new AbortController()
    conn.newStream(echo.count, { signal: ac.signal })
    ac.abort() // before the OPEN's transmit has even resolved
    await tick()
    expect(srv.tx.filter(isOpen)).toHaveLength(1)
    expect(peerGrants(srv.tx), 'the raise went out with the OPEN').toEqual({ n: 1, total: 2048 - W_CONN })

    conn.newStream(echo.count, {})
    await tick()
    expect(srv.tx.filter(isPeerGrant), 'once per Conn: the second call draws none').toHaveLength(1)
    conn.close()
  })
})

// ---------------------------------------------------------------------------
// §4.2 / §4.2.1 / §15 receiving: the server may not have more than
// maxPeerWindow buffered here across all its calls. The frame that would
// exceed it is never buffered and fails ITS call INTERNAL — the other call is
// untouched, and the failed call's frames come back as credit.
// ---------------------------------------------------------------------------

describe("the receiver bound (§4.2.1 Overrun, The receiver's ledger)", () => {
  // Pins "the receiver fails the call it is addressed to with INTERNAL ...
  // and returns the frame's credit as never buffered. Never the peer".
  it('an overrun fails only the offending call, and its discarded frames return their credit at once', async () => {
    // Per-stream windows of W_CONN, so only the connection window can trip.
    const { srv, conn } = clientFixture(SRV_A, 4096, { rxBuffer: { size: W_CONN } })
    const a = conn.newStream(echo.many, {})
    await a.send({ text: 'x' })
    const sidA = srv.lastOpen()
    const b = conn.newStream(echo.many, {})
    await b.send({ text: 'x' })
    const sidB = srv.lastOpen()

    // Nothing is read: exactly the window, split across the two calls.
    for (let i = 0; i < W_CONN / 2; i++) {
      await srv.data(sidA)
      await srv.data(sidB)
    }
    const isTerminalFor = (sid: number) => (f: Frame) => isTerminal(f) && f.sid === sid
    expect(srv.tx.filter(isTerminal), 'the window fits').toHaveLength(0)

    // One past it, on b: b fails, a does not.
    await srv.data(sidB)
    expect(srv.tx.filter(isTerminalFor(sidB)), 'the offending call aborts').toHaveLength(1)
    expect(srv.tx.filter(isTerminalFor(sidA)), 'the other call is untouched').toHaveLength(0)
    expect(srv.tx.filter(isReset)).toHaveLength(0)

    // b's buffered frames are still delivered, then the overrun surfaces.
    let got = 0
    for (;;) {
      try {
        expect(await b.recv()).toBeDefined()
        got++
      } catch (e) {
        const err = e as StatusError
        expect(err.code).toBe(Code.INTERNAL)
        expect(err.desc, 'the error must name the connection window').toContain('connection flow-control window')
        break
      }
    }
    expect(got).toBe(W_CONN / 2)

    // Discarding b's frames returned their credit at once: half the window,
    // in one grant — and draining them afterwards returned nothing twice.
    expect(peerGrants(srv.tx)).toEqual({ n: 1, total: W_CONN / 2 })

    // a is live: with b's frames gone there is room for it again.
    await srv.data(sidA)
    expect(srv.tx.filter(isTerminalFor(sidA))).toHaveLength(0)
    expect(await a.recv()).toEqual({ text: 'd' })
    conn.close()
  })

  // Pins §4.2.1 Cadence: "grant once the credit it holds back reaches half
  // its window (one small frame per MaxPeerWindow/2 messages)".
  it('consumed frames return their credit on sid 0, batched at half the window', async () => {
    const { srv, conn } = clientFixture(SRV_A, 4096, { rxBuffer: { size: W_CONN } })
    const stream = conn.newStream(echo.many, {})
    await stream.send({ text: 'x' })
    const sid = srv.lastOpen()
    const injected = 600
    for (let i = 0; i < injected; i++) await srv.data(sid)
    expect(srv.tx.filter(isPeerGrant), 'nothing consumed, nothing granted').toHaveLength(0)

    for (let i = 0; i < injected; i++) expect(await stream.recv()).toEqual({ text: 'd' })
    expect(peerGrants(srv.tx), 'batched at half the window: 600 consumed is one grant').toEqual({ n: 1, total: W_CONN / 2 })
    // The per-stream grant is still there, on the call's own sid (§4.2.1).
    expect(srv.tx.filter((f) => shapeOf(f) === FlagWindow && f.sid === sid)).toHaveLength(1)
    conn.close()
  })

  // Pins "one credit back when it was never buffered at all — dropped as
  // off-shape (§8) ... or addressed to a call the receiver no longer has
  // (RESET-drawn, §9.3)".
  it('a data frame for a call this side no longer has draws a RESET and still returns its credit', async () => {
    const { srv, conn } = clientFixture(SRV_A, 4096)
    // A finished call, so the Conn is locked to the server incarnation.
    await conn.invoke(echo.once, { text: 'x' })
    const sid = srv.lastOpen()

    // Data for the finished call: unknown at a client (no tombstones in
    // reliable mode), each draws a RESET (§9.3, §10.6)...
    for (let i = 0; i < W_CONN / 2; i++) await srv.data(sid)
    expect(srv.tx.filter(isReset)).toHaveLength(W_CONN / 2)
    // ...and the credit they spent comes back.
    expect(peerGrants(srv.tx)).toEqual({ n: 1, total: W_CONN / 2 })
    conn.close()
  })

  it('an off-shape data frame is dropped and still returns its credit', async () => {
    const { srv, conn } = clientFixture(SRV_A, 4096)
    const stream = conn.newStream(echo.count, {}) // client-streaming: no server data frames
    await tick()
    const sid = srv.lastOpen()
    for (let i = 0; i < W_CONN / 2; i++) await srv.data(sid)
    expect(srv.tx.filter(isTerminal), 'off-shape frames are dropped, the call lives').toHaveLength(0)
    expect(peerGrants(srv.tx)).toEqual({ n: 1, total: W_CONN / 2 })
    stream.closeSend()
    conn.close()
  })
})

// ---------------------------------------------------------------------------
// §4.2.1 / §4.4: a send that never reaches the wire refunds BOTH windows. The
// connection window is shared by every call to the peer and cumulative for
// the incarnation's life, so a credit spent on a frame that never went out
// would be a permanent shrink — observable from another call, which parks
// one message early.
// ---------------------------------------------------------------------------

describe('a send that never reaches the wire refunds the connection window (§4.2.1 Sending)', () => {
  // exactlyOneLeft asserts the Conn has precisely one connection credit
  // left: one message from a fresh call goes out unparked, the next parks.
  async function exactlyOneLeft(conn: Conn, counters: Counters): Promise<void> {
    const before = counters.snapshot().peerFlowStall
    const b = conn.newStream(echo.count, {})
    await b.send({ text: 'm' })
    expect(counters.snapshot().peerFlowStall, 'the refunded credit is there for another call').toBe(before)
    const p = park(b)
    await tick()
    expect(counters.snapshot().peerFlowStall, 'and it was exactly one').toBe(before + 1)
    conn.close()
    await p.done
  }

  it('the adapter refused it (§4.4)', async () => {
    const { srv, conn, counters } = clientFixture(SRV_A, 4096)
    srv.refuseOver = 64
    const a = conn.newStream(echo.count, {})
    await sendN(a, W_CONN - 1)
    const err = (await a.send({ text: 'x'.repeat(200) }).catch((e: unknown) => e)) as StatusError
    expect(err.code).toBe(Code.RESOURCE_EXHAUSTED)
    await exactlyOneLeft(conn, counters)
  })

  it('the send cap refused it (§16)', async () => {
    const { conn, counters } = clientFixture(SRV_A, 4096, { maxSendMsgSize: 64 })
    const a = conn.newStream(echo.count, {})
    await sendN(a, W_CONN - 1)
    const err = (await a.send({ text: 'x'.repeat(200) }).catch((e: unknown) => e)) as StatusError
    expect(err.code).toBe(Code.RESOURCE_EXHAUSTED)
    await exactlyOneLeft(conn, counters)
  })

  it('the call was cancelled under the park: a dead call spends nothing', async () => {
    const { srv, conn, counters } = clientFixture(SRV_A, 4096)
    const a = conn.newStream(echo.count, {})
    await sendN(a, W_CONN)
    const p = park(a)
    await tick()
    expect(counters.snapshot().peerFlowStall).toBe(1)
    a.cancel()
    await p.done
    expect(p.err).toBeInstanceOf(Error) // EndOfStream: the call ended under it
    expect(counters.snapshot().peerFlowResume, 'not a resume').toBe(0)
    // One credit granted is one credit usable by another call.
    await srv.grant(SRV_A, srv.clientEpoch, 1)
    await exactlyOneLeft(conn, counters)
  })

  // Pins "a send that never reaches the wire — ... or the call ended first,
  // between taking the credit and transmitting — refunds both", and "A
  // sender MUST NOT hold one window's credit while parked on the other". The
  // call ends in the one window the sender's own checks cannot cover: after
  // acquireBoth handed back the credit and before the frame is built (the
  // server_internal_test.go twin).
  it('the call ended after the credit was taken: refunded, nothing on the wire', async () => {
    // A stream window of one: the second send parks on the STREAM window.
    const srv = new PeerSrv(SRV_A, 1)
    let s!: CountStream
    const endUnderTheSend: ProtocolStats = (ev) => {
      // The grant woke the parked send and acquireBoth handed it both
      // credits; the call ends now, before the frame is built — a terminal,
      // an abort or a cancel racing the grant.
      if (ev.kind === 'flow-resume') s.cancel()
    }
    const conn = new Conn(srv, { reliable: true, timing: { stallMs: STALL_MS }, protocolStats: endUnderTheSend })
    srv.conn = conn
    s = conn.newStream(echo.count, {})
    await tick() // the creation ack, window 1
    await s.send({ text: 'a' }) // spends the stream's one credit and one of the Conn's
    const p = park(s)
    await tick() // parked on the stream window
    expect(p.settled).toBe(false)
    expect(conn.connTx.state().sent, 'a park holds no connection credit').toBe(1)

    await conn.handle(frame({ epoch: SRV_A, peerEpoch: conn.epoch, sid: s.sid, flags: FlagWindow, window: 1 }), {})
    await p.done
    expect((p.err as Error).name, 'send on the ended call').toBe('EndOfStreamError')
    expect(srv.tx.filter(isData), 'the second never went out').toHaveLength(1)
    expect(conn.connTx.state().sent, 'the credit of a frame that never went out is refunded').toBe(1)
    conn.close()
  })
})

// ---------------------------------------------------------------------------
// §4.2.1 / §4.5: a sender parked on connection credit has no call left to
// wake it through when the Conn closes — close releases the window itself.
// ---------------------------------------------------------------------------

describe('close (§4.2.1 Sending, §4.5)', () => {
  // Pins "Conn.Close and DisconnectPeer (§4.5) release a parked connection
  // sender, as a call's end releases a parked stream sender".
  it('unparks a sender waiting on connection credit', async () => {
    const { conn, counters } = clientFixture(SRV_A, 4096)
    const stream = conn.newStream(echo.count, {})
    await sendN(stream, W_CONN)
    const p = park(stream)
    await tick()
    expect(counters.snapshot().peerFlowStall).toBe(1)

    conn.close()
    await p.done
    const err = p.err as Error & { code?: Code }
    expect(err.name === 'EndOfStreamError' || err.code === Code.UNAVAILABLE, String(err)).toBe(true)
    expect(counters.snapshot().peerFlowResume, 'no credit ever came').toBe(0)
  })
})

// ---------------------------------------------------------------------------
// §4.2.1 restart / raise: the Conn locks to a server incarnation on the first
// sequenced frame it hears from it — on a live call or not. The server's one
// raise rides right behind its first H; when that H lands on a call the
// client already released it draws a RESET, and a Conn that had not locked
// from it would drop the raise as a stranger's and stay at W_CONN against a
// larger window for its whole life.
// ---------------------------------------------------------------------------

describe('the Conn locks to the server from a frame for a released call (§4.2.1 Restart)', () => {
  // Above 2 × W_CONN: with the raise lost, no cadence of a 4096 receiver
  // reaches a sender stuck at 1024 (pending ≥ 2048, or outstanding + pending
  // ≥ 4096) — a forever-park, not a slow one.
  const window = 4 * W_CONN
  const raise = window - W_CONN

  // lifted sends maxPeerWindow messages unparked, then parks on the next:
  // the raise landed, and it was exactly the raise.
  async function lifted(conn: Conn, counters: Counters): Promise<void> {
    const s = conn.newStream(echo.count, {})
    await sendN(s, window)
    expect(counters.snapshot().peerFlowStall, 'the raise landed').toBe(0)
    const p = park(s)
    await tick()
    expect(counters.snapshot().peerFlowStall, 'exactly the lifted window').toBe(1)
    conn.close() // releases the parked send
    await p.done
  }

  // released opens a client-streaming call and cancels it before the fake
  // has answered: the OPEN is on the wire, the call is gone.
  async function released(srv: PeerSrv, conn: Conn): Promise<number> {
    srv.muted = true
    const ac = new AbortController()
    conn.newStream(echo.count, { signal: ac.signal })
    await tick()
    ac.abort()
    await tick()
    return srv.lastOpen()
  }

  // Pins: the Conn "locks ... on the first sequenced frame it hears from it,
  // for a live call or for one it has already released".
  it("the Conn's first streaming call", async () => {
    const { srv, conn, counters } = clientFixture(SRV_A, window)
    // Released before its ack: the H then lands on no call — a RESET — and
    // the raise rides right behind it.
    const sid = await released(srv, conn)
    const h = srv.frame(sid, 0)
    h.window = window
    await conn.handle(h, {})
    expect(srv.tx.filter(isReset), 'the H found no call').toHaveLength(1)
    await srv.grant(SRV_A, srv.clientEpoch, raise)
    srv.muted = false

    await lifted(conn, counters)
  })

  it('the first streaming call to a new incarnation', async () => {
    const { srv, conn, counters } = clientFixture(SRV_A, window)
    // Locked to A by a call it answered, lifted by A, some sent.
    const warm = conn.newStream(echo.count, {})
    await tick()
    await srv.grant(SRV_A, srv.clientEpoch, raise)
    await sendN(warm, 10)

    // The server restarts; the Conn's first call to B is released before
    // B's ack, which lands on no call. The Conn re-locks to B from it all
    // the same — sender started over — and B's raise right behind it lands.
    srv.restart(SRV_B)
    const sid = await released(srv, conn)
    const h = srv.frame(sid, 0)
    h.window = window
    await conn.handle(h, {})
    await srv.grant(SRV_B, srv.clientEpoch, raise)
    srv.muted = false

    await lifted(conn, counters)
  })
})

// ---------------------------------------------------------------------------
// §4.2.1 / §14: a sender out of CONNECTION credit is a peer-flow stall, never
// a flow stall — the remedies differ — and it is observable WHILE parked,
// with the parked call's sid and method. The resume is its own event, so a
// stuck sender is distinguishable from one that recovered. (The client half
// of the Go stats_test.go coverage; the TS-server half is the next stage's.)
// ---------------------------------------------------------------------------

describe('peer-flow-stall / peer-flow-resume (§14)', () => {
  it('names the parked call, and the resume names the same window', async () => {
    const { srv, conn, counters, log } = clientFixture(SRV_A, 4096)
    const first = conn.newStream(echo.count, {})
    await sendN(first, W_CONN)
    expect(counters.snapshot().flowStall, 'no stream window was ever short').toBe(0)

    // One more call, one more message: its stream window is untouched, the
    // connection window is empty.
    const extra = conn.newStream(echo.count, {})
    const p = park(extra)
    await tick()
    expect(counters.snapshot()).toMatchObject({ peerFlowStall: 1, flowStall: 0, peerFlowResume: 0 })
    expect(log.of('flow-stall')).toEqual([])
    expect(log.first('peer-flow-stall')).toMatchObject({ sid: extra.sid, method: echo.count.path, count: 0 })
    expect(log.first('peer-flow-stall').peer).toBeUndefined()

    await srv.grant(SRV_A, srv.clientEpoch, 1)
    await p.done
    expect(p.err).toBeUndefined()
    expect(counters.snapshot()).toMatchObject({ peerFlowStall: 1, peerFlowResume: 1, flowStall: 0, flowResume: 0 })
    expect(log.first('peer-flow-resume')).toMatchObject({ sid: extra.sid, method: echo.count.path })
    conn.close()
  })
})
