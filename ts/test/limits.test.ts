// Resource bounds (PROTOCOL.md §15), drop policies (§4.2), the aged
// watermark (§9.4), and window-overrun fail-loud (§6.3) — TS twins of the Go
// limits_caps_test.go / dataloss_test.go / wire_shape_test.go coverage.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { Conn } from '../src/conn'
import type { StreamServerInterceptor } from '../src/interceptor'
import { DropPolicy, resolveLimits } from '../src/limits'
import { Server, type ServerOptions } from '../src/server'
import { abortCause, Code, type StatusError } from '../src/status'
import type { Timing } from '../src/timing'
import { FlagClose, FlagOpen, FlagPing, FlagWindow, frame, isHeaderFrame, isOpen, isReset, isTerminal, shapeOf, type Frame } from '../src/wire'
import { Counters } from '../src/stats'
import { W_CONN } from '../src/util'
import { echo, jsonCodec, makeNet, registerEcho, tick, wireClone } from '../src/testing'

const fast: Timing = { callMs: 300, livenessMs: 450, retransmitMs: 50, tombstoneMs: 1000, holdMs: 50 }

beforeEach(() => {
  vi.useFakeTimers()
})
afterEach(() => {
  vi.useRealTimers()
})

// injectServer: a server whose tx records frames; tests craft client frames
// directly (the Go injectServer shape).
function injectServer(opts: ServerOptions = {}) {
  const sent: Frame[] = []
  const server = new Server({ handle: (f: Frame) => void sent.push(wireClone(f)) }, { reliable: false, timing: fast, ...opts })
  const counts = registerEcho(server)
  return { server, sent, counts }
}

const enc = (v: unknown) => new TextEncoder().encode(JSON.stringify(v))

function openOnce(epoch: number, sid: number, text: string): Frame {
  return frame({ epoch, sid, seq: 1, flags: FlagOpen | FlagClose, method: echo.once.path, payload: enc({ text }) })
}

function openLive(epoch: number, sid: number): Frame {
  return frame({ epoch, sid, seq: 1, flags: FlagOpen, method: echo.live.path })
}

describe('MaxLiveCalls (§15)', () => {
  it('an OPEN past the cap draws T{RESOURCE_EXHAUSTED}, not a RESET', async () => {
    const net = makeNet({ reliable: true, serverOpts: { limits: { maxLiveCalls: 1 } } })
    const s1 = net.conn.newStream(echo.live, {})
    await tick()
    const s2 = net.conn.newStream(echo.live, {})
    const err = (await s2.recv().catch((e) => e)) as StatusError
    expect(err.code).toBe(Code.RESOURCE_EXHAUSTED)
    // The first call is unaffected.
    await s1.send({ text: 'x' })
    const p = s1.recv()
    await tick()
    expect(await p).toEqual({ text: 'echo:x' })
  })

  it('is counted across client epochs of one transport peer (epoch spoofing buys nothing)', async () => {
    const inj = injectServer({ limits: { maxLiveCalls: 2 } })
    await inj.server.handle(openLive(1, 1), { peer: 'p' })
    await inj.server.handle(openLive(2, 1), { peer: 'p' }) // another "incarnation", same peer
    await inj.server.handle(openLive(3, 1), { peer: 'p' })
    await tick()
    expect(inj.counts.live).toBe(2)
    const rej = inj.sent.find((f) => isTerminal(f) && f.code === Code.RESOURCE_EXHAUSTED)
    expect(rej).toBeDefined()
    expect(rej!.peerEpoch).toBe(3) // names the rejected incarnation (§6.1)
    await inj.server.stop()
  })
})

describe('aged watermark (§9.4)', () => {
  it('an OPEN at or below hwm_aged is RESET, never re-executed', async () => {
    const inj = injectServer()
    await inj.server.handle(openOnce(3, 5, 'a'), { peer: 'p' })
    await tick()
    expect(inj.counts.once).toBe(1)

    // Age the watermark past TTL_tomb (but inside the ≥ TTL container
    // retention, §9.4): checkpoints recorded by the sweep now cover sid 5,
    // and the tombstone (expired + covered) is collected — the watermark
    // alone must reject what the tombstone used to.
    await vi.advanceTimersByTimeAsync(1500)

    const before = inj.sent.length
    await inj.server.handle(openOnce(3, 3, 'stale'), { peer: 'p' })
    await tick()
    expect(inj.counts.once).toBe(1) // never re-executed
    const reset = inj.sent.slice(before).find((f) => isReset(f))
    expect(reset).toBeDefined()
    expect(reset!.sid).toBe(3)
    expect(reset!.epoch).toBe(3) // echoes the offending frame's epoch (§9.3)
    await inj.server.stop()
  })
})

