// The SERVER half of the connection window (PROTOCOL.md §4.2.1, reliable mode
// only) — the TS twin of the Go flow_peer_server_test.go coverage, against a
// scripted client driving Server.handle directly, and then the whole TS↔TS
// path end to end:
//
//   - the server paces itself, per client incarnation, by the connection
//     window that incarnation's OPEN advertised, and parks — on the
//     connection window, not the stream window — bounded by T_stall;
//   - a sid-0 WINDOW credits only an existing (peer, client-epoch) container
//     and only in reliable mode; anything else is dropped in silence and
//     creates no state;
//   - every H and T the server sends carries its own connection window,
//     maxPeerWindow, and no other server frame does;
//   - the container's sender is created from the OPEN that creates the
//     container — admitted or rejected — absent meaning off, a later OPEN
//     ignored;
//   - an overrun fails the offending call INTERNAL and nothing else;
//   - every reliable-mode data frame received returns exactly one credit on
//     sid 0 — consumed, discarded with its call, or never buffered — except
//     the server-streaming request that rode the OPEN uncredited;
//   - the starvation clause: stuck consumers pinning most of the window do
//     not starve a healthy stream (grants fire below half the window);
//   - a handler's Send after the client's abort spends nothing;
//   - a handler short on both windows parks once, under one T_stall, and
//     names the connection window;
//   - disconnectPeer releases a handler parked on connection credit and
//     reclaims the containers and the ledger with the slot (§4.5, §9.4);
//   - credit is granted to the incarnation that spent it (§4.2.1 Cadence);
//   - an evicted container continues its sender from the ledger (§9.4);
//   - end to end: a park across streams keeps the channel live, and three
//     streams past W_CONN both ways complete on sid-0 grants.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { unaryMethod } from '../src/desc'
import type { StreamServerInterceptor, UnaryServerInterceptor } from '../src/interceptor'
import { Server, type ServerContext, type ServerOptions } from '../src/server'
import { abortCause, Code, type StatusError } from '../src/status'
import { Counters, type ProtocolEvent, type ProtocolEventKind, type ProtocolStats } from '../src/stats'
import type { Timing } from '../src/timing'
import { noop, W_CONN, W_INIT, type SenderState } from '../src/util'
import { FlagClose, FlagOpen, FlagWindow, frame, isData, isHeaderFrame, isOpen, isReset, isTerminal, shapeOf, type Frame } from '../src/wire'
import { echo, jsonCodec, makeLoopNet, makeNet, registerEcho, tick, wireClone, type TestReq, type TestRes } from '../src/testing'

const enc = (v: unknown) => new TextEncoder().encode(JSON.stringify(v))

const PEER = 'peer-1'
const EPOCH_A = 0xc1a
const EPOCH_B = 0xc1b
const STALL_MS = 2000
const fast: Timing = { callMs: 300, livenessMs: 450, retransmitMs: 50, tombstoneMs: 1000, holdMs: 50 }

// isPeerGrant recognizes a connection grant: WINDOW on sid 0, seq 0, no
// payload (§4.2.1, §7).
const isPeerGrant = (f: Frame): boolean => shapeOf(f) === FlagWindow && f.sid === 0 && f.seq === 0 && f.payload === undefined

function peerGrants(frames: readonly Frame[]): { n: number; total: number } {
  const gs = frames.filter(isPeerGrant)
  return { n: gs.length, total: gs.reduce((a, f) => a + f.window, 0) }
}

const terminalOn =
  (sid: number) =>
  (f: Frame): boolean =>
    isTerminal(f) && f.sid === sid

// Every reliable-mode OPEN below advertises the client's per-stream window
// and, beside it, its connection window (§8, §4.2.1) — W_CONN unless a test
// says otherwise, so the server's sender toward the incarnation is created
// at the value the old assumption had; 0 leaves the field absent.

// streamOpen builds the eager, bare OPEN of a client-streaming or bidi call.
const streamOpen = (epoch: number, sid: number, window: number, method: string, connWindow = W_CONN): Frame =>
  frame({ epoch, sid, seq: 1, flags: FlagOpen, method, window, connWindow })

// manyOpen builds a server-streaming OPEN|CLOSE asking for n responses.
const manyOpen = (epoch: number, sid: number, window: number, n: number, connWindow = W_CONN): Frame =>
  frame({ epoch, sid, seq: 1, flags: FlagOpen | FlagClose, method: echo.many.path, window, connWindow, payload: enc({ text: 'm', n }) })

// onceOpen builds a unary OPEN|CLOSE (§8).
const onceOpen = (epoch: number, sid: number, connWindow = W_CONN): Frame =>
  frame({ epoch, sid, seq: 1, flags: FlagOpen | FlagClose, method: echo.once.path, window: 32, connWindow, payload: enc({ text: 'x' }) })

// data builds a client data frame (flags 0, payload present).
const data = (epoch: number, sid: number, seq: number): Frame => frame({ epoch, sid, seq, payload: enc({ text: 'd' }) })

// abortFrame builds a client abort: a terminal on the call (§10.3).
function abortFrame(epoch: number, sid: number, seq: number): Frame {
  const f = frame({ epoch, sid, seq, flags: FlagClose })
  f.code = Code.CANCELLED
  return f
}

// grant builds a connection grant from client incarnation epoch (§4.2.1).
const grant = (epoch: number, n: number): Frame => frame({ epoch, flags: FlagWindow, window: n })

// aborted resolves once the signal is.
const aborted = (signal: AbortSignal): Promise<void> =>
  new Promise((res) => {
    if (signal.aborted) res()
    else signal.addEventListener('abort', () => res(), { once: true })
  })

// blockStreams is a stream interceptor that never reads or writes — a
// consumer that stopped — for every call. It ends with the call's own cause,
// as a Go handler returning on <-ctx.Done() does.
const blockStreams: StreamServerInterceptor = async (_stream, ctx) => {
  await aborted(ctx.signal)
  throw abortCause(ctx.signal)
}

// blockUnlessMarked is blockStreams unless the call carries the given
// metadata key. Handlers start in no particular order, so a count would not
// say which call is the healthy one; the client marks it.
const blockUnlessMarked =
  (key: string): StreamServerInterceptor =>
  (stream, ctx, next) =>
    (ctx.metadata?.[key]?.length ?? 0) > 0 ? next(stream, ctx) : blockStreams(stream, ctx, next)

const isMarked = (ctx: ServerContext, key: string): boolean => (ctx.metadata?.[key]?.length ?? 0) > 0

