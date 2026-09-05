// The observability surface (PROTOCOL.md §14) — the TS twin of stats_test.go's
// ProtocolStats half. Every event kind Go emits is emitted from the same
// decision point here, with the same fields: sid and method for a call's
// events, the transport peer on every server-side one, and a count only where
// there is a magnitude (skipped, dropped, off-shape).
//
// The one that matters most is the first: a gap is not an error (§14 promises
// an ordered subsequence; §6.3 accepts any forward step within W_fwd), so this
// counter is the only way a receiver — and on the lossy path that is the
// browser — ever learns that the wire ate something.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { Conn } from '../src/conn'
import { DropPolicy } from '../src/limits'
import { Code, type StatusError } from '../src/status'
import { Counters, type ProtocolEvent, type ProtocolEventKind, type ProtocolStats } from '../src/stats'
import { Latch } from '../src/util'
import type { Timing } from '../src/timing'
import { FlagPing, FlagReset, frame, isData, isOpen, isReset, isTerminal, type Frame } from '../src/wire'
import { echo, makeNet, tick, wireClone } from '../src/testing'

const enc = (v: unknown) => new TextEncoder().encode(JSON.stringify(v))

// EventLog keeps every event, the way stEventLog does in Go, so a test can
// say not just how many but which call and which peer.
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

// Both ends observed by a Counters and an EventLog each — installed as an
// array, which is the TS spelling of Go's "may be given more than once".
function observed(opts: { reliable: boolean; timing?: Timing; connRx?: { size: number; policy: DropPolicy } }) {
  const c = { counters: new Counters(), log: new EventLog() }
  const s = { counters: new Counters(), log: new EventLog() }
  const net = makeNet({
    reliable: opts.reliable,
    connOpts: { timing: opts.timing, rxBuffer: opts.connRx, protocolStats: [c.counters.observe, c.log.observe] },
    serverOpts: { timing: opts.timing, protocolStats: [s.counters.observe, s.log.observe] },
  })
  return { net, c, s }
}

// isData is Go's stIsData: shape 0 with a payload — never the H ack, never
// the terminal, never the OPEN.

// ---------------------------------------------------------------------------
// §14: the skipped-message counter
// ---------------------------------------------------------------------------

describe('skipped (§14, §6.3)', () => {
  it('counts the messages a gap ate, one event per gap, naming the call', async () => {
    const { net, c } = observed({ reliable: false })
    // Every third server DATA frame vanishes: 3 of the 9 responses.
    let n = 0
    net.s2c.filter = (f) => !(isData(f) && ++n % 3 === 0)

    const stream = net.conn.newStream(echo.many, {})
    await stream.send({ text: 'm', n: 9 })
    const got: string[] = []
    for await (const res of stream) got.push(res.text) // a gap is never an error

    // Six of nine delivered, in order, and the three that were dropped are
    // exactly what the counter reports.
    expect(got).toEqual(['m#0', 'm#1', 'm#3', 'm#4', 'm#6', 'm#7'])
    expect(c.counters.snapshot().skipped).toBe(3)

    const evs = c.log.of('skipped')
    expect(evs).toHaveLength(3)
    for (const ev of evs) {
      expect(ev.count).toBe(1) // messages this gap ate
      expect(ev.sid).toBe(1) // the first call of the Conn
      expect(ev.method).toBe(echo.many.path)
      expect(ev.peer).toBeUndefined() // a Conn is one peer; nothing to name
    }
  })

  it('a wider gap is one event with the whole count', async () => {
    const { net, c } = observed({ reliable: false })
    let n = 0
    net.s2c.filter = (f) => !(isData(f) && [2, 3, 4].includes(++n))

    const stream = net.conn.newStream(echo.many, {})
    await stream.send({ text: 'm', n: 6 })
    const got: string[] = []
    for await (const res of stream) got.push(res.text)
    expect(got).toEqual(['m#0', 'm#4', 'm#5'])

    expect(c.counters.snapshot().skipped).toBe(3)
    expect(c.log.of('skipped').map((e) => e.count)).toEqual([3])
  })

  it('the server counts gaps in what the client streams to it', async () => {
    const { net, s } = observed({ reliable: false })
    let n = 0
    net.c2s.filter = (f) => !(isData(f) && ++n === 2)

    const stream = net.conn.newStream(echo.count, {})
    for (const t of ['a', 'b', 'c', 'd']) await stream.send({ text: t })
    stream.closeSend()
    expect(await stream.recv()).toEqual({ text: '3' }) // one fewer arrived

    expect(s.counters.snapshot().skipped).toBe(1)
    const evs = s.log.of('skipped')
    expect(evs).toHaveLength(1)
    const ev = evs[0]!
    expect(ev.count).toBe(1)
    expect(ev.peer).toBe(net.peer) // every server event names its peer
    expect(ev.sid).toBe(1)
    expect(ev.method).toBe(echo.count.path)
  })

  it('a second call reports under its own sid and method; the lossless call and the other end report nothing', async () => {
    const { net, c, s } = observed({ reliable: false })
    expect(await net.conn.invoke(echo.once, { text: 'x' })).toEqual({ text: 'echo:x' }) // sid 1, lossless
    let n = 0
    net.s2c.filter = (f) => !(isData(f) && ++n === 2)
    const stream = net.conn.newStream(echo.many, {}) // sid 2
    await stream.send({ text: 'm', n: 3 })
    const got: string[] = []
    for await (const res of stream) got.push(res.text)
    expect(got).toEqual(['m#0', 'm#2'])

    expect(c.log.evs).toEqual([{ kind: 'skipped', sid: 2, method: echo.many.path, count: 1 }]) // nothing under sid 1
    expect(s.log.evs).toEqual([]) // the server saw a lossless c2s
  })
})