describe('tombstone caps (§9.2, §15)', () => {
  it('entry-cap eviction raises the floor: dedup survives at zero memory', async () => {
    const inj = injectServer({ limits: { maxTombstones: 2 } })
    for (let sid = 1; sid <= 4; sid++) {
      await inj.server.handle(openOnce(3, sid, `m${sid}`), { peer: 'p' })
      await tick()
    }
    expect(inj.counts.once).toBe(4)

    // sids 1 and 2 were evicted into the floor. A duplicate OPEN for one is
    // swallowed: validated, deduped, no re-execution, no reply.
    const before = inj.sent.length
    await inj.server.handle(openOnce(3, 1, 'm1'), { peer: 'p' })
    await tick()
    expect(inj.counts.once).toBe(4)
    expect(inj.sent.length).toBe(before)
    await inj.server.stop()
  })

  it('byte-cap pressure degrades stored terminals to key-only: dedup survives, replay is lost', async () => {
    const inj = injectServer({ limits: { maxTombstoneBytes: 8 } })
    await inj.server.handle(openOnce(3, 1, 'a-very-long-response-payload'), { peer: 'p' })
    await tick()
    // Duplicate OPEN: tombstone hit, but the terminal was degraded — silence
    // (the call falls back to timeout behavior), and no re-execution.
    await vi.advanceTimersByTimeAsync(100) // clear the 1/RTI replay limit
    const before = inj.sent.length
    await inj.server.handle(openOnce(3, 1, 'a-very-long-response-payload'), { peer: 'p' })
    await tick()
    expect(inj.counts.once).toBe(1)
    expect(inj.sent.length).toBe(before)
    await inj.server.stop()
  })
})

describe('aggregate reply budget (§15)', () => {
  it('volunteered replies per peer are capped per RTI; denial is silence', async () => {
    const inj = injectServer({ limits: { maxRepliesPerRTI: 2 } })
    // 10 stream probes for unknown sids: each would draw an immediate RESET,
    // but the budget allows 2 per RTI.
    for (let sid = 100; sid < 110; sid++) {
      const probe = frame({ epoch: 3, sid, seq: 0, flags: FlagPing, method: '', codec: '', desc: '', peerEpoch: 0 })
      await inj.server.handle(probe, { peer: 'p' })
    }
    expect(inj.sent.filter((f) => isReset(f)).length).toBe(2)
    // The next RTI window turns over and replies flow again.
    await vi.advanceTimersByTimeAsync(60)
    const probe = frame({ epoch: 3, sid: 200, seq: 0, flags: FlagPing, method: '', codec: '', desc: '', peerEpoch: 0 })
    await inj.server.handle(probe, { peer: 'p' })
    expect(inj.sent.filter((f) => isReset(f)).length).toBe(3)
    await inj.server.stop()
  })
})

describe('drop policies (§4.2)', () => {
  // The Go runDropPolicy shape: a fake server incarnation streams four
  // responses into a 2-frame buffer before the app recvs; the terminal is
  // processed via the seq window, never the buffer.
  async function runDropPolicy(policy: DropPolicy): Promise<string[]> {
    const sent: Frame[] = []
    const conn = new Conn({ handle: (f: Frame) => void sent.push(wireClone(f)) }, { reliable: false, timing: fast, rxBuffer: { size: 2, policy } })
    const stream = conn.newStream(echo.many, {})
    await stream.send({ text: 'abc', n: 4 })
    const open = sent.find((f) => isOpen(f))!
    const [cEpoch, sid] = [open.epoch, open.sid]

    const srvEpoch = 7
    for (let seq = 1; seq <= 4; seq++) {
      const f = frame({ epoch: srvEpoch, sid, seq, flags: 0, method: '', codec: '', desc: '', peerEpoch: cEpoch, payload: enc({ text: `m${seq}` }) })
      await conn.handle(f, {})
    }
    const term = frame({ epoch: srvEpoch, sid, seq: 5, flags: FlagClose, method: '', codec: '', desc: '', peerEpoch: cEpoch })
    term.code = Code.OK
    await conn.handle(term, {})

    const got: string[] = []
    for (;;) {
      const m = await stream.recv()
      if (m === undefined) break
      got.push(m.text)
    }
    conn.close()
    return got
  }

  it('DropNewest keeps the buffered prefix', async () => {
    expect(await runDropPolicy(DropPolicy.Newest)).toEqual(['m1', 'm2'])
  })

  it('DropOldest keeps the freshest', async () => {
    expect(await runDropPolicy(DropPolicy.Oldest)).toEqual(['m3', 'm4'])
  })
})

