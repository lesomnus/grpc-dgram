// The WebRTC DataChannel adapter (src/transport/webrtc/index.ts) against a mock channel pair
// implementing the standard RTCDataChannel surface — reliability
// autodetection, §4.4 size refusal, open/backpressure gating, and the §4.5
// teardown duty. The reliable echo e2e at the top is the TS twin of the Go
// pion adapter's final-goal demo.

import { afterEach, describe, expect, it, vi } from 'vitest'
import { Conn } from '../src/conn'
import { Server } from '../src/server'
import { Code, type StatusError } from '../src/status'
import { channelReliable, DataChannelGateway, DataChannelTransport, type DataChannelLike } from '../src/transport/webrtc'
import { echo, registerEcho, tick } from './helpers'

class MockDC implements DataChannelLike {
  readyState = 'connecting'
  ordered = true
  maxRetransmits: number | null = null
  maxPacketLifeTime: number | null = null
  bufferedAmount = 0
  bufferedAmountLowThreshold = 0
  binaryType = 'blob'
  sent: Uint8Array[] = []
  peer: MockDC | undefined
  private listeners = new Map<string, ((ev: unknown) => void)[]>()

  addEventListener(type: string, fn: (ev: never) => void): void {
    const l = this.listeners.get(type) ?? []
    l.push(fn as (ev: unknown) => void)
    this.listeners.set(type, l)
  }

  emit(type: string, ev: unknown = {}): void {
    for (const fn of this.listeners.get(type) ?? []) fn(ev)
  }

  send(data: Uint8Array): void {
    if (this.readyState !== 'open') throw new Error('InvalidStateError: channel not open')
    this.sent.push(data)
    const peer = this.peer
    if (peer !== undefined) {
      queueMicrotask(() => {
        if (peer.readyState === 'open') peer.emit('message', { data: data.slice().buffer })
      })
    }
  }

  open(): void {
    this.readyState = 'open'
    this.emit('open')
  }

  close(): void {
    if (this.readyState === 'closed') return
    this.readyState = 'closed'
    this.emit('close')
    const peer = this.peer
    if (peer !== undefined && peer.readyState !== 'closed') peer.close()
  }
}

function mockPair(configure?: (dc: MockDC) => void): [MockDC, MockDC] {
  const a = new MockDC()
  const b = new MockDC()
  configure?.(a)
  configure?.(b)
  a.peer = b
  b.peer = a
  return [a, b]
}

afterEach(() => {
  vi.useRealTimers()
})

describe('reliability autodetection (§4.3, §10.6)', () => {
  it('derives the mode from the channel configuration', () => {
    const cfg = (init: Partial<MockDC>) => Object.assign(new MockDC(), init)
    expect(channelReliable(cfg({}))).toBe(true)
    expect(channelReliable(cfg({ maxRetransmits: 0 }))).toBe(false) // even 0 is a cap
    expect(channelReliable(cfg({ maxPacketLifeTime: 100 }))).toBe(false)
    expect(channelReliable(cfg({ ordered: false }))).toBe(false)
    const transport = new DataChannelTransport(cfg({}))
    expect(transport.reliable()).toBe(true)
  })

  it('an unreliable channel puts the Conn in unreliable mode automatically', () => {
    const [a] = mockPair((dc) => (dc.maxRetransmits = 0))
    const transport = new DataChannelTransport(a)
    expect(transport.reliable()).toBe(false)
    const conn = new Conn(transport)
    expect(conn.reliable).toBe(false)
    conn.close()
  })
})