// ---------------------------------------------------------------------------
// §6.3: a window overrun is the one loss that is NOT silent
// ---------------------------------------------------------------------------

describe('data-loss (§6.3)', () => {
  it('names the call that failed DATA_LOSS, and is not a gap', async () => {
    const counters = new Counters()
    const log = new EventLog()
    const sent: Frame[] = []
    const conn = new Conn({ handle: (f: Frame) => void sent.push(wireClone(f)) }, { reliable: false, protocolStats: [counters.observe, log.observe] })
    const stream = conn.newStream(echo.many, {})
    await stream.send({ text: 'x', n: 0 })
    const open = sent.find((f) => isOpen(f))!

    const mk = (seq: number): Frame => frame({ epoch: 7, sid: open.sid, seq, peerEpoch: open.epoch, payload: enc({ text: `m${seq}` }) })
    await conn.handle(mk(1), {}) // accepted; L = 1
    expect(await stream.recv()).toEqual({ text: 'm1' })
    for (const seq of [6000, 6001, 6002]) await conn.handle(mk(seq), {}) // K_loud consistent beyond-window frames
    const err = (await stream.recv().catch((e) => e)) as StatusError
    expect(err.code).toBe(Code.DATA_LOSS)

    expect(counters.snapshot().dataLoss).toBe(1)
    expect(counters.snapshot().skipped).toBe(0) // an overrun is a loud failure, not a gap
    const ev = log.first('data-loss')
    expect(ev.sid).toBe(open.sid)
    expect(ev.method).toBe(echo.many.path)
    expect(ev.peer).toBeUndefined()
    conn.close()
  })

  it('at the server: names the peer, the call and its method — once, and not as a gap', async () => {
    const { net, s } = observed({ reliable: false })
    const stream = net.conn.newStream(echo.live, {})
    await stream.send({ text: 'a' })
    expect(await stream.recv()).toEqual({ text: 'echo:a' }) // the server's window is past the OPEN

    const mk = (seq: number) => frame({ epoch: net.conn.epoch, sid: 1, seq, peerEpoch: net.server.epoch, payload: enc({ text: 'lost' }) })
    for (const seq of [6000, 6001, 6002]) await net.server.handle(mk(seq), { peer: net.peer }) // K_LOUD consistent beyond-window frames

    expect(s.counters.snapshot()).toMatchObject({ dataLoss: 1, skipped: 0 })
    expect(s.log.of('data-loss')).toHaveLength(1)
    expect(s.log.first('data-loss')).toMatchObject({ peer: net.peer, sid: 1, method: echo.live.path, count: 0 })
    // The handler's unwind produces the terminal the client sees.
    expect(((await stream.recv().catch((e) => e)) as StatusError).code).toBe(Code.DATA_LOSS)
  })
})

// ---------------------------------------------------------------------------
// §4.2: rx drops under the drop policy, and §8 off-shape frames
// ---------------------------------------------------------------------------