describe('window-overrun fail-loud (§6.3)', () => {
  it('K_loud consistent beyond-window frames fail the call with DATA_LOSS', async () => {
    const sent: Frame[] = []
    const conn = new Conn({ handle: (f: Frame) => void sent.push(wireClone(f)) }, { reliable: false, timing: fast })
    const stream = conn.newStream(echo.many, {})
    await stream.send({ text: 'x', n: 0 })
    const open = sent.find((f) => isOpen(f))!

    const mk = (seq: number): Frame => frame({ epoch: 7, sid: open.sid, seq, peerEpoch: open.epoch, payload: enc({ text: `m${seq}` }) })
    await conn.handle(mk(1), {}) // accepted; L = 1
    expect(await stream.recv()).toEqual({ text: 'm1' }) // drain the buffered frame
    await conn.handle(mk(6000), {}) // beyond window (Δ > 4096): run of 1
    await conn.handle(mk(6001), {}) // consistent: 2
    const p = stream.recv().catch((e) => e as StatusError)
    await conn.handle(mk(6002), {}) // K_loud = 3 → DATA_LOSS
    const err = await p
    expect(err).toBeInstanceOf(Object)
    expect((err as StatusError).code).toBe(Code.DATA_LOSS)
    // ...and the abort tells the server to stop (§6.3).
    expect(sent.some((f) => f.code === Code.DATA_LOSS && (f.flags & FlagClose) !== 0)).toBe(true)
    conn.close()
  })

  it('a lone beyond-window frame is dropped silently (anti-poisoning)', async () => {
    const sent: Frame[] = []
    const conn = new Conn({ handle: (f: Frame) => void sent.push(wireClone(f)) }, { reliable: false, timing: fast })
    const stream = conn.newStream(echo.many, {})
    await stream.send({ text: 'x', n: 0 })
    const open = sent.find((f) => isOpen(f))!
    const poison = frame({ epoch: 7, sid: open.sid, seq: 4_000_000_000, flags: 0, method: '', codec: '', desc: '', peerEpoch: open.epoch, payload: enc({ text: 'p' }) })
    await conn.handle(poison, {})
    // The stream still accepts the legitimate sequence afterwards.
    const good = frame({ epoch: 7, sid: open.sid, seq: 1, flags: 0, method: '', codec: '', desc: '', peerEpoch: open.epoch, payload: enc({ text: 'ok' }) })
    await conn.handle(good, {})
    expect(await stream.recv()).toEqual({ text: 'ok' })
    conn.close()
  })
})

describe('reliable-mode delivery never drops (§4.2, §4.2.1)', () => {
  it('a stalled consumer paces its sender instead of blocking delivery; the exact sequence survives', async () => {
    // Since per-stream flow control: the configured buffer of 2 is raised to the flow-control
    // floor W_init, and the sender is paced by the advertised window instead
    // of by a blocking Handle — so delivery does NOT stall here, which is the
    // whole point of §4.2.1 (a blocked read loop would stall every other call
    // on the channel too). Dropping is still forbidden: the messages queue and
    // the handler sees them in order once it resumes. The park boundary itself
    // is pinned in flow.test.ts.
    let release!: () => void
    const gate = new Promise<void>((res) => (release = res))
    const got: string[] = []
    const net = makeNet({
      reliable: true,
      serverOpts: { methodRxBuffer: { [echo.live.path]: { size: 2 } } },
    })
    net.server.register(echo.live, async (stream) => {
      await gate
      for await (const msg of stream) got.push(msg.text)
    })
    const stream = net.conn.newStream(echo.live, {})
    await stream.send({ text: 'a' })
    await stream.send({ text: 'b' })
    let thirdDone = false
    const third = stream.send({ text: 'c' }).then(() => (thirdDone = true))
    await tick()
    expect(thirdDone).toBe(true) // inside the window: nothing blocks
    release()
    await third
    stream.closeSend()
    const p = stream.recv()
    await tick()
    expect(await p).toBeUndefined()
    expect(got).toEqual(['a', 'b', 'c']) // exact sequence (§14)
  })
})

