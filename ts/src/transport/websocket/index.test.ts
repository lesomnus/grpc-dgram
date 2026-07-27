// The WebSocket adapter (src/transport/websocket/index.ts) against a mock socket pair
// implementing the WhatWG WebSocket surface — no network, no `ws` package.
// Covers the reliable echo round-trip (the TS side of the Go gorilla pair),
// the §4.5 teardown duty on close and on error, the post-close delivery ban,
// the §4.4 size ceiling, and the out-of-band death signals (keepalive, a
// stalled send) that make teardown work in reliable mode, plus the one-line
// dialWebSocket path with the runtime's global WebSocket stubbed out.

import { afterEach, describe, expect, it, vi } from 'vitest'
import { Conn } from '../../conn'
import { Server } from '../../server'
import { Code, type StatusError } from '../../status'
import { echo, registerEcho, tick } from '../../testing'
import { FlagPing, frame } from '../../wire'
import { dialWebSocket, WebSocketGateway, WebSocketTransport, type WebSocketLike, type WebSocketOptions } from './index'

const CONNECTING = 0
const OPEN = 1
const CLOSED = 3

// Timers only — the mock delivers messages through a real queueMicrotask, and
// faking that would deadlock every await in these tests.
function fakeTimers(): void {
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval'] })
}

// MockWS is a WhatWG WebSocket stand-in: `send` on one end delivers an
// ArrayBuffer 'message' to the other, and close propagates to the peer.
// Constructed with keepalive: true it also grows the node-`ws` ping/pong seam,
// which the browser API does not have.
class MockWS implements WebSocketLike {
  readyState = CONNECTING
  bufferedAmount = 0
  binaryType = 'blob'
  sent: Uint8Array[] = []
  peer: MockWS | undefined
  pings = 0
  pingError: Error | undefined
  silent = false // answers no pong: a peer that stopped making progress
  ping?: (data?: unknown) => void
  on?: (type: string, listener: (...args: never[]) => void) => void
  private listeners = new Map<string, ((ev: unknown) => void)[]>()

  constructor(keepalive = false) {
    if (!keepalive) return
    this.ping = () => {
      this.pings++
      if (this.pingError !== undefined) throw this.pingError
      if (this.readyState !== OPEN) throw new Error('ping on a dead socket')
      if (this.silent || this.peer?.readyState !== OPEN) return
      queueMicrotask(() => this.emit('pong')) // the peer's stack answers
    }
    this.on = (type, fn) => this.addEventListener(type, fn)
  }

  addEventListener(type: string, fn: (ev: never) => void): void {
    const l = this.listeners.get(type) ?? []
    l.push(fn as (ev: unknown) => void)
    this.listeners.set(type, l)
  }

  emit(type: string, ev: unknown = {}): void {
    for (const fn of this.listeners.get(type) ?? []) fn(ev)
  }

  send(data: Uint8Array): void {
    if (this.readyState !== OPEN) throw new Error('InvalidStateError: socket not open')
    this.sent.push(data)
    const peer = this.peer
    if (peer !== undefined) {
      queueMicrotask(() => {
        if (peer.readyState === OPEN) peer.emit('message', { data: data.slice().buffer })
      })
    }
  }

  open(): void {
    this.readyState = OPEN
    this.emit('open')
  }

  close(code = 1000, reason = ''): void {
    if (this.readyState === CLOSED) return
    this.readyState = CLOSED
    this.emit('close', { code, reason, wasClean: code === 1000 })
    const peer = this.peer
    if (peer !== undefined && peer.readyState !== CLOSED) peer.close(code, reason)
  }
}

function mockPair(keepalive = false): [MockWS, MockWS] {
  const a = new MockWS(keepalive)
  const b = new MockWS(keepalive)
  a.peer = b
  b.peer = a
  return [a, b]
}