describe('dropped (§4.2)', () => {
  it('reports each frame the drop policy discarded, on the call it belonged to', async () => {
    const { net, c } = observed({ reliable: false, connRx: { size: 2, policy: DropPolicy.Newest } })
    const stream = net.conn.newStream(echo.many, {})
    // Five responses land before anything is read: two buffered, three dropped.
    await stream.send({ text: 'm', n: 5 })
    for (let i = 0; i < 100 && net.sentS2C.filter(isData).length < 5; i++) await tick()
    const got: string[] = []
    for await (const res of stream) got.push(res.text)
    expect(got).toEqual(['m#0', 'm#1'])

    expect(c.counters.snapshot().dropped).toBe(3)
    const evs = c.log.of('dropped')
    expect(evs).toHaveLength(3)
    for (const ev of evs) {
      expect(ev.count).toBe(1)
      expect(ev.sid).toBe(1)
      expect(ev.method).toBe(echo.many.path)
      expect(ev.peer).toBeUndefined()
    }
    // Drops are not gaps: the seq window saw every frame.
    expect(c.counters.snapshot().skipped).toBe(0)
  })

  it('DropPolicy.Oldest reports the evicted frame, once per arrival, on the call it belonged to', async () => {
    const { net, c } = observed({ reliable: false, connRx: { size: 2, policy: DropPolicy.Oldest } })
    const stream = net.conn.newStream(echo.many, {})
    await stream.send({ text: 'm', n: 5 })
    for (let i = 0; i < 100 && net.sentS2C.filter(isData).length < 5; i++) await tick()
    const got: string[] = []
    for await (const res of stream) got.push(res.text)
    expect(got).toEqual(['m#3', 'm#4']) // Oldest keeps the buffered suffix

    expect(c.counters.snapshot()).toMatchObject({ dropped: 3, skipped: 0 })
    expect(c.log.of('dropped').map((e) => [e.sid, e.method, e.count])).toEqual([
      [1, echo.many.path, 1],
      [1, echo.many.path, 1],
      [1, echo.many.path, 1],
    ])
  })
})

describe('off-shape (§8, §11)', () => {
  it('a server DATA frame on a call whose shape has none', async () => {
    const counters = new Counters()
    const log = new EventLog()
    const sent: Frame[] = []
    const conn = new Conn({ handle: (f: Frame) => void sent.push(wireClone(f)) }, { reliable: false, protocolStats: [counters.observe, log.observe] })
    const stream = conn.newStream(echo.count, {}) // client-streaming: no server data
    await stream.send({ text: 'a' })
    const open = sent.find((f) => isOpen(f))!

    await conn.handle(frame({ epoch: 7, sid: open.sid, seq: 1, peerEpoch: open.epoch, payload: enc({ text: 'stray' }) }), {})
    expect(counters.snapshot().offShape).toBe(1)
    const ev = log.first('off-shape')
    expect(ev.count).toBe(1)
    expect(ev.sid).toBe(open.sid)
    expect(ev.method).toBe(echo.count.path)
    expect(ev.peer).toBeUndefined()
    conn.close()
  })

  it('a trailer the handler set that failed validation and was dropped', async () => {
    const { net, s } = observed({ reliable: true })
    net.server.register(echo.once, (_req, ctx) => {
      ctx.setTrailer({ 'bad name': ['v'] }) // dropped, as grpc-go does (§11)
      return { text: 'ok' }
    })
    let trailer: unknown
    expect(await net.conn.invoke(echo.once, { text: 'x' }, { onTrailer: (t) => (trailer = t) })).toEqual({ text: 'ok' })
    expect(trailer ?? {}).toEqual({}) // dropped, not merely reported

    expect(s.counters.snapshot().offShape).toBe(1)
    const ev = s.log.first('off-shape')
    expect(ev.count).toBe(1)
    expect(ev.peer).toBe(net.peer)
    expect(ev.sid).toBe(1)
    expect(ev.method).toBe(echo.once.path)
  })

  it('is NOT reported for a client DATA frame on a server call whose shape has none (Go parity)', async () => {
    // docs/observability.md Limits: the server drops these into its
    // per-stream counter without an event, and so does Go.
    const { net, s } = observed({ reliable: false })
    const gate = new Latch()
    net.server.register(echo.many, async () => {
      await gate.wait()
    })
    const stream = net.conn.newStream(echo.many, {}) // server-streaming: no client data
    await stream.send({ text: 'x', n: 0 })
    await tick()
    await net.server.handle(frame({ epoch: net.conn.epoch, sid: 1, seq: 2, peerEpoch: net.server.epoch, payload: enc({ text: 'stray' }) }), { peer: net.peer })
    expect(s.log.of('off-shape')).toEqual([])
    gate.trip()
    for await (const _ of stream) {
      // drain
    }
  })
})

