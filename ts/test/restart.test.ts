// Restart walkthroughs (PROTOCOL.md §6.5): a restart is not a special
// mechanism — it is what the epoch rules compose into. TS twins of the Go
// characterization tests (restart_test.go).

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { Conn } from '../src/conn'
import { Server } from '../src/server'
import { Code, type StatusError } from '../src/status'
import type { FrameHandler } from '../src/seam'
import type { Timing } from '../src/timing'
import { isReset, type Frame } from '../src/wire'
import { echo, registerEcho, tick, wireClone } from './helpers'

const fast: Timing = { callMs: 300, livenessMs: 450, retransmitMs: 50, tombstoneMs: 1000, holdMs: 50 }

beforeEach(() => {
  vi.useFakeTimers()
})
afterEach(() => {
  vi.useRealTimers()
})

describe('client restart (§6.1, §6.5)', () => {
  it('sid collision across incarnations: the peer_epoch echo protects the new call and reclaims exactly the old one', async () => {
    // Two client incarnations behind ONE transport address: server frames
    // reach whichever incarnations are alive, exactly like datagrams to a
    // shared address.
    const peer = 'addr-1'
    const conns: Conn[] = []
    let server!: Server
    const serverTx: FrameHandler = {
      async handle(f: Frame): Promise<void> {
        for (const c of [...conns]) await c.handle(wireClone(f), {})
      },
    }
    const clientTx = (aliveRef: { dead: boolean }): FrameHandler => ({
      async handle(f: Frame): Promise<void> {
        if (aliveRef.dead) return // a vanished incarnation transmits nothing
        await server.handle(wireClone(f), { peer })
      },
    })

    server = new Server(serverTx, { reliable: false, timing: fast })
    const handlerErr: StatusError[] = []
    let liveHandlers = 0
    server.register(echo.live, async (stream) => {
      liveHandlers++
      try {
        for await (const msg of stream) await stream.send({ text: `echo:${msg.text}` })
      } catch (e) {
        handlerErr.push(e as StatusError)
        throw e
      }
    })

    // Incarnation 1 opens a call (sid 1 in its epoch) and vanishes.
    const alive1 = { dead: false }
    const conn1 = new Conn(clientTx(alive1), { reliable: false, timing: fast })
    conns.push(conn1)
    const s1 = conn1.newStream(echo.live, {})
    await s1.send({ text: 'a' })
    await tick()
    expect(liveHandlers).toBe(1)
    alive1.dead = true
    conns.splice(conns.indexOf(conn1), 1) // it hears nothing either

    // Incarnation 2 re-allocates the very same sid 1 to a live call of its
    // own; the calls coexist at the server (§6.2 keying).
    const alive2 = { dead: false }
    const conn2 = new Conn(clientTx(alive2), { reliable: false, timing: fast })
    conns.push(conn2)
    const s2 = conn2.newStream(echo.live, {})
    await s2.send({ text: 'b' })
    const p = s2.recv()
    await vi.advanceTimersByTimeAsync(20)
    expect(await p).toEqual({ text: 'echo:b' })
    expect(liveHandlers).toBe(2)

    // The old call's server frames (probes, once idle) name the OLD
    // incarnation in peer_epoch: conn2 refuses them — even though the sid
    // matches its live call — and answers RESETs that re-echo that
    // peer_epoch, reclaiming exactly the old call (§9.3).
    await vi.advanceTimersByTimeAsync(500)
    expect(handlerErr.map((e) => e.code)).toEqual([Code.UNAVAILABLE])

    // The new incarnation's call was never disturbed.
    await s2.send({ text: 'c' })
    const p2 = s2.recv()
    await vi.advanceTimersByTimeAsync(20)
    expect(await p2).toEqual({ text: 'echo:c' })
    conn2.close()
    await server.stop()
  })
})

