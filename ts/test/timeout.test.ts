// The timeout system (PROTOCOL.md §10): eventual termination under loss —
// the TS twin of the Go timeout_test.go / retx_test.go / ackrecovery_test.go
// scenarios, driven deterministically by fake timers.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { Server } from '../src/server'
import { Code, type StatusError } from '../src/status'
import type { Timing } from '../src/timing'
import { FlagOpen, frame, isClose, isData, isHeaderFrame, isOpen, isPing, isTerminal, type Frame } from '../src/wire'
import { echo, makeNet, registerEcho, tick } from '../src/testing'

// probe = 150ms, tick = 25ms, T_call = 300ms, T_live = 450ms, RTI = 50ms.
const fast: Timing = { callMs: 300, livenessMs: 450, retransmitMs: 50, tombstoneMs: 1000, holdMs: 50 }

const fastNet = () => makeNet({ reliable: false, connOpts: { timing: fast }, serverOpts: { timing: fast } })

beforeEach(() => {
  vi.useFakeTimers()
})
afterEach(() => {
  vi.useRealTimers()
})

describe('unary deadline (§10.2, §10.7)', () => {
  it('a blackholed unary terminates within T_call and never executes', async () => {
    const net = fastNet()
    net.c2s.filter = () => false
    net.s2c.filter = () => false
    const p = net.conn.invoke(echo.once, { text: 'x' }).catch((e) => e)
    await vi.advanceTimersByTimeAsync(400)
    const err = (await p) as StatusError
    expect(err.code).toBe(Code.DEADLINE_EXCEEDED)
    expect(net.counts.once).toBe(0)
  })

  it('the default deadline travels as the OPEN timeout and bounds the handler', async () => {
    const net = fastNet()
    let deadline: number | undefined
    const orig = net.counts
    void orig
    net.server.register(echo.once, (_req, ctx) => {
      deadline = ctx.deadline
      return { text: 'ok' }
    })
    const p = net.conn.invoke(echo.once, { text: 'x' })
    await vi.advanceTimersByTimeAsync(10)
    await p
    const open = net.sentC2S.find((f) => isOpen(f))!
    expect(open.timeoutMs).toBeGreaterThan(0)
    expect(open.timeoutMs).toBeLessThanOrEqual(300)
    expect(deadline).toBeDefined()
  })

  it('a non-positive propagated budget expires the handler at once (§10.2)', async () => {
    const net = fastNet()
    let sawAborted: boolean | undefined
    net.server.register(echo.once, (_req, ctx) => {
      sawAborted = ctx.signal.aborted
      return { text: 'never' }
    })
    // Craft an OPEN|CLOSE with an already-expired budget.
    const open = { ...net.sentC2S, length: 0 } // noop; build manually below
    void open
    const f = frame({
      epoch: 7,
      sid: 1,
      seq: 1,
      flags: 3, // OPEN|CLOSE
      method: echo.once.path,
      codec: '',
      desc: '',
      peerEpoch: 0,
      timeoutMs: -5,
      payload: new TextEncoder().encode(JSON.stringify({ text: 'x' })),
    })
    await net.server.handle(f, { peer: net.peer })
    await tick()
    expect(sawAborted).toBe(true)
    const term = net.sentS2C.find((g) => isTerminal(g) && g.sid === 1)
    expect(term?.code).toBe(Code.DEADLINE_EXCEEDED)
  })
})