// ---------------------------------------------------------------------------
// §4.2.1: flow-control stalls, in reliable mode
// ---------------------------------------------------------------------------

describe('flow-stall / flow-resume (§4.2.1)', () => {
  it('a producer parked on credit is one stall, and one resume when the consumer grants', async () => {
    const { net, s, c } = observed({ reliable: true })
    const stream = net.conn.newStream(echo.many, {})
    // One past the client's 32-message window, read only after the producer
    // has parked on message 33: exactly the window reaches the wire.
    await stream.send({ text: 'm', n: 33 })
    for (let i = 0; i < 100 && net.sentS2C.filter(isData).length < 32; i++) await tick()
    expect(net.sentS2C.filter(isData)).toHaveLength(32)
    expect(s.counters.snapshot().flowStall).toBe(1)
    expect(s.counters.snapshot().flowResume).toBe(0)

    const got: string[] = []
    for await (const res of stream) got.push(res.text)
    expect(got).toHaveLength(33)

    expect(s.counters.snapshot()).toMatchObject({ flowStall: 1, flowResume: 1 })
    const ev = s.log.first('flow-stall')
    expect(ev.peer).toBe(net.peer)
    expect(ev.sid).toBe(1)
    expect(ev.method).toBe(echo.many.path)
    // The consumer side parked nothing.
    expect(c.counters.snapshot()).toMatchObject({ flowStall: 0, flowResume: 0 })
  })

  it('a client producer parked on credit: one stall while parked, one resume when the handler drains', async () => {
    const { net, c, s } = observed({ reliable: true })
    const gate = new Latch()
    net.server.register(echo.count, async (stream) => {
      await gate.wait()
      let n = 0
      for await (const _ of stream) n++
      return { text: String(n) }
    })
    const stream = net.conn.newStream(echo.count, {})
    const producer = (async () => {
      for (let i = 0; i < 40; i++) await stream.send({ text: 'm' })
      stream.closeSend()
    })()
    for (let i = 0; i < 200 && c.counters.snapshot().flowStall === 0; i++) await tick()
    // Parked right now, and the stall is already visible.
    expect(c.counters.snapshot()).toMatchObject({ flowStall: 1, flowResume: 0 })
    expect(c.log.first('flow-stall')).toMatchObject({ sid: 1, method: echo.count.path, count: 0 })
    expect(c.log.first('flow-stall').peer).toBeUndefined()
    expect(net.sentC2S.filter(isData)).toHaveLength(32) // exactly the peer's window reached the wire

    gate.trip()
    await producer
    expect(await stream.recv()).toEqual({ text: '40' })
    expect(c.counters.snapshot()).toMatchObject({ flowStall: 1, flowResume: 1 })
    expect(s.counters.snapshot()).toMatchObject({ flowStall: 0, flowResume: 0 }) // the consumer parked nothing
  })

  it('a park ended by the call ending is not a resume: the caller cancels', async () => {
    // Go's acquire selects on done and returns errCallEnded; only a grant is
    // a resume. A sender unparked by its own cancellation resumed nothing.
    const { net, c } = observed({ reliable: true })
    const gate = new Latch()
    net.server.register(echo.count, async (stream) => {
      await gate.wait()
      let n = 0
      for await (const _ of stream) n++
      return { text: String(n) }
    })
    const stream = net.conn.newStream(echo.count, {})
    const producer = (async () => {
      for (let i = 0; i < 40; i++) await stream.send({ text: 'm' })
    })().catch((e: unknown) => e)
    for (let i = 0; i < 200 && c.counters.snapshot().flowStall === 0; i++) await tick()
    stream.cancel()
    await producer
    expect(c.counters.snapshot()).toMatchObject({ flowStall: 1, flowResume: 0 })
    gate.trip()
  })

  it('a park ended by the call ending is not a resume: the server finishes', async () => {
    const { net, c } = observed({ reliable: true })
    const gate = new Latch()
    net.server.register(echo.count, async () => {
      await gate.wait()
      return { text: 'early' } // without reading: the terminal ends the client's park
    })
    const stream = net.conn.newStream(echo.count, {})
    const producer = (async () => {
      for (let i = 0; i < 40; i++) await stream.send({ text: 'm' })
    })().catch((e: unknown) => e)
    for (let i = 0; i < 200 && c.counters.snapshot().flowStall === 0; i++) await tick()
    gate.trip()
    await producer
    expect(await stream.recv()).toEqual({ text: 'early' })
    expect(c.counters.snapshot()).toMatchObject({ flowStall: 1, flowResume: 0 })
  })
})