// wireEnds builds the whole shape: a client Conn over one end, a Server behind
// a Gateway over the other. `open: false` leaves the handshake pending.
function wireEnds(opts: { open?: boolean; keepalive?: boolean; client?: WebSocketOptions } = {}) {
  const [a, b] = mockPair(opts.keepalive)
  const gateway = new WebSocketGateway()
  const server = new Server(gateway)
  const counts = registerEcho(server)
  gateway.bind(b)
  const serving = gateway.servePeer(server, b)
  const conn = new Conn(new WebSocketTransport(a, opts.client)) // attachConn starts the pump
  if (opts.open !== false) {
    a.open()
    b.open()
  }
  return { a, b, conn, server, gateway, counts, serving }
}

afterEach(() => {
  vi.useRealTimers()
})

describe('reliable websocket echo (the gorilla pair, TS side)', () => {
  it('echoes a unary call over an open socket pair, zero mode options', async () => {
    const net = wireEnds()
    expect(net.conn.reliable).toBe(true) // discovered via TransportInfo, not configured
    expect(net.gateway.reliable()).toBe(true)
    expect(net.a.binaryType).toBe('arraybuffer') // browser-safe framing (§4.1)
    expect(await net.conn.invoke(echo.once, { text: 'hello' })).toEqual({ text: 'echo:hello' })
    expect(net.a.sent).toHaveLength(1) // one marshaled Envelop per message
    net.conn.close()
    await net.serving
  })

  it('runs all four RPC types', async () => {
    const net = wireEnds()

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

  it('queues sends until the socket opens (open gate)', async () => {
    const net = wireEnds({ open: false })
    const p = net.conn.invoke(echo.once, { text: 'early' })
    await tick()
    expect(net.a.sent).toHaveLength(0) // gated: nothing hit the wire yet
    net.a.open()
    net.b.open()
    expect(await p).toEqual({ text: 'echo:early' })
    net.conn.close()
  })

  it('carries a 256 KiB envelop: no size ceiling by default (§4.4)', async () => {
    const net = wireEnds()
    const text = 'x'.repeat(256 * 1024)
    expect(await net.conn.invoke(echo.once, { text })).toEqual({ text: `echo:${text}` })
    net.conn.close()
    await net.serving
  })
})

describe('message size (§4.4)', () => {
  it('refuses an oversize envelop synchronously and the call fails RESOURCE_EXHAUSTED', async () => {
    const net = wireEnds({ client: { maxMessageSize: 128 } })
    const err = (await net.conn.invoke(echo.once, { text: 'x'.repeat(500) }).catch((e) => e)) as StatusError
    expect(err.code).toBe(Code.RESOURCE_EXHAUSTED)
    // The socket survives a refused send: a small call still works.
    expect(await net.conn.invoke(echo.once, { text: 'ok' })).toEqual({ text: 'echo:ok' })
    net.conn.close()
  })
})

describe('teardown duty (§4.5)', () => {
  it('socket close fails live client calls with UNAVAILABLE and cancels the server handler', async () => {
    const [a, b] = mockPair()
    const gateway = new WebSocketGateway()
    const server = new Server(gateway)
    let handlerErr: StatusError | undefined
    server.register(echo.live, async (stream) => {
      try {
        for await (const _ of stream) {
          /* consume until the peer dies */
        }
      } catch (e) {
        handlerErr = e as StatusError
      }
    })
    gateway.bind(b)
    const serving = gateway.servePeer(server, b)
    const conn = new Conn(new WebSocketTransport(a))
    a.open()
    b.open()

    const stream = conn.newStream(echo.live, {})
    await stream.send({ text: 'x' })
    await tick()

    b.close() // transport death, both ends
    const err = (await stream.recv().catch((e) => e)) as StatusError
    expect(err.code).toBe(Code.UNAVAILABLE)
    expect(await serving).toBeUndefined() // an orderly close carries no cause
    await tick()
    expect(handlerErr?.code).toBe(Code.UNAVAILABLE) // servePeer ran disconnectPeer on exit
  })

  it('an abnormal close carries its cause into the teardown', async () => {
    const net = wireEnds()
    const stream = net.conn.newStream(echo.live, {})
    await stream.send({ text: 'x' })
    expect(await stream.recv()).toEqual({ text: 'echo:x' })

    net.a.close(1006, 'connection reset')
    const err = (await stream.recv().catch((e) => e)) as StatusError
    expect(err.code).toBe(Code.UNAVAILABLE)
    expect(err.message).toMatch(/1006/)
    const cause = (await net.serving) as Error
    expect(cause.message).toMatch(/1006/) // servePeer resolves with the death cause
  })

  it('an error event tears the endpoint down with its cause', async () => {
    const [a] = mockPair()
    const conn = new Conn(new WebSocketTransport(a))
    a.open()
    const p = conn.invoke(echo.once, { text: 'x' }).catch((e) => e)
    await tick()
    a.emit('error', { error: new Error('boom') })
    const err = (await p) as StatusError
    expect(err.code).toBe(Code.UNAVAILABLE)
    expect(err.message).toMatch(/boom/)
    await tick()
    // ...and the Conn is torn down: new calls are refused.
    expect(() => conn.newStream(echo.live, {})).toThrow(/connection is closed/)
    expect(a.readyState).toBe(CLOSED) // the teardown reaches the socket
  })

  it('an error event with no detail still tears the endpoint down', async () => {
    const [a] = mockPair()
    const conn = new Conn(new WebSocketTransport(a))
    a.open()
    const p = conn.invoke(echo.once, { text: 'x' }).catch((e) => e)
    await tick()
    a.emit('error', {}) // the browser Event carries nothing
    expect(((await p) as StatusError).code).toBe(Code.UNAVAILABLE)
  })

  it('a socket already dead when wrapped tears down at once', async () => {
    const [a] = mockPair()
    a.readyState = CLOSED // the close event fired before the adapter existed
    const conn = new Conn(new WebSocketTransport(a))
    // Racing the teardown or after it, the call fails the same way.
    const racing = (await conn.invoke(echo.once, { text: 'x' }).catch((e) => e)) as StatusError
    expect(racing.code).toBe(Code.UNAVAILABLE)
    await tick()
    expect(() => conn.newStream(echo.live, {})).toThrow(/connection is closed/)
  })

  it('conn.close() tears the whole endpoint down, socket included', async () => {
    const net = wireEnds()
    net.conn.close()
    expect(net.a.readyState).toBe(CLOSED) // one close reaches the socket
    expect(await net.serving).toBeUndefined() // and the peer end saw it
  })
})

describe('nothing is delivered after close (§4.5)', () => {
  it('a message arriving after the socket died is never handed to the core', async () => {
    const net = wireEnds()
    expect(await net.conn.invoke(echo.once, { text: 'live' })).toEqual({ text: 'echo:live' })
    expect(net.counts.once).toBe(1)
    const replay = net.a.sent[0] // the OPEN envelop the server already handled
    expect(replay).toBeDefined()

    const spy = vi.spyOn(net.server, 'handle')
    net.b.close()
    await net.serving

    net.b.emit('message', { data: replay!.slice().buffer }) // late arrival
    await tick()
    expect(spy).not.toHaveBeenCalled()
    expect(net.counts.once).toBe(1) // the handler did not run a second time
    net.conn.close()
  })

  it('a client that dies mid-flight delivers nothing further to its Conn', async () => {
    const [a] = mockPair()
    const transport = new WebSocketTransport(a)
    const conn = { handle: vi.fn(), close: vi.fn() }
    transport.attachConn(conn as unknown as Conn)
    a.open()
    await tick()

    a.close()
    await tick()
    expect(conn.close).toHaveBeenCalledTimes(1) // the §4.5 teardown, exactly once
    a.emit('message', { data: new Uint8Array([0x0a, 0x00]).buffer }) // a well-formed envelop
    await tick()
    expect(conn.handle).not.toHaveBeenCalled()
  })
})

describe('gateway (§6.4)', () => {
  it('routes responses to the right peer and isolates disconnects', async () => {
    const gateway = new WebSocketGateway()
    const server = new Server(gateway)
    registerEcho(server)

    const [a1, b1] = mockPair()
    const [a2, b2] = mockPair()
    gateway.bind(b1)
    gateway.bind(b2)
    void gateway.servePeer(server, b1)
    void gateway.servePeer(server, b2)
    const conn1 = new Conn(new WebSocketTransport(a1))
    const conn2 = new Conn(new WebSocketTransport(a2))
    for (const ws of [a1, b1, a2, b2]) ws.open()

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

  it('serves a socket at most once and refuses a peer that is gone', async () => {
    const gateway = new WebSocketGateway()
    const server = new Server(gateway)
    const ping = frame({ epoch: 1, flags: FlagPing })
    const [, b] = mockPair()
    b.open()
    const serving = gateway.servePeer(server, b)
    await expect(gateway.servePeer(server, b)).rejects.toThrow(/already served/)
    b.close()
    await serving
    await expect(gateway.handle(ping, { peer: 1 })).rejects.toThrow(/disconnected/)
    await expect(gateway.handle(ping)).rejects.toThrow(/no gateway peer/)
  })

  it('gateway.close() tears every served socket down', async () => {
    const gateway = new WebSocketGateway()
    const server = new Server(gateway)
    registerEcho(server)
    const [a, b] = mockPair()
    gateway.bind(b)
    const serving = gateway.servePeer(server, b)
    const conn = new Conn(new WebSocketTransport(a))
    a.open()
    b.open()
    expect(await conn.invoke(echo.once, { text: 'hi' })).toEqual({ text: 'echo:hi' })

    gateway.close()
    expect(b.readyState).toBe(CLOSED)
    await serving
    await tick()
    const err = (await conn.invoke(echo.once, { text: 'gone' }).catch((e) => e)) as StatusError
    expect(err.code).toBe(Code.UNAVAILABLE)
  })
})

describe('keepalive (§10.6, the out-of-band death signal)', () => {
  const ka: WebSocketOptions = { keepaliveIntervalMs: 1000, keepaliveTimeoutMs: 2500 }

  it('pings on a cadence and rides out an answering peer', async () => {
    fakeTimers()
    const net = wireEnds({ keepalive: true, client: ka })
    const stream = net.conn.newStream(echo.live, {})
    await stream.send({ text: 'x' })
    expect(await stream.recv()).toEqual({ text: 'echo:x' })

    await vi.advanceTimersByTimeAsync(10_000) // ten keepalive rounds, all answered
    expect(net.a.pings).toBeGreaterThanOrEqual(9)

    await stream.send({ text: 'y' }) // the call survived the idle stretch
    expect(await stream.recv()).toEqual({ text: 'echo:y' })
    net.conn.close()
  })

  it('declares a peer with no read progress dead and runs the teardown', async () => {
    fakeTimers()
    const net = wireEnds({ keepalive: true, client: ka })
    const stream = net.conn.newStream(echo.live, {})
    await stream.send({ text: 'x' })
    expect(await stream.recv()).toEqual({ text: 'echo:x' })

    net.a.silent = true // pings leave, nothing ever comes back
    await vi.advanceTimersByTimeAsync(5000)
    const err = (await stream.recv().catch((e) => e)) as StatusError
    expect(err.code).toBe(Code.UNAVAILABLE)
    expect(err.message).toMatch(/no read progress/)
    expect(net.a.readyState).toBe(CLOSED)
  })

  it('a ping the socket cannot carry is transport death', async () => {
    fakeTimers()
    const net = wireEnds({ keepalive: true, client: ka })
    const stream = net.conn.newStream(echo.live, {})
    await stream.send({ text: 'x' })
    expect(await stream.recv()).toEqual({ text: 'echo:x' })

    net.a.pingError = new Error('broken pipe')
    await vi.advanceTimersByTimeAsync(1500)
    const err = (await stream.recv().catch((e) => e)) as StatusError
    expect(err.code).toBe(Code.UNAVAILABLE)
    expect(err.message).toMatch(/keepalive ping/)
  })

  it('is off where the runtime exposes no ping/pong (the browser)', async () => {
    fakeTimers()
    const net = wireEnds({ client: ka }) // a mock without ping/on
    await vi.advanceTimersByTimeAsync(60_000) // idle far past the timeout
    expect(net.a.pings).toBe(0)
    expect(await net.conn.invoke(echo.once, { text: 'still here' })).toEqual({ text: 'echo:still here' })
    net.conn.close()
  })
})

describe('send gating (§4.2)', () => {
  const ping = frame({ epoch: 1, flags: FlagPing })

  it('parks at the buffered-amount mark and resumes when the socket drains', async () => {
    const [a] = mockPair()
    a.open()
    a.bufferedAmount = 4096 // at the mark from the start
    const transport = new WebSocketTransport(a, { maxBufferedAmount: 4096 })
    let sent = false
    const p = transport.handle(ping).then(() => (sent = true))
    await tick()
    expect(sent).toBe(false) // parked
    a.bufferedAmount = 0
    await p
    expect(a.sent).toHaveLength(1)
  })

  it('a send stalled past the budget declares the socket dead (§4.2)', async () => {
    fakeTimers()
    const [a] = mockPair()
    // The socket never opens: the stall budget bounds the open gate.
    const conn = new Conn(new WebSocketTransport(a, { sendStallTimeoutMs: 100 }))
    const p = conn.invoke(echo.once, { text: 'x' }).catch((e) => e)
    await vi.advanceTimersByTimeAsync(200)
    const err = (await p) as StatusError
    // The stall killed the transport; the call fails (the stall error or the
    // teardown's UNAVAILABLE, whichever won the race).
    expect([Code.UNKNOWN, Code.UNAVAILABLE]).toContain(err.code)
    await tick()
    expect(() => conn.newStream(echo.live, {})).toThrow(/connection is closed/)
    expect(a.readyState).toBe(CLOSED)
  })
})

describe('dialWebSocket (the one-line client path)', () => {
  // The runtime's global WebSocket, stubbed: a constructor may return an
  // object of its own, so `new` on this hands back a mock end instead of
  // opening a real socket.
  function withGlobalWS<T>(ws: MockWS | undefined, fn: (seen: () => { url: string; protocols?: string | string[] } | undefined) => T): T {
    const g = globalThis as { WebSocket?: unknown }
    const had = Object.hasOwn(g, 'WebSocket')
    const saved = g.WebSocket
    let seen: { url: string; protocols?: string | string[] } | undefined
    if (ws === undefined) delete g.WebSocket
    else {
      g.WebSocket = function (url: string, protocols?: string | string[]) {
        seen = { url, protocols }
        return ws
      }
    }
    try {
      return fn(() => seen)
    } finally {
      if (had) g.WebSocket = saved
      else delete g.WebSocket
    }
  }

  it('hands back a Conn, ready to call, with one options bag split three ways', async () => {
    const [a, b] = mockPair()
    const gateway = new WebSocketGateway()
    const server = new Server(gateway)
    registerEcho(server)
    gateway.bind(b)
    const serving = gateway.servePeer(server, b)

    // `protocols` is the socket's, maxMessageSize the adapter's (§4.4),
    // reliable the Conn's (§4.3): one bag, three consumers, no key in common.
    const conn = withGlobalWS(a, (seen) => {
      const c = dialWebSocket('wss://host/rpc', { protocols: 'drpc', maxMessageSize: 1 << 20, reliable: true })
      expect(seen()).toEqual({ url: 'wss://host/rpc', protocols: 'drpc' })
      return c
    })
    expect(conn.reliable).toBe(true)

    // It returns before the handshake: the call queues rather than fails.
    const early = conn.invoke(echo.once, { text: 'hi' })
    a.open()
    b.open()
    expect(await early).toEqual({ text: 'echo:hi' })

    conn.close() // one close: the Conn, the transport, the socket
    await serving
    expect(a.readyState).toBe(CLOSED)
  })

  it('names the way out where the runtime has no global WebSocket', () => {
    withGlobalWS(undefined, () => {
      expect(() => dialWebSocket('wss://host/rpc')).toThrow(/no global WebSocket/)
    })
  })
})