describe('control retransmission and replay (§10.3, §9.2)', () => {
  it('a lost terminal is recovered by OPEN retransmission → tombstone replay; the handler runs once', async () => {
    const net = fastNet()
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
    expect(net.counts.once).toBe(1) // at-most-once: replay, not re-execution (§14)
  })

  it('a lost creation ack is recovered: dup OPEN elicits an H replay that stops the retransmission', async () => {
    const net = fastNet()
    let droppedH = false
    net.s2c.filter = (f) => {
      if (!droppedH && isHeaderFrame(f)) {
        droppedH = true
        return false
      }
      return true
    }
    const stream = net.conn.newStream(echo.count, {})
    await vi.advanceTimersByTimeAsync(120) // ≥ 2×RTI: retx → H replay arrives
    const opensBefore = net.sentC2S.filter((f) => isOpen(f)).length
    expect(opensBefore).toBeGreaterThanOrEqual(2) // the OPEN did retransmit
    await vi.advanceTimersByTimeAsync(300)
    const opensAfter = net.sentC2S.filter((f) => isOpen(f)).length
    expect(opensAfter).toBe(opensBefore) // ...and the replayed H stopped it
    // The call is fully usable afterwards.
    await stream.send({ text: 'a' })
    stream.closeSend()
    const p = stream.recv()
    await vi.advanceTimersByTimeAsync(50)
    expect(await p).toEqual({ text: '1' })
  })

  it('a lost half-close keeps retransmitting until the terminal arrives', async () => {
    const net = fastNet()
    let drops = 0
    net.c2s.filter = (f) => {
      if (isClose(f) && f.code === undefined && !isOpen(f) && drops < 2) {
        drops++
        return false
      }
      return true
    }
    const stream = net.conn.newStream(echo.count, {})
    await tick()
    await stream.send({ text: 'a' })
    stream.closeSend() // dropped twice, then the RTI retransmission lands
    const p = stream.recv()
    await vi.advanceTimersByTimeAsync(400)
    expect(await p).toEqual({ text: '1' })
    expect(drops).toBe(2)
  })
})

describe('stream probe (§10.5)', () => {
  it('a lost terminal on an idle stream is recovered by the probe within ~T_probe + RTI', async () => {
    const net = fastNet()
    let droppedTerm = false
    net.s2c.filter = (f) => {
      if (!droppedTerm && isTerminal(f)) {
        droppedTerm = true
        return false
      }
      return true
    }
    const stream = net.conn.newStream(echo.many, {})
    await stream.send({ text: 'm', n: 1 })
    await tick()
    expect(await stream.recv()).toEqual({ text: 'm#0' }) // data delivered; T dropped
    const p = stream.recv()
    await vi.advanceTimersByTimeAsync(300) // T_probe(150) + RTI + jitter
    expect(await p).toBeUndefined() // probe → tombstone replay → EOF
  })

  it('an orphaned handler is reclaimed by its own probe → RESET (§9.3)', async () => {
    const net = fastNet()
    let handlerErr: StatusError | undefined
    let handlerDone = false
    net.server.register(echo.live, async (stream) => {
      try {
        await stream.recv()
      } catch (e) {
        handlerErr = e as StatusError
      } finally {
        handlerDone = true
      }
    })
    // The client's abort CLOSE never reaches the server: the handler is
    // orphaned — nothing retransmits for it, other traffic keeps the peer
    // alive is irrelevant here (single call).
    net.c2s.filter = (f) => !(isClose(f) && !isOpen(f))
    const stream = net.conn.newStream(echo.live, {})
    await tick()
    stream.cancel() // local-immediate; the abort frame is blackholed
    await vi.advanceTimersByTimeAsync(400) // server probes after T_probe; client RESETs
    expect(handlerDone).toBe(true)
    expect(handlerErr?.code).toBe(Code.UNAVAILABLE)
    expect(handlerErr?.desc).toContain('reset by peer')
    // The peer disowned the call: no terminal was sent for it (§9.3).
    expect(net.sentS2C.some((f) => isTerminal(f))).toBe(false)
  })

  it('a healthy idle stream is never killed by silence', async () => {
    const net = fastNet()
    const stream = net.conn.newStream(echo.live, {})
    await tick()
    await vi.advanceTimersByTimeAsync(2000) // >> T_live; probes keep flowing
    // Still alive: an exchange works.
    await stream.send({ text: 'x' })
    const p = stream.recv()
    await vi.advanceTimersByTimeAsync(30)
    expect(await p).toEqual({ text: 'echo:x' })
    const probes = net.sentC2S.filter((f) => isPing(f) && f.sid !== 0)
    expect(probes.length).toBeGreaterThan(3) // both idle clocks passed T_probe repeatedly
  })
})