// ---------------------------------------------------------------------------
// §9.3: RESETs, counted on both ends, immediately on a reliable channel
// ---------------------------------------------------------------------------

describe('reset-sent / reset-received, immediate (§9.3)', () => {
  it('at the client: sent for a frame it cannot own, received and acted on for its live call', async () => {
    const counters = new Counters()
    const log = new EventLog()
    const sent: Frame[] = []
    const conn = new Conn({ handle: (f: Frame) => void sent.push(wireClone(f)) }, { reliable: true, protocolStats: [counters.observe, log.observe] })
    const stream = conn.newStream(echo.many, {})
    await stream.send({ text: 'x', n: 0 })
    const open = sent.find((f) => isOpen(f))!

    // An unknown sid echoing our epoch: RESET.
    await conn.handle(frame({ epoch: 7, sid: open.sid + 7, seq: 1, peerEpoch: open.epoch, payload: enc({}) }), {})
    expect(counters.snapshot()).toMatchObject({ resetSent: 1, resetReceived: 0 })
    expect(log.first('reset-sent')).toMatchObject({ sid: open.sid + 7, method: '', count: 0 })
    expect(log.first('reset-sent').peer).toBeUndefined()
    expect(sent.filter(isReset).map((f) => f.sid)).toEqual([open.sid + 7])
    // A foreign peer_epoch on the live sid: RESET, and the call is untouched.
    await conn.handle(frame({ epoch: 7, sid: open.sid, seq: 1, peerEpoch: open.epoch ^ 1, payload: enc({}) }), {})
    expect(counters.snapshot().resetSent).toBe(2)
    expect(log.of('reset-sent')[1]!.sid).toBe(open.sid)
    // Another incarnation's keepalive draws nothing.
    await conn.handle(frame({ epoch: 7, flags: FlagPing, peerEpoch: open.epoch ^ 1 }), {})
    expect(counters.snapshot().resetSent).toBe(2)

    // The mirror, on the live call: counted AND acted on.
    await conn.handle(frame({ flags: FlagReset, epoch: open.epoch, sid: open.sid, peerEpoch: open.epoch }), {})
    expect(counters.snapshot().resetReceived).toBe(1)
    expect(log.first('reset-received')).toMatchObject({ sid: open.sid, method: '', count: 0 })
    expect(((await stream.recv().catch((e) => e)) as StatusError).code).toBe(Code.UNAVAILABLE)
    conn.close()
  })

  it('at the server: sent at once on a reliable channel, naming the peer; the far Conn ignores a RESET echoing a foreign epoch', async () => {
    const { net, c, s } = observed({ reliable: true })
    // A frame for a sid this server never opened: no reordering on a reliable
    // channel, so it is RESET now, not held for T_hold.
    await net.server.handle(frame({ epoch: 0xc0ffee, sid: 42, seq: 2, payload: enc({ text: '?' }) }), { peer: net.peer })
    expect(s.counters.snapshot()).toMatchObject({ resetSent: 1, resetReceived: 0 })
    expect(s.log.first('reset-sent')).toMatchObject({ peer: net.peer, sid: 42, method: '', count: 0 })
    expect(net.sentS2C.filter(isReset).map((f) => [f.epoch, f.sid])).toEqual([[0xc0ffee, 42]])
    // The RESET echoes 0xc0ffee, not the Conn's epoch: dropped there, no event.
    expect(c.log.evs).toEqual([])
  })

  it('at the server: received names the peer and sid, kills the call, and ignores a foreign-epoch RESET', async () => {
    const { net, s } = observed({ reliable: true })
    let seen: StatusError | undefined
    net.server.register(echo.live, async (stream) => {
      try {
        for await (const _ of stream) {
          // consume; the RESET lands while waiting for the next
        }
      } catch (e) {
        seen = e as StatusError
      }
    })
    const stream = net.conn.newStream(echo.live, {})
    await stream.send({ text: 'a' })
    await tick()
    await net.server.handle(frame({ flags: FlagReset, epoch: net.server.epoch, sid: 1, peerEpoch: net.conn.epoch }), { peer: net.peer })
    await tick()
    expect(s.counters.snapshot().resetReceived).toBe(1)
    expect(s.log.first('reset-received')).toMatchObject({ peer: net.peer, sid: 1, method: '', count: 0 })
    expect(seen?.code).toBe(Code.UNAVAILABLE) // acted on, not merely counted
    expect(net.sentS2C.filter(isTerminal)).toEqual([]) // the peer disowned the call: no T
    // Echoes another server incarnation: not ours.
    await net.server.handle(frame({ flags: FlagReset, epoch: net.server.epoch ^ 1, sid: 1, peerEpoch: net.conn.epoch }), { peer: net.peer })
    expect(s.counters.snapshot().resetReceived).toBe(1)
    stream.cancel()
  })
})