class EventLog {
  readonly evs: ProtocolEvent[] = []
  readonly observe: ProtocolStats = (ev) => {
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

// srvFixture is a reliable Server whose tx frames are recorded without bound,
// so a scripted client can inject any number of frames and read back exactly
// what the server emitted. Every frame arrives from PEER unless said
// otherwise.
function srvFixture(opts: ServerOptions = {}) {
  const tx: Frame[] = []
  const counters = new Counters()
  const log = new EventLog()
  const server = new Server(
    { handle: (f: Frame) => void tx.push(wireClone(f)) },
    { reliable: true, timing: { stallMs: STALL_MS }, protocolStats: [counters.observe, log.observe], ...opts },
  )
  const counts = registerEcho(server)
  const handle = (f: Frame, peer: unknown = PEER): Promise<void> => server.handle(f, { peer })
  return { server, tx, counters, log, counts, handle }
}

// settle lets every promise chain run out: a macrotask turn drains the whole
// microtask queue, so afterwards a handler has either finished or parked on
// something only a frame or a timer can wake (the synctest.Wait of these
// tests). setImmediate is the one timer left real for it, see beforeEach.
async function settle(): Promise<void> {
  for (let i = 0; i < 2; i++) await new Promise<void>((res) => setImmediate(res))
}

// prime returns credit for n frames of a call this server never had: each
// draws a RESET (§9.3, §10.6) and is never buffered.
async function prime(sf: ReturnType<typeof srvFixture>, n: number): Promise<void> {
  for (let i = 0; i < n; i++) await sf.handle(data(EPOCH_A, 99, 2 + i))
}

// The server's private state, read the way server_internal_test.go reads
// srv.peers / srv.peerFlow: one container per client incarnation on the
// peer's slot, and the slot's ledger.
interface Container {
  connTx: { state(): SenderState }
}
interface Slot {
  epochs: Map<number, Container>
  flow: { unstash(epoch: number): unknown } | undefined
}
const slotsOf = (server: Server): Map<unknown, Slot> => (server as unknown as { slots: Map<unknown, Slot> }).slots

beforeEach(() => {
  // T_stall and the clock are faked; setImmediate stays real so that settle
  // has a macrotask boundary to drain the microtask queue on.
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval', 'Date'] })
})
afterEach(() => {
  vi.useRealTimers()
})

// ---------------------------------------------------------------------------
// §4.2.1 sending: W_CONN is spent across every call to one client
// incarnation, then the handler parks on the CONNECTION window — its stream
// window still has credit — resumes on a sid-0 grant, and fails UNAVAILABLE
// at T_stall naming that window.
// ---------------------------------------------------------------------------

describe('the server paces itself by the advertised window per client incarnation (§4.2.1 Scope, Sending)', () => {
  // Pins "the sender's credit is per peer incarnation: on the server one
  // window per (peer, client-epoch) container" — the window the OPENs
  // advertise, W_CONN here.
  it('parks on the connection window across calls, then resumes on a sid-0 grant', async () => {
    const sf = srvFixture()
    // Two calls in turn: the connection window is per incarnation, not per
    // call, so the second one parks after the first one's 300 plus 724 of
    // its own.
    await sf.handle(manyOpen(EPOCH_A, 1, 4096, 300))
    await settle()
    expect(sf.tx.filter(terminalOn(1))).toHaveLength(1)
    await sf.handle(manyOpen(EPOCH_A, 2, 4096, 1100))
    await settle()
    expect(sf.tx.filter(isData), 'exactly W_CONN data frames reach the wire').toHaveLength(W_CONN)
    expect(sf.tx.filter(terminalOn(2)), 'the second call is parked, not over').toHaveLength(0)
    expect(sf.counters.snapshot()).toMatchObject({ peerFlowStall: 1, flowStall: 0 })

    await sf.handle(grant(EPOCH_A, 400))
    await settle()
    expect(sf.tx.filter(isData), 'the grant releases exactly what it credits').toHaveLength(1400)
    expect(sf.tx.filter(terminalOn(2))).toHaveLength(1)
    expect(sf.counters.snapshot().peerFlowResume).toBe(1)
    await sf.server.stop()
  })

  it('the park ends exactly at T_stall, UNAVAILABLE naming the window', async () => {
    const sf = srvFixture()
    await sf.handle(manyOpen(EPOCH_A, 1, 4096, W_CONN + 1))
    await settle()
    expect(sf.counters.snapshot().peerFlowStall).toBe(1)
    expect(sf.tx.filter(isTerminal)).toHaveLength(0)

    await vi.advanceTimersByTimeAsync(STALL_MS - 1)
    await settle()
    expect(sf.tx.filter(isTerminal), 'the park must not end early').toHaveLength(0)
    await vi.advanceTimersByTimeAsync(2)
    await settle()
    const term = sf.tx.find(isTerminal)
    expect(term, 'the park must end at T_stall').toBeDefined()
    expect(term!.code).toBe(Code.UNAVAILABLE)
    expect(term!.desc, 'the error must name the window that starved it').toContain('connection credit')
    expect(sf.counters.snapshot().peerFlowResume).toBe(0)
    await sf.server.stop()
  })

  // Pins "One park, one bound: the same T_stall (§10.1), armed at the first
  // park, measures the whole wait across both windows, and on expiry the
  // call fails UNAVAILABLE naming the window it was parked on": a handler
  // short on BOTH windows parks once, and it is the connection window it
  // names.
  it('short on both windows: one park, one T_stall, naming the connection window', async () => {
    const sf = srvFixture()
    await sf.handle(manyOpen(EPOCH_A, 1, 4096, W_CONN - 1))
    await settle()
    expect(sf.tx.filter(terminalOn(1))).toHaveLength(1) // one connection credit left
    await sf.handle(manyOpen(EPOCH_A, 2, 1, 2)) // a stream window of one, two responses
    await settle()
    expect(sf.tx.filter(isData), 'the first response took the last credit of both windows').toHaveLength(W_CONN)
    expect(sf.counters.snapshot(), 'short on both: the connection window is the one named').toMatchObject({ peerFlowStall: 1, flowStall: 0 })

    await vi.advanceTimersByTimeAsync(STALL_MS - 1)
    await settle()
    expect(sf.tx.filter(terminalOn(2)), 'one budget, not two: still parked short of T_stall').toHaveLength(0)
    await vi.advanceTimersByTimeAsync(2)
    await settle()
    const term = sf.tx.find(terminalOn(2))
    expect(term?.code).toBe(Code.UNAVAILABLE)
    expect(term!.desc).toContain('connection credit')
    expect(sf.counters.snapshot(), 'one stall, no resume').toMatchObject({ peerFlowStall: 1, flowStall: 0, peerFlowResume: 0, flowResume: 0 })
    await sf.server.stop()
  })
})

// ---------------------------------------------------------------------------
// §4.2.1 / §9.1 grants: a sid-0 WINDOW credits the (peer, client-epoch)
// container it names, in reliable mode, when that container exists. Anything
// else is dropped in silence — no RESET, no credit, no container.
// ---------------------------------------------------------------------------

describe('sid-0 grants (§4.2.1 Grants)', () => {
  // Pins: the server applies a sid-0 WINDOW only "when a container exists for
  // (peer, epoch) — and otherwise drops it silently: never validated (§9.1),
  // never answered with a RESET (§9.3), never creating state".
  it('before any OPEN: silent, and no state', async () => {
    const sf = srvFixture()
    await sf.handle(grant(EPOCH_A, 100))
    await tick()
    expect(sf.tx, 'a sid-0 grant before any OPEN is silent').toHaveLength(0)
    expect(slotsOf(sf.server).size, 'and creates no state').toBe(0)

    // A grant that DID land would be spent by the call below, which asks for
    // W_CONN + 100 and would complete.
    await sf.handle(manyOpen(EPOCH_A, 1, 4096, W_CONN + 100))
    await settle()
    expect(sf.tx.find(isHeaderFrame), 'the OPEN is admitted').toBeDefined()
    expect(sf.tx.filter(isData), "the sender is created from the OPEN's advertisement (W_CONN): the early grant credited nothing").toHaveLength(W_CONN)
    await sf.server.stop()
  })

  it('a foreign client epoch: silent, never a RESET', async () => {
    const sf = srvFixture()
    await sf.handle(manyOpen(EPOCH_A, 1, 4096, W_CONN + 100))
    await settle()
    await sf.handle(grant(EPOCH_B, 100)) // no container for B
    await settle()
    expect(sf.tx.filter(isData)).toHaveLength(W_CONN)
    expect(sf.tx.filter(isReset), 'never answered with a RESET').toHaveLength(0)

    await sf.handle(grant(EPOCH_A, 100)) // the real one
    await settle()
    expect(sf.tx.filter(isData)).toHaveLength(W_CONN + 100)
    await sf.server.stop()
  })

  // Pins "A receiver applies it only in reliable mode".
  it('unreliable mode: silent', async () => {
    const tx: Frame[] = []
    const server = new Server({ handle: (f: Frame) => void tx.push(wireClone(f)) }, { reliable: false, timing: fast })
    registerEcho(server)
    await server.handle(grant(EPOCH_A, 100), { peer: PEER })
    await tick()
    expect(tx, 'unreliable mode has no connection window').toHaveLength(0)
    await server.stop()
  })

  // Pins "A conn_window of 0 (absent) ... the sender's connection window is
  // then off toward that incarnation" and "a grant never enables".
  it('after the peer advertised no connection window: off, and a grant never enables', async () => {
    const sf = srvFixture()
    // No window at all on the container's first OPEN: the client does no
    // flow control, on either window.
    await sf.handle(manyOpen(EPOCH_A, 1, 0, 2 * W_CONN, 0))
    await settle()
    expect(sf.tx.filter(isData), 'no window binds').toHaveLength(2 * W_CONN)
    expect(sf.counters.snapshot()).toMatchObject({ peerFlowStall: 0, flowStall: 0 })

    await sf.handle(grant(EPOCH_A, 1)) // dropped: never enables
    await sf.handle(manyOpen(EPOCH_A, 2, 0, 2 * W_CONN, 0))
    await settle()
    expect(sf.tx.filter(isData)).toHaveLength(4 * W_CONN)
    expect(sf.counters.snapshot().peerFlowStall).toBe(0)
    await sf.server.stop()
  })
})

// ---------------------------------------------------------------------------
// §4.2.1 advertisement, the server's half: every H and every T the server
// sends carries its connection window, maxPeerWindow — the creation ack, a
// sendHeader flush, a unary or streaming T, a rejection terminal, stop's
// UNAVAILABLE — and no other server frame does. Nothing rides behind the
// first H.
// ---------------------------------------------------------------------------

describe('every H and T advertises the connection window (§4.2.1 Advertisement)', () => {
  const limits = { limits: { maxPeerWindow: 2048 } }
  // flush is a unary method whose handler flushes a header before answering
  // (§8, §11): the one server header frame no creation ack covers.
  const flush = unaryMethod<TestReq, TestRes>('/test.Echo/Flush', { request: jsonCodec(), response: jsonCodec() })
  const flushOpen = (epoch: number, sid: number): Frame =>
    frame({ epoch, sid, seq: 1, flags: FlagOpen | FlagClose, method: flush.path, window: 32, connWindow: W_CONN, payload: enc({ text: 'x' }) })
  const registerFlush = (server: Server): void =>
    server.register(flush, async (req, ctx) => {
      await ctx.sendHeader({ k: ['v'] })
      return { text: req.text }
    })
  const halfClose = (epoch: number, sid: number, seq: number): Frame => frame({ epoch, sid, seq, flags: FlagClose })

  // Pins "the server on every H and T (§7). Every frame of those kinds
  // carries it ... not a creation ack from a SendHeader flush, not a
  // streaming call from a unary one, not an admitted OPEN from a rejected
  // one".
  it("creation ack, sendHeader H, unary T, streaming T, rejection T and stop's T all carry conn_window = maxPeerWindow (2048)", async () => {
    const key = 'flow-live'
    const sf = srvFixture({ ...limits, streamInterceptors: [blockUnlessMarked(key)] })
    registerFlush(sf.server)
    await sf.handle(streamOpen(EPOCH_A, 1, 32, echo.count.path)) // blocked: alive at stop
    await sf.handle(onceOpen(EPOCH_A, 2))
    await sf.handle(flushOpen(EPOCH_A, 3))
    const open = streamOpen(EPOCH_A, 4, 32, echo.count.path)
    open.header = { [key]: ['1'] }
    await sf.handle(open)
    await sf.handle(halfClose(EPOCH_A, 4, 2)) // the count call ends: a streaming T
    await sf.handle(streamOpen(EPOCH_A, 5, 32, '/test.Echo/Nope'))
    await settle()

    const on = (sid: number): Frame[] => sf.tx.filter((f) => f.sid === sid)
    expect(on(1).filter(isHeaderFrame).map((f) => f.connWindow), 'the creation ack').toEqual([2048])
    expect(on(2).filter(isTerminal).map((f) => f.connWindow), 'the unary T').toEqual([2048])
    expect(on(3).filter(isHeaderFrame).map((f) => f.connWindow), 'the sendHeader H').toEqual([2048])
    expect(on(3).filter(isTerminal).map((f) => f.connWindow), 'the T behind it').toEqual([2048])
    expect(on(4).filter(isHeaderFrame).map((f) => f.connWindow), 'the creation ack of the live call').toEqual([2048])
    expect(on(4).filter(isTerminal).map((f) => f.connWindow), 'the streaming T').toEqual([2048])
    const rej = on(5).find(isTerminal)
    expect(rej?.code).toBe(Code.UNIMPLEMENTED)
    expect(rej!.connWindow, 'the rejection T').toBe(2048)
    expect(sf.tx.filter(isPeerGrant), 'nothing rides behind the first H: the advertisement is the whole of it').toHaveLength(0)

    await sf.server.stop()
    await settle()
    const stopped = on(1).find(isTerminal)
    expect(stopped?.code).toBe(Code.UNAVAILABLE)
    expect(stopped!.connWindow, "stop's T").toBe(2048)
  })

  it('a live-call cap rejection T carries it too', async () => {
    const sf = srvFixture({ limits: { maxPeerWindow: 2048, maxLiveCalls: 1 }, streamInterceptors: [blockStreams] })
    await sf.handle(streamOpen(EPOCH_A, 1, 32, echo.count.path))
    await sf.handle(streamOpen(EPOCH_A, 2, 32, echo.count.path))
    const rej = sf.tx.find(terminalOn(2))
    expect(rej?.code).toBe(Code.RESOURCE_EXHAUSTED)
    expect(rej!.connWindow).toBe(2048)
    await sf.server.stop()
  })

  // Pins Appendix B: MaxPeerWindow defaults to W_conn.
  it('W_CONN by default', async () => {
    const sf = srvFixture({ streamInterceptors: [blockStreams] })
    await sf.handle(onceOpen(EPOCH_A, 1))
    await sf.handle(streamOpen(EPOCH_A, 2, 32, echo.count.path))
    await settle()
    expect(sf.tx.find(isTerminal)!.connWindow).toBe(W_CONN)
    expect(sf.tx.find(isHeaderFrame)!.connWindow).toBe(W_CONN)
    await sf.server.stop()
  })

  // Pins "Data frames, WINDOW, PING, RESET never carry it".
  it('no other server frame carries it: data, WINDOW on either sid, RESET', async () => {
    const sf = srvFixture({ ...limits, rxBuffer: { size: W_CONN } })
    await sf.handle(manyOpen(EPOCH_A, 1, 4096, 5)) // five data frames and a T
    await sf.handle(streamOpen(EPOCH_A, 2, 32, echo.count.path))
    for (let i = 0; i < W_CONN; i++) await sf.handle(data(EPOCH_A, 2, 2 + i)) // consumed: grants on sid 2 and sid 0
    await sf.handle(data(EPOCH_A, 77, 2)) // an unknown sid: RESET
    await settle()
    expect(sf.tx.filter(isData)).toHaveLength(5)
    expect(sf.tx.filter(isPeerGrant).length, 'the sweep below includes a sid-0 grant').toBeGreaterThan(0)
    expect(sf.tx.filter((f) => shapeOf(f) === FlagWindow && f.sid === 2).length, 'and a per-stream grant').toBeGreaterThan(0)
    expect(sf.tx.filter(isReset)).toHaveLength(1)
    for (const f of sf.tx) {
      if (!isHeaderFrame(f) && !isTerminal(f)) expect(f.connWindow, `flags 0x${f.flags.toString(16)} sid ${f.sid}`).toBe(0)
    }
    await sf.server.stop()
  })

  // Pins "Unreliable mode has no connection window: no advertisement".
  it('unreliable mode advertises nothing', async () => {
    const tx: Frame[] = []
    const server = new Server({ handle: (f: Frame) => void tx.push(wireClone(f)) }, { reliable: false, timing: fast, ...limits })
    registerEcho(server)
    await server.handle(onceOpen(EPOCH_A, 1), { peer: PEER })
    await server.handle(streamOpen(EPOCH_A, 2, 32, echo.live.path), { peer: PEER })
    await settle()
    expect(tx.find(isTerminal)).toMatchObject({ connWindow: 0 })
    expect(tx.find(isHeaderFrame)).toMatchObject({ window: 0, connWindow: 0 })
    await server.stop()
  })
})

// ---------------------------------------------------------------------------
// §4.2.1 / §9.4: the container's sender is created from the OPEN that creates
// the container — admitted or rejected — the first advertisement winning, a
// later OPEN ignored, an absent one meaning off, the two windows apart.
// ---------------------------------------------------------------------------

describe("the container's sender is created from the OPEN's advertisement (§4.2.1 Advertisement, §9.4)", () => {
  const sender = (sf: ReturnType<typeof srvFixture>, epoch: number): SenderState | undefined =>
    slotsOf(sf.server).get(PEER)?.epochs.get(epoch)?.connTx.state()

  // Pins "the server from the OPEN that creates the (peer, client-epoch)
  // container (§9.4)" and "honours the advertisement from then on".
  it("honours the client's 2048: 2048 frames unparked, the 2049th parks until a sid-0 grant", async () => {
    const sf = srvFixture()
    await sf.handle(manyOpen(EPOCH_A, 1, 4096, 2049, 2048))
    await settle()
    expect(sf.tx.filter(isData), 'exactly the advertised window reaches the wire').toHaveLength(2048)
    expect(sf.counters.snapshot()).toMatchObject({ peerFlowStall: 1, flowStall: 0 })
    expect(sender(sf, EPOCH_A)).toMatchObject({ on: true, observed: true, granted: 2048, sent: 2048 })

    await sf.handle(grant(EPOCH_A, 1))
    await settle()
    expect(sf.tx.filter(isData)).toHaveLength(2049)
    expect(sf.tx.filter(terminalOn(1))).toHaveLength(1)
    expect(sf.counters.snapshot().peerFlowResume).toBe(1)
    await sf.server.stop()
  })

  // Pins "A peer applies the first advertisement it hears from a peer
  // incarnation and ignores the rest".
  it('a later OPEN with a different value is ignored', async () => {
    const sf = srvFixture()
    await sf.handle(onceOpen(EPOCH_A, 1, 2048)) // creates the container: 2048
    await settle()
    expect(sender(sf, EPOCH_A)).toMatchObject({ on: true, observed: true, granted: 2048 })
    await sf.handle(manyOpen(EPOCH_A, 2, 4096, 3000, 8192)) // 8192: ignored
    await settle()
    expect(sf.tx.filter(isData), 'the first advertisement rules').toHaveLength(2048)
    expect(sf.counters.snapshot().peerFlowStall).toBe(1)
    expect(sender(sf, EPOCH_A)).toMatchObject({ granted: 2048 })
    await sf.server.stop()
  })

  // Pins "A conn_window of 0 (absent) ... the sender's connection window is
  // then off toward that incarnation, whatever the per-stream window said —
  // which is what makes a partial implementation harmless".
  it('absent: off — the server streams unpaced on the connection window while the stream window still paces', async () => {
    const sf = srvFixture()
    await sf.handle(manyOpen(EPOCH_A, 1, 4096, 3 * W_CONN, 0))
    await settle()
    expect(sf.tx.filter(isData), 'no connection window binds').toHaveLength(3 * W_CONN)
    expect(sf.counters.snapshot()).toMatchObject({ peerFlowStall: 0, flowStall: 0 })
    expect(sender(sf, EPOCH_A)).toMatchObject({ on: false, observed: true })

    // Another incarnation whose OPEN advertises only its stream window (32):
    // the handler parks there, and only there.
    await sf.handle(manyOpen(EPOCH_B, 1, 32, 40, 0))
    await settle()
    expect(sf.tx.filter((f) => isData(f) && f.peerEpoch === EPOCH_B)).toHaveLength(32)
    expect(sf.counters.snapshot()).toMatchObject({ peerFlowStall: 0, flowStall: 1 })
    await sf.server.stop()
  })

  // The two advertisements are read apart: a per-stream window of 0 says
  // nothing about the connection window beside it.
  it('a stream window of 0 beside conn_window 1024 still parks on the connection window', async () => {
    const sf = srvFixture()
    await sf.handle(manyOpen(EPOCH_A, 1, 0, W_CONN + 1, W_CONN))
    await settle()
    expect(sf.tx.filter(isData)).toHaveLength(W_CONN)
    expect(sf.counters.snapshot()).toMatchObject({ peerFlowStall: 1, flowStall: 0 })
    await sf.server.stop()
  })

  // Pins §9.4 Container on rejection: "the OPEN was validated (§9.1) and
  // carries the client's advertisement, from which the container's sender
  // is created".
  it("a rejected first OPEN — unknown method — creates the container's sender from its advertisement", async () => {
    const sf = srvFixture()
    // The incarnation's first OPEN names a method that does not exist.
    await sf.handle(streamOpen(EPOCH_A, 1, 32, '/test.Echo/Nope', 2048))
    const rej = sf.tx.find(isTerminal)
    expect(rej?.code, 'rejected').toBe(Code.UNIMPLEMENTED)
    expect(rej!.peerEpoch).toBe(EPOCH_A)
    expect(sender(sf, EPOCH_A), 'created from the rejected OPEN').toMatchObject({ on: true, observed: true, granted: 2048, sent: 0 })

    // The incarnation's next OPEN advertises something else — ignored — and
    // asks for 2049 unread responses: exactly the advertised 2048 go out.
    await sf.handle(manyOpen(EPOCH_A, 2, 4096, 2049, 8192))
    await settle()
    expect(sf.tx.filter(isData), "the rejected OPEN's window rules").toHaveLength(2048)
    expect(sf.counters.snapshot().peerFlowStall).toBe(1)
    await sf.server.stop()
  })

  it("a first OPEN refused by the live-call cap creates the container's sender from its advertisement", async () => {
    const sf = srvFixture({ limits: { maxLiveCalls: 1 }, streamInterceptors: [blockStreams] })
    await sf.handle(streamOpen(EPOCH_A, 1, 32, echo.count.path)) // A fills the peer's cap
    await sf.handle(streamOpen(EPOCH_B, 1, 32, echo.count.path, 2048)) // B's first OPEN: refused
    const rej = sf.tx.find((f) => isTerminal(f) && f.peerEpoch === EPOCH_B)
    expect(rej?.code).toBe(Code.RESOURCE_EXHAUSTED)
    expect(sender(sf, EPOCH_B), "created from B's refused OPEN").toMatchObject({ on: true, observed: true, granted: 2048, sent: 0 })
    await sf.server.stop()
  })
})

// ---------------------------------------------------------------------------
// §4.2 / §4.2.1 / §15 receiving: one transport peer may not have more than
// maxPeerWindow buffered here across all of its calls. The frame that would
// exceed it is never buffered and fails ITS call INTERNAL — the other call is
// untouched — and the credit of both the refused frame and the failed call's
// discarded frames comes back on sid 0.
// ---------------------------------------------------------------------------

describe("the receiver bound (§4.2.1 Overrun, The receiver's ledger)", () => {
  // Pins "A data frame that would take the peer past MaxPeerWindow is not
  // buffered; the receiver fails the call it is addressed to with INTERNAL
  // ... and returns the frame's credit as never buffered".
  it('an overrun fails only the offending call; the refused frame and the discarded ones return their credit', async () => {
    // Per-stream buffers of W_CONN each, handlers that never read: only the
    // connection window can trip.
    const sf = srvFixture({ rxBuffer: { size: W_CONN }, streamInterceptors: [blockStreams] })
    await sf.handle(streamOpen(EPOCH_A, 1, 32, echo.live.path))
    await sf.handle(streamOpen(EPOCH_A, 2, 32, echo.live.path))

    // Exactly the window, split across the two calls.
    for (let i = 0; i < W_CONN / 2; i++) {
      await sf.handle(data(EPOCH_A, 1, 2 + i))
      await sf.handle(data(EPOCH_A, 2, 2 + i))
    }
    await settle()
    expect(sf.tx.filter(isTerminal), 'the window fits').toHaveLength(0)
    expect(sf.tx.filter(isPeerGrant), 'nothing consumed, nothing granted').toHaveLength(0)

    // One past it, on 2: call 2 fails, call 1 does not.
    await sf.handle(data(EPOCH_A, 2, 2 + W_CONN / 2))
    await settle()
    const term = sf.tx.find(terminalOn(2))
    expect(term, 'the offending call aborts').toBeDefined()
    expect(term!.code).toBe(Code.INTERNAL)
    expect(term!.desc, 'the error must name the connection window').toContain('connection flow-control window')
    expect(sf.tx.filter(terminalOn(1)), 'the other call is untouched').toHaveLength(0)
    expect(sf.tx.filter(isReset)).toHaveLength(0)

    // The refused frame's credit came back at once (the starvation clause:
    // the window was full), and the failed call's 512 discarded frames came
    // back in one grant when it finished.
    expect(peerGrants(sf.tx)).toEqual({ n: 2, total: W_CONN / 2 + 1 })
    expect(sf.tx[sf.tx.length - 1]!.peerEpoch).toBe(EPOCH_A)

    // Call 1 is live and, with call 2's frames gone, there is room again.
    await sf.handle(data(EPOCH_A, 1, 2 + W_CONN / 2))
    await settle()
    expect(sf.tx.filter(terminalOn(1))).toHaveLength(0)
    expect(sf.tx.filter(isReset)).toHaveLength(0)
    await sf.server.stop()
  })

  // Pins §4.2.1 Cadence: "each is granted exactly what its own frames
  // returned, in a grant naming it (peer_epoch)".
  it('consumed frames return their credit on sid 0, batched at half the window', async () => {
    const sf = srvFixture({ rxBuffer: { size: W_CONN } })
    await sf.handle(streamOpen(EPOCH_A, 1, 32, echo.count.path))
    const injected = 600
    for (let i = 0; i < injected; i++) await sf.handle(data(EPOCH_A, 1, 2 + i))
    await settle() // the count handler consumes everything it is given
    expect(peerGrants(sf.tx), 'batched at half the window: 600 consumed is one grant').toEqual({ n: 1, total: W_CONN / 2 })
    expect(sf.tx.find(isPeerGrant)!.peerEpoch).toBe(EPOCH_A)
    // The per-stream grant is still there, on the call's own sid.
    expect(sf.tx.filter((f) => shapeOf(f) === FlagWindow && f.sid === 1)).toHaveLength(1)
    await sf.server.stop()
  })
})

// ---------------------------------------------------------------------------
// §4.2.1 credit return, the leak test: every reliable-mode data frame received
// returns one credit on sid 0 once it stops occupying a buffer — the frames
// pipelined behind an abort (RESET-drawn or discarded with the call), an
// off-shape frame, a frame for a finished sid, a frame from an incarnation
// whose container the cap evicted (§9.4) — and the server-streaming request
// that rode the OPEN returns none, because the client never charged it.
// Grants batch at half the window, so each case is primed to the edge.
// ---------------------------------------------------------------------------

describe('every non-buffered frame returns its credit (§4.2.1 The receiver’s ledger)', () => {
  it('frames behind an abort', async () => {
    const behind = 40
    const sf = srvFixture({ streamInterceptors: [blockStreams] })
    await sf.handle(streamOpen(EPOCH_A, 1, 32, echo.live.path))
    await prime(sf, W_CONN / 2 - behind)
    expect(sf.tx.filter(isReset)).toHaveLength(W_CONN / 2 - behind)
    expect(sf.tx.filter(isPeerGrant), 'one short of the batch').toHaveLength(0)

    // The abort, then 40 data frames pipelined behind it: each is either
    // discarded with the call or RESET-drawn — and either way its credit is
    // exactly what completes the batch.
    await sf.handle(abortFrame(EPOCH_A, 1, 2))
    for (let i = 0; i < behind; i++) await sf.handle(data(EPOCH_A, 1, 3 + i))
    await settle()
    expect(peerGrants(sf.tx)).toEqual({ n: 1, total: W_CONN / 2 })
    await sf.server.stop()
  })

  it('off-shape data on a server-streaming call', async () => {
    const sf = srvFixture({ streamInterceptors: [blockStreams] })
    await sf.handle(manyOpen(EPOCH_A, 1, 32, 1))
    await prime(sf, W_CONN / 2 - 1)
    await sf.handle(data(EPOCH_A, 1, 2)) // no client data frames on this shape
    await settle()
    expect(sf.tx.filter(isTerminal), 'off-shape is dropped, the call lives').toHaveLength(0)
    expect(peerGrants(sf.tx)).toEqual({ n: 1, total: W_CONN / 2 })
    await sf.server.stop()
  })

  it('off-shape data on a unary call', async () => {
    // A unary handler that parks, so the call is live — and has no client
    // data frames in its shape — when the frame lands.
    const block: UnaryServerInterceptor = async (_req, ctx) => {
      await aborted(ctx.signal)
      throw abortCause(ctx.signal)
    }
    const sf = srvFixture({ unaryInterceptors: [block] })
    await sf.handle(onceOpen(EPOCH_A, 1))
    await settle()
    await prime(sf, W_CONN / 2 - 1)
    await sf.handle(data(EPOCH_A, 1, 2)) // no client data frames on this shape
    await settle()
    expect(sf.tx.filter(isTerminal), 'off-shape is dropped, the call lives').toHaveLength(0)
    expect(peerGrants(sf.tx)).toEqual({ n: 1, total: W_CONN / 2 })
    await sf.server.stop()
  })

  // Pins §4.2.1: the server returns credit "to an incarnation it holds flow
  // state for — a container, or the sender position its ledger kept when the
  // container cap evicted one (§9.4)".
  it('a frame from an incarnation the container cap evicted', async () => {
    // Several Conns on one socket: the cap evicts an idle one's container and
    // the ledger keeps its sender position (§9.4). A data frame that Conn
    // still had in flight for a call the server finished draws its RESET and
    // returns its credit to that position, exactly as it would to the
    // container.
    const sf = srvFixture({ limits: { maxDeadPeers: 2 } })
    const idle = async (epoch: number): Promise<void> => {
      await sf.handle(onceOpen(epoch, 1))
      await settle() // the call ran: the container is idle
      vi.advanceTimersByTime(1000) // containers are evicted oldest first
    }
    await idle(EPOCH_A)
    await prime(sf, W_CONN / 2 - 1)
    expect(sf.tx.filter(isPeerGrant), 'one short of the batch').toHaveLength(0)
    await idle(EPOCH_B)
    await idle(EPOCH_B + 1) // the third idle incarnation evicts A, the oldest
    // A's straggler, for its finished call.
    await sf.handle(data(EPOCH_A, 1, 2))
    await tick()
    expect(sf.tx.filter(isReset), 'RESET-drawn like any other').toHaveLength(W_CONN / 2)
    expect(peerGrants(sf.tx)).toEqual({ n: 1, total: W_CONN / 2 })
    expect(sf.tx.find(isPeerGrant)!.peerEpoch, 'granted to the incarnation that spent it').toBe(EPOCH_A)
    await sf.server.stop()
  })

  // Pins "Two exclusions, both because the sender never charged the frame:
  // the server-streaming OPEN payload (§8)".
  it('the server-streaming request rode the OPEN uncredited', async () => {
    const sf = srvFixture()
    await sf.handle(streamOpen(EPOCH_A, 1, 32, echo.count.path))
    await prime(sf, W_CONN / 2 - 1)
    // A whole server-streaming call: its request is read by the handler out
    // of the same buffer as any data frame would be.
    await sf.handle(manyOpen(EPOCH_A, 2, 32, 1))
    await settle()
    expect(sf.tx.filter(terminalOn(2))).toHaveLength(1)
    expect(sf.tx.filter(isPeerGrant), 'the request returned nothing: one short still').toHaveLength(0)
    await prime(sf, 1) // control: the edge is exactly here
    expect(peerGrants(sf.tx)).toEqual({ n: 1, total: W_CONN / 2 })
    await sf.server.stop()
  })
})

// ---------------------------------------------------------------------------
// §4.2.1 sending, the late send: a handler's send after the client's abort is
// the ordinary "send until it errors" loop racing a cancel. It must spend
// nothing on the connection window toward that client — shared by every call
// to it and cumulative — or each cancelled call is a permanent shrink.
// ---------------------------------------------------------------------------

describe('a send that never reaches the wire refunds the connection window (§4.2.1 Sending)', () => {
  // Pins "a send that never reaches the wire — the adapter refused it (§4.4),
  // or the call ended first — refunds both".
  it('a send after the client’s abort spends nothing', async () => {
    const cancelled = 40
    const key = 'flow-healthy'
    // Unmarked calls wait for the client's abort and then send once; the
    // marked one runs the real handler.
    const lateSend: StreamServerInterceptor = async (stream, ctx, next) => {
      if (isMarked(ctx, key)) return next(stream, ctx)
      await aborted(ctx.signal)
      await stream.send({ text: 'late' }).catch(noop)
      return undefined
    }
    const sf = srvFixture({ streamInterceptors: [lateSend] })
    for (let sid = 1; sid <= cancelled; sid++) {
      await sf.handle(manyOpen(EPOCH_A, sid, 4096, 1))
      await settle() // the handler waits on its call
      await sf.handle(abortFrame(EPOCH_A, sid, 2))
      await settle() // it sent into the ended call and unwound
    }
    expect(sf.tx.filter(isData), 'no late send reached the wire').toHaveLength(0)

    // A healthy call now moves the whole W_CONN: none of the late sends
    // spent a credit. A leak of one per cancelled call would park it 40
    // short of the window.
    const open = manyOpen(EPOCH_A, cancelled + 1, 4096, W_CONN)
    open.header = { [key]: ['1'] }
    await sf.handle(open)
    await settle()
    expect(sf.tx.filter(isData), 'exactly W_CONN data frames reach the wire').toHaveLength(W_CONN)
    expect(sf.counters.snapshot().peerFlowStall, 'nothing was spent on the connection window').toBe(0)
    expect(sf.tx.filter(terminalOn(cancelled + 1)), 'and the call completed').toHaveLength(1)
    await sf.server.stop()
  })

  // The call ends in the one window the sender's own checks cannot cover:
  // after acquireBoth handed back the credit and before the frame is built
  // (the server_internal_test.go twin).
  it('the call ended after the credit was taken: refunded, nothing on the wire', async () => {
    const tx: Frame[] = []
    let server: Server
    const endUnderTheSend: ProtocolStats = (ev) => {
      // The grant woke the parked send and acquireBoth handed it both
      // credits; the client's abort lands now, before the frame is built —
      // synchronously, as an abort frame's delivery is.
      if (ev.kind === 'flow-resume') void server.handle(abortFrame(EPOCH_A, 1, 2), { peer: PEER })
    }
    server = new Server({ handle: (f: Frame) => void tx.push(wireClone(f)) }, { reliable: true, protocolStats: endUnderTheSend })
    const results: unknown[] = []
    server.register(echo.many, async (_req, stream) => {
      results.push(await stream.send({ text: 'a' }).then(() => undefined, (e: unknown) => e)) // the stream's one credit
      results.push(await stream.send({ text: 'b' }).then(() => undefined, (e: unknown) => e)) // parks on the stream window
    })

    await server.handle(manyOpen(EPOCH_A, 1, 1, 0), { peer: PEER }) // a stream window of one
    await settle() // the first send went out, the second is parked
    expect(results).toEqual([undefined])
    const ps = slotsOf(server).get(PEER)!.epochs.get(EPOCH_A)!
    expect(ps.connTx.state().sent, 'a park holds no connection credit').toBe(1)

    await server.handle(frame({ epoch: EPOCH_A, sid: 1, flags: FlagWindow, window: 1 }), { peer: PEER })
    await settle()
    expect(results).toHaveLength(2)
    expect((results[1] as StatusError).code, 'send on the ended call').toBe(Code.CANCELLED)
    expect(tx.filter(isData), 'the second never went out').toHaveLength(1)
    expect(ps.connTx.state().sent, 'the credit of a frame that never went out is refunded').toBe(1)
    await server.stop()
  })
})

// ---------------------------------------------------------------------------
// §4.2.1 / §4.5 / §9.4 disconnectPeer: a handler parked on connection credit
// has no grant coming once the peer is gone — the teardown releases the
// sender itself — and the slot goes with its containers and the ledger:
// reliable containers are never swept, so nothing else would ever reclaim
// them, and a stray grant afterwards must not bring any of it back.
// ---------------------------------------------------------------------------

describe('disconnectPeer (§4.2.1 Sending, §4.5)', () => {
  // Pins "Conn.Close and DisconnectPeer (§4.5) release a parked connection
  // sender, as a call's end releases a parked stream sender", and §9.4:
  // reliable containers live "until adapter teardown (DisconnectPeer /
  // Conn.Close), never swept" (the server_internal_test.go twin).
  it('unparks a handler waiting on connection credit and reclaims the containers and the ledger', async () => {
    let unwound: unknown = 'still parked'
    const observe: StreamServerInterceptor = async (stream, ctx, next) => {
      try {
        return await next(stream, ctx)
      } catch (e) {
        unwound = e
        throw e
      }
    }
    const sf = srvFixture({ streamInterceptors: [observe] })
    await sf.handle(manyOpen(EPOCH_A, 1, 4096, W_CONN + 1))
    await settle()
    expect(sf.counters.snapshot().peerFlowStall).toBe(1)
    expect(unwound).toBe('still parked')
    expect(slotsOf(sf.server).get(PEER)?.flow, 'the peer has a container and a ledger').toBeDefined()

    sf.server.disconnectPeer(PEER, new Error('gone'))
    await settle()
    expect((unwound as StatusError).code, 'the parked send failed with the disconnect cause').toBe(Code.UNAVAILABLE)
    expect((unwound as StatusError).desc).toContain('gone')
    expect(sf.counters.snapshot().peerFlowResume, 'no credit ever came').toBe(0)
    expect(slotsOf(sf.server).has(PEER), 'no container and no ledger leaked past disconnectPeer').toBe(false)

    // The peer's straggling grant afterwards: no sender to credit, and no
    // state recreated for it (§4.2.1 Grants).
    const at = sf.tx.length
    await sf.handle(grant(EPOCH_A, 100))
    await tick()
    expect(sf.tx.slice(at), 'silent').toHaveLength(0)
    expect(slotsOf(sf.server).size, 'and creates nothing').toBe(0)
    await sf.server.stop()
  })
})

// ---------------------------------------------------------------------------
// §4.2.1 cadence across incarnations: the ledger is per transport peer, the
// grants are per incarnation. A dead incarnation's bulk return must not carry
// the live one's credit away — that credit would be dropped by the client and
// lost for good.
// ---------------------------------------------------------------------------

describe('credit is granted to the incarnation that spent it (§4.2.1 Cadence)', () => {
  // Pins "credit is held back and granted per incarnation: each is granted
  // exactly what its own frames returned".
  it("a dead incarnation's bulk return carries nothing of the live one's", async () => {
    const key = 'flow-live'
    const half = W_CONN / 2
    const sf = srvFixture({ rxBuffer: { size: W_CONN }, streamInterceptors: [blockUnlessMarked(key)] })

    // Incarnation A: a client-streaming call under a stuck consumer, 300
    // buffered. Then the client restarts at the same key as B: its call is
    // consumed promptly, 300 sent.
    await sf.handle(streamOpen(EPOCH_A, 1, 32, echo.count.path))
    for (let i = 0; i < 300; i++) await sf.handle(data(EPOCH_A, 1, 2 + i))
    const open = streamOpen(EPOCH_B, 1, 32, echo.count.path)
    open.header = { [key]: ['1'] }
    await sf.handle(open)
    for (let i = 0; i < 300; i++) await sf.handle(data(EPOCH_B, 1, 2 + i))
    await settle()
    expect(sf.tx.filter(isPeerGrant), '300 held for B, 300 buffered for A: nothing due').toHaveLength(0)

    // A's call ends and its 300 are discarded: that credit is A's, dead with
    // it. B's 300 stay held for B — nothing is due to anyone.
    await sf.handle(abortFrame(EPOCH_A, 1, 302))
    await settle()
    expect(sf.tx.filter(isPeerGrant), "A's bulk return carries nothing of B's").toHaveLength(0)

    // B reaches its own half window and is granted exactly that, to B.
    for (let i = 0; i < half - 300; i++) await sf.handle(data(EPOCH_B, 1, 302 + i))
    await settle()
    const grants = sf.tx.filter(isPeerGrant)
    expect(grants).toHaveLength(1)
    expect(grants[0]!.window).toBe(half)
    expect(grants[0]!.peerEpoch, "B's credit goes to B").toBe(EPOCH_B)
    await sf.server.stop()
  })
})

// ---------------------------------------------------------------------------
// §9.4 / §15: the ledger keeps the evicted container's connection sender —
// its window and its credit — so that the incarnation's next OPEN continues
// it instead of recreating it from that OPEN's advertisement; a sid-0 grant
// addressed to it in the meantime still lands. (The server_internal_test.go
// twin.)
// ---------------------------------------------------------------------------

describe('an evicted container continues its sender (§4.2.1, §9.4)', () => {
  it("is recreated from the held position — its window and its credit — not from its next OPEN's advertisement", async () => {
    const sf = srvFixture({ limits: { maxDeadPeers: 2 } })
    const open = async (epoch: number, sid: number, connWindow = W_CONN): Promise<void> => {
      await sf.handle(onceOpen(epoch, sid, connWindow))
      await settle() // the call ran to completion: the container is idle
      vi.advanceTimersByTime(1000) // containers are evicted oldest first
    }
    const container = (epoch: number): Container | undefined => slotsOf(sf.server).get(PEER)?.epochs.get(epoch)

    // Incarnation 1: its sender created from its OPEN's advertisement
    // (W_CONN), lifted by the client's grants to 4096.
    await open(1, 1)
    const ps = container(1)
    expect(ps, 'a container for epoch 1').toBeDefined()
    expect(ps!.connTx.state()).toMatchObject({ on: true, observed: true, granted: W_CONN, sent: 0 })
    await sf.handle(grant(1, 3 * W_CONN))
    expect(ps!.connTx.state()).toMatchObject({ on: true, granted: 4 * W_CONN, sent: 0 })

    // Two more idle incarnations: the third OPEN evicts 1, the oldest.
    await open(2, 1)
    await open(3, 1)
    expect(container(1), 'epoch 1 must have been evicted by the cap').toBeUndefined()
    expect(container(2)).toBeDefined()
    expect(container(3)).toBeDefined()

    // The client returns credit to the evicted incarnation: it lands on the
    // held position, not on the floor.
    await sf.handle(grant(1, 5))

    // A fourth idle incarnation evicts 2: the ledger now holds as many
    // positions as the cap holds containers — 1 and 2, oldest first — and
    // epoch 1 is its oldest entry. Exactly 2 × maxDeadPeers idle
    // incarnations on the key: the bound §16 states holds exactly.
    await open(4, 1)
    expect(container(2), 'epoch 2 must have been evicted by the cap').toBeUndefined()

    // Its next OPEN recreates the container from that position — not from
    // what that OPEN advertises (8192: ignored, the position's latch is set).
    // That OPEN itself evicts 3 into a full ledger: the position of the one
    // coming back must not be what the trim drops.
    await open(1, 2, 8192)
    const back = container(1)
    expect(back, 'epoch 1 must be back').toBeDefined()
    expect(back!.connTx.state(), 'the held position, advertisement latch included').toMatchObject({ on: true, observed: true, granted: 4 * W_CONN + 5, sent: 0 })
    const pf = slotsOf(sf.server).get(PEER)!.flow!
    expect(pf.unstash(1), 'the position went back into the container: the ledger holds it no more').toBeUndefined()
    // The ledger holds the two the cap evicted and nothing was trimmed: 2,
    // then 3 (evicted by epoch 1's return).
    expect(pf.unstash(2), "epoch 2's position must still be held").toBeDefined()
    expect(pf.unstash(3), "epoch 3's position must still be held").toBeDefined()
    await sf.server.stop()
  })
})

// ---------------------------------------------------------------------------
// Counters: a park on the connection window is a peer-flow stall, not a flow
// stall, and the event carries the parked call's sid and method — the server
// half of the Go stats_test.go coverage.
// ---------------------------------------------------------------------------

describe('peer-flow-stall / peer-flow-resume (§14)', () => {
  // Pins §14: "flow-stall counters (per stream and per peer, §4.2.1)".
  it('names the peer and the parked call, and the resume names the same window', async () => {
    const sf = srvFixture()
    await sf.handle(manyOpen(EPOCH_A, 7, 4096, W_CONN + 1))
    await settle()
    expect(sf.counters.snapshot()).toMatchObject({ peerFlowStall: 1, flowStall: 0, peerFlowResume: 0 })
    expect(sf.log.of('flow-stall')).toEqual([])
    expect(sf.log.first('peer-flow-stall')).toMatchObject({ peer: PEER, sid: 7, method: echo.many.path, count: 0 })

    await sf.handle(grant(EPOCH_A, 1))
    await settle()
    expect(sf.counters.snapshot()).toMatchObject({ peerFlowStall: 1, peerFlowResume: 1, flowStall: 0, flowResume: 0 })
    expect(sf.log.first('peer-flow-resume')).toMatchObject({ peer: PEER, sid: 7, method: echo.many.path })
    expect(sf.tx.filter(isTerminal)).toHaveLength(1)
    await sf.server.stop()
  })
})

// ---------------------------------------------------------------------------
// §4.2.1 end to end, TS↔TS.
// ---------------------------------------------------------------------------

describe('end to end (§4.2.1)', () => {
  // Pins §4.2.1 Cadence: "It MUST grant, whatever it holds back, whenever
  // buffered + held back ≥ MaxPeerWindow": 17 client-streaming consumers
  // stuck at full 32-message windows pin 544 of the 1024 connection window,
  // so pending can never reach half of it — yet an 18th stream consumed
  // promptly moves 2000 messages.
  it('the starvation clause: stuck consumers do not starve a healthy stream', async () => {
    const stuck = 17
    const moved = 2000
    const healthyKey = 'flow-healthy'
    const counters = new Counters()
    // A queued delivery loop each way, like Go's pipe: the sender can outrun
    // the grants coming back and actually park on the connection window.
    const net = makeLoopNet({
      connOpts: { protocolStats: counters.observe },
      serverOpts: { streamInterceptors: [blockUnlessMarked(healthyKey)] },
    })

    for (let i = 0; i < stuck; i++) {
      const s = net.conn.newStream(echo.count, {})
      for (let k = 0; k < W_INIT; k++) await s.send({ text: 'm' }) // exactly a window each: never parks
    }
    await net.settle()
    expect(counters.snapshot()).toMatchObject({ flowStall: 0, peerFlowStall: 0 })

    const healthy = net.conn.newStream(echo.count, { metadata: { [healthyKey]: ['1'] } })
    for (let i = 0; i < moved; i++) await healthy.send({ text: 'm' })
    healthy.closeSend()
    expect(await healthy.recv()).toEqual({ text: String(moved) })

    const snap = counters.snapshot()
    expect(snap.peerFlowStall, 'the sender did run out of connection credit').toBeGreaterThan(0)
    expect(snap.peerFlowResume, 'and was released every time').toBe(snap.peerFlowStall)
    const grants = net.s2c.sent.filter(isPeerGrant)
    expect(grants.length, 'the starvation clause must have fired').toBeGreaterThan(0)
    for (const g of grants) expect(g.window, 'granted below half the window').toBeLessThan(W_CONN / 2)
  })

  // Pins §4.2.1: "The connection window, one per peer, bounds what a peer can
  // pin across all of its calls (§15)" — and the channel stays live under it
  // (§4.2): two server-streaming calls left unread exceed the client's
  // connection window, the server parks on it — not on either stream window
  // — and a unary call still completes. As the application consumes, sid-0
  // grants resume it and every message arrives.
  it('a park across streams keeps the channel live', async () => {
    const counters = new Counters()
    const net = makeNet({
      reliable: true,
      connOpts: { rxBuffer: { size: W_CONN } },
      serverOpts: { rxBuffer: { size: W_CONN }, protocolStats: counters.observe },
    })
    const burst = 600 // two of them: past W_CONN, within each stream window
    const stalled = [net.conn.newStream(echo.many, {}), net.conn.newStream(echo.many, {})]
    for (const s of stalled) await s.send({ text: 'm', n: burst })
    for (let i = 0; i < 5000 && counters.snapshot().peerFlowStall === 0; i++) await tick()
    expect(counters.snapshot().peerFlowStall, 'the producer parks on the connection window').toBeGreaterThan(0)
    expect(counters.snapshot().flowStall, 'neither stream window is full').toBe(0)

    expect(await net.conn.invoke(echo.once, { text: 'abc' })).toEqual({ text: 'echo:abc' })

    for (const s of stalled) {
      let n = 0
      for await (const res of s) {
        expect(res.text).toBe(`m#${n}`)
        n++
      }
      expect(n).toBe(burst)
    }
    expect(counters.snapshot().peerFlowResume).toBeGreaterThanOrEqual(1)
    expect(net.sentC2S.filter(isPeerGrant).length, 'the client granted on sid 0').toBeGreaterThan(0)
  })

  // Pins §4.2.1 Grants: a sid-0 WINDOW "is the only thing that adds
  // connection credit" — both sides send it and honour it, or neither
  // direction passes W_CONN. The TS twin of the cross-language case: three
  // bidi streams interleaved, 400 messages each way, under default options.
  it('three streams past W_CONN both ways complete on sid-0 grants', async () => {
    const net = makeNet({ reliable: true })
    const streams = 3
    const each = 400 // 1200 data frames each way > W_CONN
    const live = Array.from({ length: streams }, () => net.conn.newStream(echo.live, {}))
    for (let i = 0; i < each; i++) {
      for (const [k, s] of live.entries()) {
        await s.send({ text: `${k}/${i}` })
        expect(await s.recv()).toEqual({ text: `echo:${k}/${i}` })
      }
    }
    for (const s of live) {
      s.closeSend()
      expect(await s.recv()).toBeUndefined()
    }
    const sids = new Set(net.sentC2S.filter(isOpen).map((f) => f.sid))
    expect(sids.size).toBe(streams)
    const clientEpoch = net.sentC2S.find(isOpen)!.epoch

    const check = (dir: string, frames: readonly Frame[], serverSent: boolean): void => {
      const { n, total } = peerGrants(frames)
      expect(n, `${dir}: no sid-0 grant`).toBeGreaterThan(0)
      expect(total, `${dir}: too little credit returned`).toBeGreaterThanOrEqual(W_CONN / 2)
      for (const f of frames) {
        if (shapeOf(f) !== FlagWindow) continue
        if (f.sid === 0) {
          expect(f.window).toBeGreaterThan(0)
          if (serverSent) expect(f.peerEpoch, `${dir}: a server grant names the client incarnation`).toBe(clientEpoch)
          continue
        }
        expect(sids.has(f.sid), `${dir}: a per-stream grant on a live sid, got ${f.sid}`).toBe(true)
      }
    }
    check('client->server', net.sentC2S, false)
    check('server->client', net.sentS2C, true)
  })
})