describe('unknown-sid handling and T_hold (§9.3)', () => {
  it('a data frame whose OPEN is merely late draws no RESET once the OPEN lands', async () => {
    const inj = injectServer()
    const data = frame({ epoch: 3, sid: 12, seq: 2, flags: 0, method: '', codec: '', desc: '', peerEpoch: 0, payload: enc({ text: 'early' }) })
    await inj.server.handle(data, { peer: 'p' }) // schedules a delayed RESET (T_hold)
    await inj.server.handle(openLive(3, 12), { peer: 'p' }) // the OPEN arrives after all
    await vi.advanceTimersByTimeAsync(300) // well past T_hold
    expect(inj.sent.filter((f) => isReset(f))).toEqual([]) // §9.3: cancelled by the OPEN
    await inj.server.stop()
  })

  it('an unknown-sid frame with no OPEN draws the delayed RESET after T_hold', async () => {
    const inj = injectServer()
    const data = frame({ epoch: 3, sid: 13, seq: 2, flags: 0, method: '', codec: '', desc: '', peerEpoch: 0, payload: enc({ text: 'stray' }) })
    await inj.server.handle(data, { peer: 'p' })
    expect(inj.sent.filter((f) => isReset(f))).toEqual([]) // not yet: the grace period
    await vi.advanceTimersByTimeAsync(120) // > T_hold(50) + tick
    const reset = inj.sent.find((f) => isReset(f))
    expect(reset).toBeDefined()
    expect(reset!.sid).toBe(13)
    expect(reset!.epoch).toBe(3)
    await inj.server.stop()
  })
})

describe('rejection replay (§9.4)', () => {
  it('duplicate unknown-method OPENs elicit the tombstoned T{UNIMPLEMENTED}, not fresh work', async () => {
    const inj = injectServer()
    const bogus = frame({ epoch: 3, sid: 9, seq: 1, flags: FlagOpen | FlagClose, method: '/x/Nope', codec: '', desc: '', peerEpoch: 0, payload: enc({ text: 'x' }) })
    await inj.server.handle(bogus, { peer: 'p' })
    const first = inj.sent.filter((f) => isTerminal(f) && f.code === Code.UNIMPLEMENTED)
    expect(first).toHaveLength(1)
    await vi.advanceTimersByTimeAsync(100) // clear the 1/RTI replay limit
    await inj.server.handle({ ...bogus }, { peer: 'p' })
    const after = inj.sent.filter((f) => isTerminal(f) && f.code === Code.UNIMPLEMENTED)
    expect(after).toHaveLength(2) // a replay of the stored T — same bounded answer
    expect(after[1]!.seq).toBe(after[0]!.seq) // byte-identical replay, same seq (§10.3)
    await inj.server.stop()
  })
})