// ---------------------------------------------------------------------------
// §9.2, §9.3, §10: the sweep's events — RESETs, replays, probes, keepalives,
// retransmissions, liveness — under fake timers
// ---------------------------------------------------------------------------

// probe = 150ms, T_call = 300ms, T_live = 450ms, RTI = 50ms, T_hold = 50ms.
const fast: Timing = { callMs: 300, livenessMs: 450, retransmitMs: 50, tombstoneMs: 1000, holdMs: 50 }

describe('the sweep (§9, §10)', () => {
  beforeEach(() => {
    vi.useFakeTimers()
  })
  afterEach(() => {
    vi.useRealTimers()
  })

  it('reset-sent at the server that disowns a call, reset-received at the client it answers', async () => {
    const { net, c, s } = observed({ reliable: false, timing: fast })
    const p = net.conn.invoke(echo.once, { text: 'x' })
    await vi.advanceTimersByTimeAsync(10)
    await p

    // A frame for a call the server never saw: held for T_hold in case its
    // OPEN is merely late, then RESET (§9.3).
    await net.server.handle(frame({ epoch: net.conn.epoch, sid: 42, seq: 1, payload: enc({ text: '?' }) }), { peer: net.peer })
    await vi.advanceTimersByTimeAsync(200)

    expect(s.counters.snapshot().resetSent).toBe(1)
    const sent = s.log.first('reset-sent')
    expect(sent.peer).toBe(net.peer) // what makes a RESET storm attributable (§15)
    expect(sent.sid).toBe(42)
    expect(sent.method).toBe('') // peer scope: no call to name

    expect(c.counters.snapshot().resetReceived).toBe(1)
    const recv = c.log.first('reset-received')
    expect(recv.sid).toBe(42)
    expect(recv.peer).toBeUndefined()
  })

  it('retransmit at the client whose OPEN was not answered, tombstone-replay at the server that answers the duplicate', async () => {
    const { net, c, s } = observed({ reliable: false, timing: fast })
    let droppedTerm = false
    net.s2c.filter = (f) => {
      if (!droppedTerm && isTerminal(f)) {
        droppedTerm = true
        return false
      }
      return true
    }
    const p = net.conn.invoke(echo.once, { text: 'x' })
    await vi.advanceTimersByTimeAsync(250)
    expect(await p).toEqual({ text: 'echo:x' })
    expect(net.counts.once).toBe(1)

    expect(c.counters.snapshot().retransmit).toBeGreaterThanOrEqual(1)
    const rt = c.log.first('retransmit')
    expect(rt.sid).toBe(1)
    expect(rt.method).toBe(echo.once.path)

    expect(s.counters.snapshot().tombstoneReplay).toBeGreaterThanOrEqual(1)
    const tr = s.log.first('tombstone-replay')
    expect(tr.peer).toBe(net.peer)
    expect(tr.sid).toBe(1)
  })

  it('tombstone-replay: one per duplicate outside 1/RTI, none inside it, and a probe draws it too', async () => {
    const { net, c, s } = observed({ reliable: false, timing: fast })
    const p = net.conn.invoke(echo.once, { text: 'x' })
    await vi.advanceTimersByTimeAsync(10)
    await p
    const open = net.sentC2S.find((f) => isOpen(f))!

    await net.server.handle(wireClone(open), { peer: net.peer }) // the replay clock starts at 0: due at once
    expect(s.counters.snapshot().tombstoneReplay).toBe(1)
    expect(s.log.first('tombstone-replay')).toMatchObject({ peer: net.peer, sid: 1, method: '', count: 0 })
    await net.server.handle(wireClone(open), { peer: net.peer }) // inside RTI: rate-limited, no event
    expect(s.counters.snapshot().tombstoneReplay).toBe(1)
    await vi.advanceTimersByTimeAsync(2 * fast.retransmitMs!)
    await net.server.handle(frame({ epoch: net.conn.epoch, sid: 1, flags: FlagPing, peerEpoch: net.server.epoch }), { peer: net.peer }) // the probe path
    expect(s.counters.snapshot().tombstoneReplay).toBe(2)
    expect(net.counts.once).toBe(1) // replayed, never re-executed
    expect(c.log.of('reset-sent')).toEqual([]) // a replayed T at a finished call is a straggler, not a RESET
  })

  it('a tombstoned abort retransmits under its sid with no method; a live OPEN retransmits with one', async () => {
    const { net, c } = observed({ reliable: false, timing: fast })
    net.c2s.filter = () => false // nothing reaches the server
    const stream = net.conn.newStream(echo.live, {})
    await stream.send({ text: 'a' })
    await vi.advanceTimersByTimeAsync(3 * fast.retransmitMs!)
    const live = c.log.of('retransmit')
    expect(live.length).toBeGreaterThan(0)
    expect(live.every((e) => e.sid === 1 && e.method === echo.live.path)).toBe(true)

    stream.cancel() // the abort parks under the tombstone
    await vi.advanceTimersByTimeAsync(3 * fast.retransmitMs!)
    const tomb = c.log.of('retransmit').slice(live.length)
    expect(tomb.length).toBeGreaterThan(0)
    expect(tomb.every((e) => e.sid === 1 && e.method === '' && e.count === 0)).toBe(true)
  })

  it('keepalive-sent and probe-sent while an idle stream is kept alive', async () => {
    const { net, c, s } = observed({ reliable: false, timing: fast })
    net.server.register(echo.live, async (stream) => {
      await stream.recv().catch(() => undefined)
    })
    const stream = net.conn.newStream(echo.live, {})
    await tick()
    await vi.advanceTimersByTimeAsync(1500) // >> T_live, with PINGs flowing

    for (const side of [c, s]) {
      expect(side.counters.snapshot().keepaliveSent).toBeGreaterThanOrEqual(1)
      expect(side.counters.snapshot().probeSent).toBeGreaterThanOrEqual(1)
      expect(side.counters.snapshot().livenessExpired).toBe(0)
    }
    // Keepalives are peer scope; probes name the stream they probe.
    expect(c.log.first('keepalive-sent')).toMatchObject({ sid: 0, method: '' })
    expect(s.log.first('keepalive-sent')).toMatchObject({ peer: net.peer, sid: 0, method: '' })
    expect(c.log.first('probe-sent')).toMatchObject({ sid: 1, method: echo.live.path })
    expect(s.log.first('probe-sent')).toMatchObject({ peer: net.peer, sid: 1, method: echo.live.path })
    stream.cancel()
  })

  it('liveness-expired at the end that stopped hearing its peer', async () => {
    const { net, c, s } = observed({ reliable: false, timing: fast })
    net.server.register(echo.live, async (stream) => {
      await stream.recv().catch(() => undefined)
    })
    const stream = net.conn.newStream(echo.live, {})
    await tick()
    net.s2c.filter = () => false // the server vanishes, as the client sees it
    net.c2s.filter = () => false // and the client, as the server sees it
    const p = stream.recv().catch((e) => e)
    await vi.advanceTimersByTimeAsync(600)
    expect(((await p) as StatusError).code).toBe(Code.UNAVAILABLE)

    expect(c.counters.snapshot().livenessExpired).toBe(1)
    expect(c.log.first('liveness-expired')).toMatchObject({ sid: 0, method: '' })
    expect(s.counters.snapshot().livenessExpired).toBe(1)
    expect(s.log.first('liveness-expired')).toMatchObject({ peer: net.peer, sid: 0, method: '' })
  })
})