describe('server restart (§6.5)', () => {
  function swapNet() {
    // The client talks to whichever server incarnation is active; frames
    // from a dead incarnation go nowhere.
    const peer = 'client-1'
    let conn!: Conn
    const active: { server: Server | undefined } = { server: undefined }
    const clientTx: FrameHandler = {
      async handle(f: Frame): Promise<void> {
        await active.server?.handle(wireClone(f), { peer })
      },
    }
    const serverTxFor = (self: () => Server): FrameHandler => ({
      async handle(f: Frame): Promise<void> {
        if (active.server !== self()) return // a dead incarnation transmits nothing
        await conn.handle(wireClone(f), {})
      },
    })
    conn = new Conn(clientTx, { reliable: false, timing: fast })
    return { peer, conn, active, serverTxFor }
  }

  it('mid-unary: the retransmitted OPEN re-executes on the new incarnation and the call succeeds (§16 L2)', async () => {
    const net = swapNet()
    let execs = 0
    const mkServer = () => {
      let s!: Server
      s = new Server(net.serverTxFor(() => s), { reliable: false, timing: fast })
      s.register(echo.once, (req) => {
        execs++
        return { text: `echo:${req.text}` }
      })
      return s
    }
    const server1 = mkServer()
    net.active.server = server1

    // Drop server1's response by killing the incarnation right after the
    // handler ran — its T is emitted by a dead server and goes nowhere.
    const p = net.conn.invoke(echo.once, { text: 'x' })
    net.active.server = undefined // vanished before the T could travel
    await tick()
    expect(execs).toBe(1)

    const server2 = mkServer()
    net.active.server = server2

    // The client's §10.3 OPEN retransmission reaches the new incarnation as
    // a fresh call: the handler executes AGAIN, and the response — carrying
    // the new server epoch — is accepted by the not-yet-locked stream.
    await vi.advanceTimersByTimeAsync(200)
    expect(await p).toEqual({ text: 'echo:x' })
    expect(execs).toBe(2) // the hidden double execution, exactly as documented
    net.conn.close()
    await server2.stop()
  })

  it('mid-stream: the locked stream is RESET by the new incarnation and fails with UNAVAILABLE', async () => {
    const net = swapNet()
    const mkServer = () => {
      let s!: Server
      s = new Server(net.serverTxFor(() => s), { reliable: false, timing: fast })
      registerEcho(s)
      return s
    }
    const server1 = mkServer()
    net.active.server = server1

    const stream = net.conn.newStream(echo.live, {})
    await stream.send({ text: 'a' })
    const p0 = stream.recv()
    await vi.advanceTimersByTimeAsync(20)
    expect(await p0).toEqual({ text: 'echo:a' }) // the stream locked to server1's epoch

    const server2 = mkServer()
    net.active.server = server2 // restart

    // Nothing the new incarnation says can be delivered as data; the next
    // client frame draws a delayed RESET (§9.3) echoing the client's epoch,
    // and the call fails with UNAVAILABLE ("call reset by peer").
    const p = stream.recv().catch((e) => e)
    await stream.send({ text: 'b' })
    await vi.advanceTimersByTimeAsync(200) // ≥ T_hold + delivery
    const err = (await p) as StatusError
    expect(err.code).toBe(Code.UNAVAILABLE)
    expect(err.desc).toContain('reset by peer')
    net.conn.close()
    await server2.stop()
  })

  it('a RESET from a dead incarnation is ignored (epoch echo mismatch, §9.3)', async () => {
    const net = swapNet()
    let s!: Server
    s = new Server(net.serverTxFor(() => s), { reliable: false, timing: fast })
    registerEcho(s)
    net.active.server = s

    const stream = net.conn.newStream(echo.live, {})
    await stream.send({ text: 'a' })
    await vi.advanceTimersByTimeAsync(20)

    // A stale RESET whose epoch names some other incarnation must not kill
    // the live call.
    const staleReset: Frame = { epoch: 0xdeadbeef, sid: 1, seq: 0, flags: 4, method: '', codec: '', desc: '', peerEpoch: 0 }
    expect(isReset(staleReset)).toBe(true)
    await net.conn.handle(staleReset, {})
    await stream.send({ text: 'b' })
    const p = stream.recv()
    await vi.advanceTimersByTimeAsync(20)
    expect(await p).toEqual({ text: 'echo:a' }) // still alive, in order
    net.conn.close()
    await s.stop()
  })
})