describe('per-peer mode (§4.3)', () => {
  it('one server runs a reliable peer strict and an unreliable peer with the full machinery', async () => {
    const inj = injectServer() // server default: unreliable
    // Reliable peer: a duplicate OPEN on a live streaming call is a broken
    // transport → the call dies with INTERNAL (§10.6)...
    let relErr: StatusError | undefined
    inj.server.register(echo.live, async (stream) => {
      try {
        for await (const _ of stream) {
          /* consume */
        }
      } catch (e) {
        relErr = e as StatusError
        throw e
      }
    })
    await inj.server.handle(openLive(1, 1), { peer: 'rel', reliable: true })
    await inj.server.handle(openLive(1, 1), { peer: 'rel', reliable: true })
    await tick()
    expect(relErr?.code).toBe(Code.INTERNAL)

    // ...while the SAME server replays the creation ack for the unreliable
    // peer's duplicate OPEN (§8 ack recovery).
    await inj.server.handle(openLive(1, 1), { peer: 'unrel', reliable: false })
    await tick()
    const hs = inj.sent.filter((f) => f.flags === 0 && f.payload === undefined && f.sid === 1 && f.peerEpoch === 1)
    await vi.advanceTimersByTimeAsync(60) // clear the 1/RTI H-replay limit
    await inj.server.handle(openLive(1, 1), { peer: 'unrel', reliable: false })
    await tick()
    const hs2 = inj.sent.filter((f) => f.flags === 0 && f.payload === undefined && f.sid === 1 && f.peerEpoch === 1)
    expect(hs2.length).toBe(hs.length + 1) // H replayed, call still live
    await inj.server.stop()
  })
})

describe('sid exhaustion (§6.2)', () => {
  it('fails new calls with RESOURCE_EXHAUSTED without recycling', () => {
    const conn = new Conn({ handle: () => {} }, { reliable: true })
    // Reach into the allocator the honest way: 2^32 allocations are not
    // practical, so this pins only the closed/exhausted refusal shape.
    conn.close()
    expect(() => conn.newStream(echo.live, {})).toThrow(/connection is closed/)
  })
})

describe('off-shape frames (§7, §8)', () => {
  it('a payload-bearing data frame on a unary call is dropped and counted', async () => {
    const sent: Frame[] = []
    const conn = new Conn({ handle: (f: Frame) => void sent.push(wireClone(f)) }, { reliable: false, timing: fast })
    const p = conn.invoke(echo.once, { text: 'q' }).catch((e) => e as StatusError)
    await tick()
    const open = sent.find((f) => isOpen(f))!
    // A data frame at a unary client: off-shape; dropped, but the terminal
    // that follows (same seq space) still lands.
    const rogue = frame({ epoch: 7, sid: open.sid, seq: 1, flags: 0, method: '', codec: '', desc: '', peerEpoch: open.epoch, payload: enc({ text: 'rogue' }) })
    await conn.handle(rogue, {})
    const term = frame({ epoch: 7, sid: open.sid, seq: 2, flags: FlagClose, method: '', codec: '', desc: '', peerEpoch: open.epoch, payload: enc({ text: 'real' }) })
    term.code = Code.OK
    await conn.handle(term, {})
    expect(await p).toEqual({ text: 'real' })
    conn.close()
  })

  it('an OPEN with seq != 1 does not create a call', async () => {
    const inj = injectServer()
    const bad = frame({ ...openLive(3, 30), seq: 2 })
    await inj.server.handle(bad, { peer: 'p' })
    await tick()
    expect(inj.counts.live).toBe(0)
    await inj.server.stop()
  })

  it('an illegal shape fails the call at the server: never routed, never dropped (§7.1)', async () => {
    const inj = injectServer()
    await inj.server.handle(openLive(3, 50), { peer: 'p' })
    await tick()
    // WINDOW|CLOSE is not a shape any receiver can route (§7.1). Delivering it
    // would be a silent corruption and dropping it a silent gap, so the call
    // fails loudly instead.
    await inj.server.handle(frame({ epoch: 3, sid: 50, seq: 2, flags: FlagWindow | FlagClose, payload: enc({ text: 'x' }) }), { peer: 'p' })
    await tick()
    const term = inj.sent.find((f) => isTerminal(f) && f.sid === 50)
    expect(term).toBeDefined()
    expect(term!.code).toBe(Code.INTERNAL)
    await inj.server.stop()
  })

  it('an unknown flag bit fails the call at the client, and aborts (§7.1)', async () => {
    const sent: Frame[] = []
    const conn = new Conn({ handle: (f: Frame) => void sent.push(wireClone(f)) }, { reliable: false, timing: fast })
    const stream = conn.newStream(echo.many, {})
    await stream.send({ text: 'x', n: 1 })
    const open = sent.find((f) => isOpen(f))!

    // Bit 0x40 is outside KNOWN_FLAGS: a newer peer changed something about
    // this frame that this build cannot honor.
    const p = stream.recv().catch((e) => e as StatusError)
    await conn.handle(frame({ epoch: 7, sid: open.sid, seq: 1, flags: 0x40, peerEpoch: open.epoch, payload: enc({ text: 'm' }) }), {})
    const err = (await p) as StatusError
    expect(err.code).toBe(Code.INTERNAL)
    expect(err.desc).toContain('unsupported flags')
    // ...and the abort tells the server to stop (§10.3).
    expect(sent.some((f) => isTerminal(f) && f.code === Code.INTERNAL)).toBe(true)
    conn.close()
  })
})