// ---------------------------------------------------------------------------
// the surface itself
// ---------------------------------------------------------------------------

describe('Counters', () => {
  it('adds the magnitude for skipped / dropped / off-shape, and one for everything else', () => {
    const c = new Counters()
    const ev = (kind: ProtocolEventKind, count: number): ProtocolEvent => ({ kind, sid: 1, method: '/m', count })
    c.observe(ev('skipped', 5))
    c.observe(ev('skipped', 0)) // a magnitude of 0 still counts as one event
    c.observe(ev('dropped', 1))
    c.observe(ev('off-shape', 2))
    c.observe(ev('reset-sent', 9)) // no magnitude: one
    c.observe(ev('flow-stall', 0))
    expect(c.snapshot()).toMatchObject({ skipped: 6, dropped: 1, offShape: 2, resetSent: 1, flowStall: 1, flowResume: 0 })
    // A snapshot is a copy.
    const snap = c.snapshot()
    c.observe(ev('skipped', 1))
    expect(snap.skipped).toBe(6)
  })

  it('observe is unbound: it can be handed over as a bare function', () => {
    const c = new Counters()
    const observe = c.observe
    observe({ kind: 'retransmit', sid: 0, method: '', count: 0 })
    expect(c.snapshot().retransmit).toBe(1)
  })
})