describe('reliable datachannel echo (the final-goal demo shape)', () => {
  function wireEnds() {
    const [a, b] = mockPair()
    const gateway = new DataChannelGateway()
    const server = new Server(gateway)
    const counts = registerEcho(server)
    gateway.bind(b)
    const serving = gateway.servePeer(server, b)
    const conn = new Conn(new DataChannelTransport(a)) // attachConn starts the pump
    return { a, b, conn, server, counts, serving }
  }

  it('echoes a unary call over an open channel pair, zero mode options', async () => {
    const net = wireEnds()
    net.a.open()
    net.b.open()
    expect(net.conn.reliable).toBe(true) // discovered, not configured
    const res = await net.conn.invoke(echo.once, { text: 'hello' })
    expect(res).toEqual({ text: 'echo:hello' })
    net.conn.close()
    await net.serving
  })

  it('runs all four RPC types', async () => {
    const net = wireEnds()
    net.a.open()
    net.b.open()

    expect(await net.conn.invoke(echo.once, { text: 'u' })).toEqual({ text: 'echo:u' })

    const many = net.conn.newStream(echo.many, {})
    await many.send({ text: 'm', n: 3 })
    const got: string[] = []
    for await (const m of many) got.push(m.text)
    expect(got).toEqual(['m#0', 'm#1', 'm#2'])

    const count = net.conn.newStream(echo.count, {})
    await count.send({ text: 'a' })
    await count.send({ text: 'b' })
    count.closeSend()
    expect(await count.recv()).toEqual({ text: '2' })

    const live = net.conn.newStream(echo.live, {})
    await live.send({ text: 'x' })
    expect(await live.recv()).toEqual({ text: 'echo:x' })
    live.closeSend()
    expect(await live.recv()).toBeUndefined()

    net.conn.close()
    await net.serving
  })

  it('queues sends until the channel opens (open gate)', async () => {
    const net = wireEnds()
    const p = net.conn.invoke(echo.once, { text: 'early' })
    await tick()
    expect(net.a.sent).toHaveLength(0) // gated: nothing hit the wire yet
    net.a.open()
    net.b.open()
    expect(await p).toEqual({ text: 'echo:early' })
    net.conn.close()
  })
})

describe('message size (§4.4)', () => {
  it('refuses an oversize envelop synchronously and the call fails RESOURCE_EXHAUSTED', async () => {
    const [a, b] = mockPair()
    a.open()
    b.open()
    const gateway = new DataChannelGateway()
    const server = new Server(gateway)
    registerEcho(server)
    gateway.bind(b)
    void gateway.servePeer(server, b)
    const conn = new Conn(new DataChannelTransport(a, { maxMessageSize: 128 }))
    const err = (await conn.invoke(echo.once, { text: 'x'.repeat(500) }).catch((e) => e)) as StatusError
    expect(err.code).toBe(Code.RESOURCE_EXHAUSTED)
    // The channel survives a refused send: a small call still works.
    expect(await conn.invoke(echo.once, { text: 'ok' })).toEqual({ text: 'echo:ok' })
    conn.close()
  })
})

describe('backpressure (§4.2)', () => {
  it('blocks at the buffered-amount mark and resumes on bufferedamountlow', async () => {
    const [a, b] = mockPair()
    a.open()
    b.open()
    a.bufferedAmount = 4096 // at the mark from the start
    const transport = new DataChannelTransport(a, { maxBufferedAmount: 4096 })
    expect(a.bufferedAmountLowThreshold).toBe(2048)
    let sent = false
    const p = transport.handle({ epoch: 1, sid: 1, seq: 1, flags: 8, method: '', codec: '', desc: '', peerEpoch: 0 }).then(() => (sent = true))
    await tick()
    expect(sent).toBe(false) // waiting at the mark
    a.bufferedAmount = 0
    a.emit('bufferedamountlow')
    await p
    expect(a.sent).toHaveLength(1)
  })

  it('a send stalled past the stall budget declares the channel dead (§4.2)', async () => {
    vi.useFakeTimers()
    const [a, b] = mockPair()
    void b
    // The channel never opens: the stall budget bounds the open gate.
    const gateway = void 0
    void gateway
    const conn = new Conn(new DataChannelTransport(a, { sendStallTimeoutMs: 100 }))
    const p = conn.invoke(echo.once, { text: 'x' }).catch((e) => e)
    await vi.advanceTimersByTimeAsync(200)
    const err = (await p) as StatusError
    // The stall killed the transport; the call fails (stall error or the
    // teardown's UNAVAILABLE, whichever won the race).
    expect([Code.UNKNOWN, Code.UNAVAILABLE]).toContain(err.code)
    // ...and the Conn is torn down (§4.5): new calls are refused.
    await tick()
    expect(() => conn.newStream(echo.live, {})).toThrow(/connection is closed/)
  })
})