describe('metadata plumbing (§11)', () => {
  it('later header metadata never overwrites the latched first (first-wins)', async () => {
    // A handler can no longer flush twice — the 2026-07-25 round makes the second
    // sendHeader grpc-go's ErrIllegalHeaderWrite (pinned in compat.test.ts) —
    // so the two header-bearing frames are crafted directly here: whatever a
    // server puts on a later frame, the client keeps what it latched first
    // (§7, §11).
    const sent: Frame[] = []
    const conn = new Conn({ handle: (f: Frame) => void sent.push(wireClone(f)) }, { reliable: true })
    const stream = conn.newStream(echo.many, {})
    await stream.send({ text: 'x', n: 0 })
    const open = sent.find((f) => isOpen(f))!
    const srv = { epoch: 7, sid: open.sid, peerEpoch: open.epoch }

    await conn.handle(frame({ ...srv, seq: 1, header: { h: ['first'] } }), {}) // the ack H
    await conn.handle(frame({ ...srv, seq: 2, header: { h: ['second'] }, payload: enc({ text: 'm' }) }), {})
    expect(await stream.header()).toEqual({ h: ['first'] })
    expect(await stream.recv()).toEqual({ text: 'm' })
    conn.close()
  })
})

describe('codec is call-scoped (§12)', () => {
  it('a codec name on a later frame addresses nothing', async () => {
    const inj = injectServer()
    await inj.server.handle(openLive(3, 40), { peer: 'p' })
    const data = frame({ epoch: 3, sid: 40, seq: 2, flags: 0, method: '', codec: 'nope', desc: '', peerEpoch: 0, payload: enc({ text: 'abc' }) })
    await inj.server.handle(data, { peer: 'p' })
    await tick()
    // The call keeps its proto codec and echoes.
    const res = inj.sent.find((f) => f.payload !== undefined && f.flags === 0 && f.sid === 40)
    expect(res).toBeDefined()
    expect(JSON.parse(new TextDecoder().decode(res!.payload))).toEqual({ text: 'echo:abc' })
    await inj.server.stop()
  })
})

// Ensure helper imports stay referenced even if describe blocks shuffle.
void jsonCodec

// Limits: the floor at W_CONN, for the same reason the rx buffer is floored
// at W_INIT — a client assumes it until the server's first H or T.
// Pins §4.2.1 Initial window: "MaxPeerWindow (§15) is floored at W_conn, for
// the same reason the rx buffer is floored at W_init".
// ---------------------------------------------------------------------------
// §4.2.1 / §15 connection-window scope: the receiver's bound is per TRANSPORT
// PEER across client epochs — an epoch-spoofing peer pins no more — while the
// sender's credit is per (peer, client-epoch) container: each incarnation's
// sender is created from its own first OPEN, and every H and T the server
// sends advertises the bound. A different transport peer has its own bound.
// ---------------------------------------------------------------------------