describe('a throwing observer is inert', () => {
  const boom = (kind: ProtocolEventKind): ProtocolStats => (ev) => {
    if (ev.kind === kind) throw new Error(`boom:${kind}`)
  }

  it('on the receive path: the frame is still delivered and the gap still counted', async () => {
    const counters = new Counters()
    const sent: Frame[] = []
    const conn = new Conn({ handle: (f: Frame) => void sent.push(wireClone(f)) }, { reliable: false, protocolStats: [boom('skipped'), counters.observe] })
    const stream = conn.newStream(echo.many, {})
    await stream.send({ text: 'x', n: 0 })
    const open = sent.find((f) => isOpen(f))!
    const mk = (seq: number) => frame({ epoch: 7, sid: open.sid, seq, peerEpoch: open.epoch, payload: enc({ text: `m${seq}` }) })
    await conn.handle(mk(1), {})
    await conn.handle(mk(3), {}) // a gap of one; the first observer throws on it
    expect(await stream.recv()).toEqual({ text: 'm1' })
    expect(await stream.recv()).toEqual({ text: 'm3' })
    expect(counters.snapshot().skipped).toBe(1) // the second observer still saw it
    conn.close()
  })

  it('in the sweep: liveness still expires and fails the call', async () => {
    vi.useFakeTimers()
    try {
      const counters = new Counters()
      const net = makeNet({ reliable: false, connOpts: { timing: fast, protocolStats: [boom('liveness-expired'), counters.observe] }, serverOpts: { timing: fast } })
      const stream = net.conn.newStream(echo.live, {})
      await tick()
      net.s2c.filter = () => false
      const p = stream.recv().catch((e) => e)
      await vi.advanceTimersByTimeAsync(600)
      expect(((await p) as StatusError).code).toBe(Code.UNAVAILABLE)
      expect(counters.snapshot().livenessExpired).toBe(1)
    } finally {
      vi.useRealTimers()
    }
  })

  it('on the send path: a parked producer still resumes and the call completes', async () => {
    const counters = new Counters()
    const net = makeNet({ reliable: true, serverOpts: { protocolStats: [boom('flow-stall'), counters.observe] } })
    const stream = net.conn.newStream(echo.many, {})
    await stream.send({ text: 'm', n: 33 })
    for (let i = 0; i < 100 && net.sentS2C.filter(isData).length < 32; i++) await tick() // parked on message 33
    expect(counters.snapshot().flowStall).toBe(1)
    const got: string[] = []
    for await (const res of stream) got.push(res.text)
    expect(got).toHaveLength(33)
    expect(counters.snapshot()).toMatchObject({ flowStall: 1, flowResume: 1 })
  })
})

describe('a reliable channel is quiet', () => {
  it('a healthy exchange emits nothing at all', async () => {
    const { net, c, s } = observed({ reliable: true })
    expect(await net.conn.invoke(echo.once, { text: 'x' })).toEqual({ text: 'echo:x' })
    const stream = net.conn.newStream(echo.live, {})
    await stream.send({ text: 'a' })
    expect(await stream.recv()).toEqual({ text: 'echo:a' })
    stream.closeSend()
    while ((await stream.recv()) !== undefined) {
      // drain
    }
    expect(c.log.evs).toEqual([])
    expect(s.log.evs).toEqual([])
  })
})