describe('peer liveness (§10.4)', () => {
  it('a vanished client expires the handler within T_live with no terminal sent', async () => {
    const net = fastNet()
    let handlerErr: StatusError | undefined
    net.server.register(echo.live, async (stream) => {
      try {
        await stream.recv()
      } catch (e) {
        handlerErr = e as StatusError
      }
    })
    const stream = net.conn.newStream(echo.live, {})
    await tick()
    void stream
    net.c2s.filter = () => false // the client vanishes: no data, no PING, no abort
    const before = net.sentS2C.filter((f) => isTerminal(f)).length
    await vi.advanceTimersByTimeAsync(600) // > T_live
    expect(handlerErr?.code).toBe(Code.UNAVAILABLE)
    expect(handlerErr?.desc).toContain('peer lost')
    expect(net.sentS2C.filter((f) => isTerminal(f)).length).toBe(before) // §10.4: no T
  })

  it('a vanished server fails client calls within T_live', async () => {
    const net = fastNet()
    const stream = net.conn.newStream(echo.live, {})
    await tick()
    net.s2c.filter = () => false // the server vanishes
    const p = stream.recv().catch((e) => e)
    await vi.advanceTimersByTimeAsync(600)
    const err = (await p) as StatusError
    expect(err.code).toBe(Code.UNAVAILABLE)
    expect(err.desc).toContain('peer lost')
  })

  it('keepalive PINGs keep a silent-but-healthy peer alive', async () => {
    const net = fastNet()
    let handlerErr: StatusError | undefined
    net.server.register(echo.live, async (stream) => {
      try {
        await stream.recv()
      } catch (e) {
        handlerErr = e as StatusError
      }
    })
    const stream = net.conn.newStream(echo.live, {})
    await tick()
    await vi.advanceTimersByTimeAsync(1500) // >> T_live with PINGs flowing
    expect(handlerErr).toBeUndefined()
    expect(net.sentC2S.some((f) => isPing(f) && f.sid === 0)).toBe(true)
    stream.cancel()
  })
})

describe('at-most-once (§9.2, §9.4, §14)', () => {
  it('a network-duplicated OPEN never re-executes the handler', async () => {
    const net = fastNet()
    let duped = false
    net.c2s.filter = (f) => {
      if (isOpen(f) && !duped) {
        duped = true
        // Redeliver the same OPEN out of band.
        queueMicrotask(() => void net.server.handle({ ...f }, { peer: net.peer }))
      }
      return true
    }
    const p = net.conn.invoke(echo.once, { text: 'x' })
    await vi.advanceTimersByTimeAsync(50)
    expect(await p).toEqual({ text: 'echo:x' })
    expect(net.counts.once).toBe(1)
  })

  it('a duplicate OPEN long after completion hits the tombstone: replay, no re-execution', async () => {
    const net = fastNet()
    const p = net.conn.invoke(echo.once, { text: 'x' })
    await vi.advanceTimersByTimeAsync(10)
    await p
    const open = net.sentC2S.find((f) => isOpen(f))!
    const before = net.sentS2C.filter((f) => isTerminal(f)).length
    await vi.advanceTimersByTimeAsync(100) // clear the replay rate-limit window
    await net.server.handle({ ...open }, { peer: net.peer })
    await tick()
    expect(net.counts.once).toBe(1)
    expect(net.sentS2C.filter((f) => isTerminal(f)).length).toBe(before + 1) // replayed T
  })
})

describe('multi-frame envelops (§4.1)', () => {
  it('frames later in an envelop land on the call created earlier in it', async () => {
    // An inject server (recording tx, no live client — the Go
    // TestWireShape_MultiFrameEnvelop shape): two sequential awaited
    // handle() calls are exactly what unpack() does for a 2-frame envelop.
    const sent: Frame[] = []
    const server = new Server({ handle: (f: Frame) => void sent.push(f) }, { reliable: false, timing: fast })
    registerEcho(server)
    const open = frame({
      epoch: 5,
      sid: 21,
      seq: 1,
      flags: FlagOpen,
      method: echo.live.path,
      codec: '',
      desc: '',
      peerEpoch: 0,
    })
    const data = frame({
      epoch: 5,
      sid: 21,
      seq: 2,
      flags: 0,
      method: '',
      codec: '',
      desc: '',
      peerEpoch: 0,
      payload: new TextEncoder().encode(JSON.stringify({ text: 'abc' })),
    })
    await server.handle(open, { peer: 'p' })
    await server.handle(data, { peer: 'p' })
    await vi.advanceTimersByTimeAsync(20)
    const h = sent.find((f) => isHeaderFrame(f) && f.sid === 21)
    expect(h).toBeDefined()
    expect(h!.peerEpoch).toBe(5) // server frames echo the client epoch (§6.1)
    const res = sent.find((f) => isData(f) && f.sid === 21)
    expect(res).toBeDefined()
    expect(JSON.parse(new TextDecoder().decode(res!.payload))).toEqual({ text: 'echo:abc' })
    await server.stop()
  })
})