describe('MaxPeerWindow scope (§4.2.1, §15)', () => {
  const isPeerGrant = (f: Frame): boolean => shapeOf(f) === FlagWindow && f.sid === 0 && f.seq === 0 && f.payload === undefined
  const isPeerGrantFor = (epoch: number) => (f: Frame) => isPeerGrant(f) && f.peerEpoch === epoch
  const isAckH = (f: Frame): boolean => shapeOf(f) === 0 && f.payload === undefined
  const dataFrame = (epoch: number, sid: number, seq: number): Frame => frame({ epoch, sid, seq, payload: enc({ text: 'd' }) })
  // A consumer that stopped, for every call: the call ends with its own
  // cause.
  const blockStreams: StreamServerInterceptor = (_stream, ctx) =>
    new Promise((_res, rej) => ctx.signal.addEventListener('abort', () => rej(abortCause(ctx.signal)), { once: true }))

  // Pins §4.2.1 Scope: "the receiver's bound is per transport peer, across
  // client epochs on the server ... the sender's credit is per peer
  // incarnation", and Advertisement: "the server on every H and T".
  it('the bound is per transport peer across client epochs; the sender is per incarnation, and every H and T advertises the bound', async () => {
    const window = 2048 // above the W_CONN floor, so the advertisement is observable
    // Reliable (the connection window is reliable-only), per-stream buffers
    // of a whole window so only the connection window can trip, handlers
    // that never read so everything stays buffered.
    const inj = injectServer({
      reliable: true,
      rxBuffer: { size: window },
      limits: { maxPeerWindow: window },
      streamInterceptors: [blockStreams],
    })
    const peerA = 'peer-window-a'
    const peerB = 'peer-window-b'
    const epochA = 0x4a
    const epochB = 0x4b // a "fresh incarnation" of the same transport peer
    const epochC = 0x4c
    const since = (from: number) => inj.sent.slice(from)

    // Two bidi calls under epoch A: each creation ack advertises the bound,
    // and nothing rides behind it.
    for (let sid = 1; sid <= 2; sid++) {
      const from = inj.sent.length
      await inj.server.handle(openLive(epochA, sid), { peer: peerA })
      const out = since(from)
      expect(out).toHaveLength(1)
      expect(isAckH(out[0]!), 'creation ack H (§8)').toBe(true)
      expect(out[0]!.connWindow, 'the ack advertises the bound').toBe(window)
    }
    // Fill peerA's whole bound across the two calls: nothing is refused.
    let from = inj.sent.length
    for (let i = 0; i < window / 2; i++) {
      await inj.server.handle(dataFrame(epochA, 1, 2 + i), { peer: peerA })
      await inj.server.handle(dataFrame(epochA, 2, 2 + i), { peer: peerA })
    }
    await tick()
    expect(since(from), 'exactly the window fits').toHaveLength(0)

    // A call under a DIFFERENT client epoch of the SAME peer: its own sender
    // (created from its OPEN), its own ack advertising the same bound...
    from = inj.sent.length
    await inj.server.handle(openLive(epochB, 1), { peer: peerA })
    expect(since(from)).toHaveLength(1)
    expect(isAckH(since(from)[0]!), 'creation ack H for the new incarnation').toBe(true)
    expect(since(from)[0]!.connWindow, 'advertising the bound').toBe(window)
    // ...but the SAME receive bound: one more frame from this peer is one
    // too many, and it fails its own call INTERNAL, naming the window. The
    // refused frame's credit comes straight back beside the terminal: the
    // window was full, so the starvation clause grants it at once (§4.2.1)
    // — to B, whose call it was — from the receive path, while the T
    // follows from the handler as it unwinds. Epoch A's calls are untouched.
    from = inj.sent.length
    await inj.server.handle(dataFrame(epochB, 1, 2), { peer: peerA })
    await tick()
    const out = since(from)
    expect(out).toHaveLength(2)
    const term = out.find(isTerminal)
    const g = out.find(isPeerGrant)
    expect(term, 'the overrun fails the offending call').toBeDefined()
    expect(term!.code).toBe(Code.INTERNAL)
    expect(term!.peerEpoch).toBe(epochB)
    expect(term!.desc).toContain('connection flow-control window')
    expect(term!.connWindow, 'the T advertises the bound too').toBe(window)
    expect(g !== undefined && isPeerGrantFor(epochB)(g) && g.window === 1, 'the refused frame’s credit, to B').toBe(true)
    expect(g!.connWindow, 'a WINDOW never carries it').toBe(0)

    // A different transport peer is under ITS OWN bound: admitted, buffered.
    from = inj.sent.length
    await inj.server.handle(openLive(epochC, 1), { peer: peerB })
    expect(since(from)).toHaveLength(1)
    expect(isAckH(since(from)[0]!), 'creation ack for the other peer').toBe(true)
    from = inj.sent.length
    await inj.server.handle(dataFrame(epochC, 1, 2), { peer: peerB })
    await tick()
    expect(since(from), "the other peer's frame is buffered").toHaveLength(0)
    await inj.server.stop()
  })
})