describe('teardown duty (§4.5)', () => {
  it('channel close fails live client calls with UNAVAILABLE and cancels the server handler', async () => {
    const [a, b] = mockPair()
    a.open()
    b.open()
    const gateway = new DataChannelGateway()
    const server = new Server(gateway)
    let handlerErr: StatusError | undefined
    server.register(echo.live, async (stream) => {
      try {
        for await (const _ of stream) {
          /* consume */
        }
      } catch (e) {
        handlerErr = e as StatusError
      }
    })
    gateway.bind(b)
    const serving = gateway.servePeer(server, b)
    const conn = new Conn(new DataChannelTransport(a))

    const stream = conn.newStream(echo.live, {})
    await stream.send({ text: 'x' })
    await tick()

    b.close() // transport death, both ends
    const err = (await stream.recv().catch((e) => e)) as StatusError
    expect(err.code).toBe(Code.UNAVAILABLE)
    await serving // servePeer performed disconnectPeer on exit
    await tick()
    expect(handlerErr?.code).toBe(Code.UNAVAILABLE)
  })

  it('conn.close() tears the whole endpoint down, channel included', async () => {
    const [a, b] = mockPair()
    a.open()
    b.open()
    const conn = new Conn(new DataChannelTransport(a))
    conn.close()
    expect(a.readyState).toBe('closed') // one close reaches the socket (§4.3)
  })
})

describe('gateway (§4.3, §6.4)', () => {
  it('routes responses to the right peer and isolates disconnects', async () => {
    const gateway = new DataChannelGateway()
    const server = new Server(gateway)
    registerEcho(server)

    const [a1, b1] = mockPair()
    const [a2, b2] = mockPair()
    for (const dc of [a1, b1, a2, b2]) dc.open()
    gateway.bind(b1)
    gateway.bind(b2)
    void gateway.servePeer(server, b1)
    void gateway.servePeer(server, b2)
    const conn1 = new Conn(new DataChannelTransport(a1))
    const conn2 = new Conn(new DataChannelTransport(a2))

    const [r1, r2] = await Promise.all([conn1.invoke(echo.once, { text: 'one' }), conn2.invoke(echo.once, { text: 'two' })])
    expect(r1).toEqual({ text: 'echo:one' })
    expect(r2).toEqual({ text: 'echo:two' })

    // Killing peer 1 leaves peer 2 untouched.
    b1.close()
    await tick()
    expect(await conn2.invoke(echo.once, { text: 'still' })).toEqual({ text: 'echo:still' })
    conn1.close()
    conn2.close()
  })

  it('serves channels of differing reliability at once, each peer in its channel mode (§4.3)', async () => {
    const gateway = new DataChannelGateway()
    const server = new Server(gateway) // server-wide mode is irrelevant: per-peer annotation wins
    registerEcho(server)

    const [ra, rb] = mockPair() // reliable control channel
    const [ua, ub] = mockPair((dc) => (dc.maxRetransmits = 0)) // unreliable telemetry channel
    for (const dc of [ra, rb, ua, ub]) dc.open()
    gateway.bind(rb)
    gateway.bind(ub)
    void gateway.servePeer(server, rb)
    void gateway.servePeer(server, ub)

    const relConn = new Conn(new DataChannelTransport(ra))
    const unrelConn = new Conn(new DataChannelTransport(ua), { timing: { callMs: 5000 } })
    expect(relConn.reliable).toBe(true)
    expect(unrelConn.reliable).toBe(false)

    expect(await relConn.invoke(echo.once, { text: 'ctl' })).toEqual({ text: 'echo:ctl' })
    expect(await unrelConn.invoke(echo.once, { text: 'tlm' })).toEqual({ text: 'echo:tlm' })

    relConn.close()
    unrelConn.close()
  })

  it('a channel is served at most once', async () => {
    const gateway = new DataChannelGateway()
    const server = new Server(gateway)
    const [, b] = mockPair()
    b.open()
    void gateway.servePeer(server, b)
    await expect(gateway.servePeer(server, b)).rejects.toThrow(/already served/)
    b.close()
  })
})