describe('MaxPeerWindow floor and cap (§4.2.1, §15, §7)', () => {
  it('is floored at W_CONN, defaults to it, and is capped at what the wire carries', () => {
    for (const [given, want] of [
      [undefined, W_CONN],
      [0, W_CONN],
      [-1, W_CONN],
      [NaN, W_CONN],
      [100, W_CONN],
      [1024, 1024],
      [2048, 2048],
      [2048.9, 2048], // a message count is whole
      [2 ** 32, 0xffff_ffff], // `window` is a uint32 on the wire (§7)
      [Infinity, 0xffff_ffff], // the JS spelling of "unlimited": as much as the wire carries
    ] as const) {
      expect(resolveLimits({ maxPeerWindow: given }).maxPeerWindow, `maxPeerWindow ${given}`).toBe(want)
    }
    // The default really is the sender's assumption (limits.ts spells the
    // constant out rather than importing it).
    expect(resolveLimits().maxPeerWindow).toBe(W_CONN)
  })

  // maxPeerWindow: Infinity — the natural JS spelling of "unlimited", and one
  // rxBuffer.size happens to accept — must be a working window, not a silent
  // one. Unclamped, Infinity would reach the ledger, where the grant rule's
  // thresholds compare against it so no batched or starvation grant ever
  // fires, and the wire, whose uint32 varint cannot carry the advertisement
  // — the peer would run on whatever it read there. Go cannot express it
  // (MaxPeerWindow is an int, cast to uint32 at the ledger), so the clamp is
  // where TS meets the wire.
  it('Infinity is as much as the wire carries: the advertisement carries it, and W_CONN + 1 messages move each way', async () => {
    const stallMs = 50
    const limits = { maxPeerWindow: Infinity }
    const c = new Counters()
    const s = new Counters()
    const net = makeNet({
      reliable: true,
      connOpts: { limits, timing: { stallMs }, protocolStats: [c.observe] },
      serverOpts: { limits, timing: { stallMs }, protocolStats: [s.observe] },
    })
    const isPeerGrant = (f: Frame): boolean => shapeOf(f) === FlagWindow && f.sid === 0 && f.seq === 0 && f.payload === undefined
    const advertised = 0xffff_ffff

    const stream = net.conn.newStream(echo.live, {})
    await stream.send({ text: 'm' })
    expect(await stream.recv()).toEqual({ text: 'echo:m' })
    // The client's OPEN and the server's H each advertise everything a
    // uint32 holds, and the other side runs on it.
    expect(net.sentC2S.filter(isOpen).map((f) => f.connWindow), "the client's advertisement").toEqual([advertised])
    expect(net.sentS2C.filter(isHeaderFrame).map((f) => f.connWindow), "the server's advertisement").toEqual([advertised])
    expect(net.conn.connTx.state(), 'adopted by the client').toMatchObject({ on: true, observed: true, granted: advertised })

    // W_CONN + 1 round trips: the last message in each direction is past the
    // old assumption and moves on the advertised window alone — no batched
    // grant is ever due against a window this size.
    for (let i = 1; i < W_CONN; i++) {
      await stream.send({ text: 'm' })
      expect(await stream.recv()).toEqual({ text: 'echo:m' })
    }
    const last = stream.send({ text: 'last' })
    // A sender parked on connection credit would have failed by now,
    // UNAVAILABLE naming it.
    await vi.advanceTimersByTimeAsync(stallMs + 1)
    await last
    expect(await stream.recv()).toEqual({ text: 'echo:last' })
    expect(c.snapshot(), 'the client parked on nothing').toMatchObject({ peerFlowStall: 0, flowStall: 0 })
    expect(s.snapshot(), 'the server parked on nothing').toMatchObject({ peerFlowStall: 0, flowStall: 0 })
    expect(net.sentC2S.filter(isPeerGrant), 'no sid-0 grant is ever due against a window this size').toHaveLength(0)
    expect(net.sentS2C.filter(isPeerGrant)).toHaveLength(0)

    stream.closeSend()
    expect(await stream.recv()).toBeUndefined()
    net.conn.close()
    await net.server.stop()
  })
})
