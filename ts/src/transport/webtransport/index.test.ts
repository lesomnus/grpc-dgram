// The WebTransport datagram adapter (src/transport/webtransport/index.ts)
// against a mock session pair implementing the surface the adapter names —
// no network, no browser. There is no TS server in this step, so the far end
// is a Server over a hand-built datagram pump standing in for the Go
// gateway. Covers the unreliable echo round-trip (the TS side of the Go
// transport/webtransport pair), the mode discovery that turns the core's
// timers on, the §4.4 size ceiling from each of its three sources, the §4.5
// teardown from each way a session ends, the post-close delivery ban, and
// the one-line dialWebTransport path with the runtime's global WebTransport
// stubbed out — and the datagram sink in both shapes engines ship,
// createWritable() and `writable`.

import { afterEach, describe, expect, it, vi } from 'vitest'
import { Conn, type ConnOptions } from '../../conn'
import { Server } from '../../server'
import { unpack } from '../../seam'
import { Code, MessageTooLargeError, type StatusError } from '../../status'
import { echo, registerEcho, tick } from '../../testing'
import { noop } from '../../util'
import { decodeEnvelop, encodeEnvelop, FlagOpen, FlagPing, frame, type Frame } from '../../wire'
import { DefaultMaxMessageSize, dialWebTransport, WebTransportDatagramTransport, type WebTransportDatagramOptions, type WebTransportDialOptions, type WebTransportLike } from './index'

// The platform's own WebTransport fits the structural type as the DOM lib
// declares it, and the dial options fit the DOM's constructor options —
// checked by tsc, never run.
const domFits = (wt: WebTransport): WebTransportLike => wt
void domFits
const dialFits = (o: WebTransportDialOptions): WebTransportOptions => o
void dialFits

// Timers only — the mock delivers through a real queueMicrotask and the
// streams settle on real microtasks; faking those would deadlock every await.
function fakeTimers(): void {
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval'] })
}

interface CloseInfo {
  closeCode?: number
  reason?: string
}

// SinkShape is how a mock exposes its datagram sink: the `writable`
// attribute (Chromium) or the spec's createWritable() (WebKit).
type SinkShape = 'writable' | 'createWritable'

// MockWebTransport is a WebTransport stand-in with the lifecycle under the
// test's hand: connect() settles `ready`; close(info) is the graceful path —
// `closed` resolves, the readable closes, and the peer sees the same close,
// as the WT_CLOSE_SESSION capsule carries it; abort(err) the abrupt one —
// `closed` rejects, and `ready` too while still connecting. A datagram
// written on one end is enqueued on the peer's readable a microtask later; an
// oversize one is dropped silently, as the platform's writeDatagrams does, so
// the adapter must refuse it before the write.
class MockWebTransport implements WebTransportLike {
  readonly ready: Promise<void>
  readonly closed: Promise<unknown>
  readonly datagrams: { readonly readable: ReadableStream<Uint8Array>; writable?: WritableStream<Uint8Array>; createWritable?: () => WritableStream<Uint8Array>; maxDatagramSize?: number }
  readonly sink: WritableStream<Uint8Array> // the datagram sink behind whichever shape datagrams exposes
  state: 'connecting' | 'connected' | 'closed' | 'failed' = 'connecting'
  sent: Uint8Array[] = [] // every chunk the writable sink received, oversize included
  peer: MockWebTransport | undefined
  silent = false // delivers nothing to the peer: datagrams that vanish on the path
  closeInfo: CloseInfo | undefined // what close() was called with
  private settleReady!: { res: () => void; rej: (e: unknown) => void }
  private settleClosed!: { res: (v: unknown) => void; rej: (e: unknown) => void }
  private rx!: ReadableStreamDefaultController<Uint8Array>

  constructor(maxDatagramSize?: number, shape: SinkShape = 'writable') {
    this.ready = new Promise<void>((res, rej) => {
      this.settleReady = { res, rej }
    })
    this.closed = new Promise<unknown>((res, rej) => {
      this.settleClosed = { res, rej }
    })
    // A mock nobody wraps must not leak an unhandled rejection; the platform
    // marks these promises handled the same way.
    this.ready.catch(noop)
    this.closed.catch(noop)
    const readable = new ReadableStream<Uint8Array>({
      start: (c) => {
        this.rx = c
      },
    })
    this.sink = new WritableStream<Uint8Array>({ write: (chunk) => this.onWrite(chunk) })
    this.datagrams =
      shape === 'writable'
        ? { readable, writable: this.sink, maxDatagramSize }
        : {
            readable,
            maxDatagramSize,
            // The spec's shape: no `writable` at all, and createWritable()
            // throws InvalidStateError once the session is closed or failed.
            createWritable: () => {
              if (this.state === 'closed' || this.state === 'failed') throw new Error('InvalidStateError: the session is closed')
              return this.sink
            },
          }
  }

  private onWrite(chunk: Uint8Array): void {
    this.sent.push(chunk)
    const max = this.datagrams.maxDatagramSize
    if (max !== undefined && chunk.length > max) return // the platform's silent drop
    const peer = this.peer
    if (peer === undefined || this.silent) return
    const copy = chunk.slice()
    queueMicrotask(() => peer.deliver(copy))
  }

  connect(): void {
    if (this.state !== 'connecting') return
    this.state = 'connected'
    this.settleReady.res()
  }

  // deliver enqueues a datagram as if it arrived from the network.
  deliver(data: Uint8Array): void {
    if (this.state !== 'connected') return
    this.rx.enqueue(data)
  }

  close(info: CloseInfo = {}): void {
    if (this.state === 'closed' || this.state === 'failed') return
    this.closeInfo = info
    if (this.state === 'connecting') {
      // close() before the session is established is an abort: `ready` and
      // `closed` both reject (the spec's cleanup with an AbortError).
      this.abort(new Error('webtransport: session aborted while connecting'))
      return
    }
    this.state = 'closed'
    this.settleClosed.res({ closeCode: info.closeCode ?? 0, reason: info.reason ?? '' })
    try {
      this.rx.close()
    } catch {
      // already cancelled by the adapter
    }
    const peer = this.peer
    if (peer !== undefined && peer.state === 'connected') peer.close(info)
  }

  abort(err: unknown): void {
    if (this.state === 'closed' || this.state === 'failed') return
    const connecting = this.state === 'connecting'
    this.state = 'failed'
    if (connecting) this.settleReady.rej(err)
    this.settleClosed.rej(err)
    try {
      this.rx.error(err)
    } catch {
      // already cancelled by the adapter
    }
  }
}

function mockPair(maxDatagramSize?: number, shape?: SinkShape): [MockWebTransport, MockWebTransport] {
  const a = new MockWebTransport(maxDatagramSize, shape)
  const b = new MockWebTransport(maxDatagramSize, shape)
  a.peer = b
  b.peer = a
  return [a, b]
}

// serveMock stands in for the Go gateway on the far end of a pair: a Server
// whose tx writes one envelop per datagram, fed by a pump over the mock's
// readable under a fixed peer key and the mode annotation (§4.3).
function serveMock(wt: MockWebTransport, reliable = false) {
  const writer = wt.sink.getWriter()
  const server = new Server({ handle: (f: Frame) => writer.write(encodeEnvelop([f])).catch(noop) }, { reliable })
  const counts = registerEcho(server)
  const serving = (async () => {
    const reader = wt.datagrams.readable.getReader()
    for (;;) {
      const { done, value } = await reader.read()
      if (done) return
      if (value === undefined) continue
      let frames: Frame[]
      try {
        frames = decodeEnvelop(value)
      } catch {
        continue
      }
      void unpack(frames, server, { peer: 'client', reliable })
    }
  })().catch(noop) // an errored readable (abrupt close) ends the pump the same way
  return { server, counts, serving }
}

// wireEnds builds the whole shape: a client Conn over one end, a Server over
// the other. `connect: false` leaves the handshake pending.
function wireEnds(opts: { connect?: boolean; maxDatagramSize?: number; client?: WebTransportDatagramOptions; conn?: ConnOptions } = {}) {
  const [a, b] = mockPair(opts.maxDatagramSize)
  const far = serveMock(b)
  const conn = new Conn(new WebTransportDatagramTransport(a, opts.client), opts.conn) // attachConn starts the pump
  if (opts.connect !== false) {
    a.connect()
    b.connect()
  }
  return { a, b, conn, ...far }
}

// bigFrame is an OPEN whose envelop is at least n bytes.
function bigFrame(n: number): Frame {
  return frame({ epoch: 1, sid: 1, flags: FlagOpen, method: '/test.Echo/Once', payload: new Uint8Array(n) })
}

afterEach(() => {
  vi.useRealTimers()
})

describe('unreliable webtransport echo (the Go transport/webtransport pair, TS side)', () => {
  it('echoes a unary call over a connected session pair, zero mode options', async () => {
    const net = wireEnds()
    expect(net.conn.reliable).toBe(false) // discovered via TransportInfo, not configured
    expect(await net.conn.invoke(echo.once, { text: 'hello' })).toEqual({ text: 'echo:hello' })
    expect(net.a.sent.length).toBeGreaterThanOrEqual(1)
    for (const d of net.a.sent) expect(() => decodeEnvelop(d)).not.toThrow() // one marshaled Envelop per datagram
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

  it('queues sends until the session is ready (the open gate)', async () => {
    const net = wireEnds({ connect: false })
    const p = net.conn.invoke(echo.once, { text: 'early' })
    await tick()
    expect(net.a.sent).toHaveLength(0) // gated: nothing hit the platform yet
    net.a.connect()
    net.b.connect()
    expect(await p).toEqual({ text: 'echo:early' })
    net.conn.close()
  })

  it('unreliable mode is discovered: a vanished peer is bounded by the core, and the session survives it', async () => {
    fakeTimers()
    const net = wireEnds({ conn: { timing: { callMs: 1000, retransmitMs: 100 } } })
    expect(net.conn.reliable).toBe(false)
    net.a.silent = true // every datagram the client sends goes nowhere
    const p = net.conn.invoke(echo.once, { text: 'lost' }).catch((e) => e)
    await vi.advanceTimersByTimeAsync(1500)
    const err = (await p) as StatusError
    expect(err.code).toBe(Code.DEADLINE_EXCEEDED) // T_call, the core's timer (§10.2)
    expect(net.a.sent.length).toBeGreaterThan(1) // retransmitted meanwhile (§10.3): the timer machinery is on
    expect(net.a.state).toBe('connected') // loss is not death: the adapter tore nothing down
    net.a.silent = false
    expect(await net.conn.invoke(echo.once, { text: 'back' })).toEqual({ text: 'echo:back' })
    net.conn.close()
  })
})

describe('message size (§4.4)', () => {
  it('refuses an oversize envelop synchronously — nothing reaches the platform — and the call fails RESOURCE_EXHAUSTED', async () => {
    const net = wireEnds({ client: { maxMessageSize: 128 } })
    const err = (await net.conn.invoke(echo.once, { text: 'x'.repeat(500) }).catch((e) => e)) as StatusError
    expect(err.code).toBe(Code.RESOURCE_EXHAUSTED)
    expect(err.message).toMatch(/128-byte limit/)
    expect(net.a.sent).toHaveLength(0) // refused before the write, not dropped by the platform after it
    // The session survives a refused send: a small call still works.
    expect(await net.conn.invoke(echo.once, { text: 'ok' })).toEqual({ text: 'echo:ok' })
    net.conn.close()
  })

  it("defaults to the session's maxDatagramSize", async () => {
    const [a] = mockPair(64)
    const transport = new WebTransportDatagramTransport(a)
    a.connect()
    await a.ready // the session's word counts once it is up
    expect(() => transport.handle(bigFrame(100))).toThrow(MessageTooLargeError)
    expect(() => transport.handle(bigFrame(100))).toThrow(/64-byte limit/)
    expect(a.sent).toHaveLength(0)
    await transport.handle(frame({ epoch: 1, flags: FlagPing }))
    expect(a.sent).toHaveLength(1)
    transport.close()
  })

  it('falls back to 1200 bytes when the session reports none', async () => {
    expect(DefaultMaxMessageSize).toBe(1200)
    const [a] = mockPair() // no maxDatagramSize at all
    const transport = new WebTransportDatagramTransport(a)
    a.connect()
    await a.ready
    expect(() => transport.handle(bigFrame(1300))).toThrow(/1200-byte limit/)
    await transport.handle(bigFrame(1000))
    expect(a.sent).toHaveLength(1)
    transport.close()
  })

  it("follows the session's ceiling as it changes, and the explicit option overrides it", async () => {
    // A browser reports a placeholder before `ready` and the path's real
    // ceiling after; the adapter reads it at each send.
    const [a] = mockPair(64)
    const transport = new WebTransportDatagramTransport(a)
    a.connect()
    await a.ready
    expect(() => transport.handle(bigFrame(100))).toThrow(/64-byte limit/)
    a.datagrams.maxDatagramSize = 2048
    await transport.handle(bigFrame(100))
    expect(a.sent).toHaveLength(1)
    transport.close()

    const [c] = mockPair(64)
    const fixed = new WebTransportDatagramTransport(c, { maxMessageSize: 4096 })
    c.connect()
    await fixed.handle(bigFrame(100)) // the option is the ceiling; what the platform then does with it is its own
    expect(c.sent).toHaveLength(1)
    fixed.close()
  })

  it('a call issued while connecting is judged against the default, not the placeholder the platform reports before `ready`', async () => {
    // Chromium reports 1024 until the handshake and the path's real ceiling
    // (~1200) after: a 1.1 KB request on the dial tick must queue, not fail,
    // and go out under the ceiling in force once the session is up.
    const net = wireEnds({ connect: false, maxDatagramSize: 1024 })
    const text = 'x'.repeat(1100)
    const p = net.conn.invoke(echo.once, { text })
    await tick()
    expect(net.a.sent).toHaveLength(0) // queued at the gate, not refused
    net.a.datagrams.maxDatagramSize = 1300
    net.b.datagrams.maxDatagramSize = 1300
    net.a.connect()
    net.b.connect()
    expect(await p).toEqual({ text: `echo:${text}` })
    expect(net.a.sent).toHaveLength(1)
    expect(net.a.sent[0]!.length).toBeGreaterThan(1024) // over the placeholder, under the ceiling it met
    net.conn.close()
  })

  it('once up, the ceiling in force at the write is the judge: a queued envelop over it is refused, and nothing reaches the platform', async () => {
    const net = wireEnds({ connect: false, conn: { timing: { callMs: 300 } } })
    const p = net.conn.invoke(echo.once, { text: 'x'.repeat(1100) }).catch((e) => e)
    await tick()
    expect(net.a.sent).toHaveLength(0) // under the default while connecting
    net.a.datagrams.maxDatagramSize = 1050 // the path turned out narrower than the default
    net.a.connect()
    net.b.connect()
    const err = (await p) as StatusError
    expect(err.code).toBe(Code.RESOURCE_EXHAUSTED) // not a silent drop and a deadline
    expect(err.message).toMatch(/1050-byte limit/)
    expect(net.a.sent).toHaveLength(0) // refused at the write, before the platform
    net.conn.close()
  })
})

describe('the datagram sink (createWritable() or `writable`, whichever the engine ships)', () => {
  it('writes through createWritable() where the session offers it (WebKit, Gecko)', async () => {
    const [a, b] = mockPair(undefined, 'createWritable')
    expect(a.datagrams.writable).toBeUndefined() // Safari-shaped: no `writable` at all
    const far = serveMock(b)
    const conn = new Conn(new WebTransportDatagramTransport(a))
    a.connect()
    b.connect()
    expect(await conn.invoke(echo.once, { text: 'hi' })).toEqual({ text: 'echo:hi' })
    expect(a.sent.length).toBeGreaterThanOrEqual(1)
    conn.close()
    await far.serving
  })

  it('a session already dead when wrapped, offering only createWritable(), tears down with its refusal as the cause', async () => {
    const [a] = mockPair(undefined, 'createWritable')
    a.abort(new Error('refused'))
    const conn = new Conn(new WebTransportDatagramTransport(a)) // createWritable() throws: a death, not a construction failure
    const err = (await conn.invoke(echo.once, { text: 'x' }).catch((e) => e)) as StatusError
    expect(err.code).toBe(Code.UNAVAILABLE)
    expect(err.message).toMatch(/InvalidStateError/)
    await tick()
    expect(() => conn.newStream(echo.live, {})).toThrow(/connection is closed/)
  })

  it('a session with neither shape is refused at construction', () => {
    const [a] = mockPair()
    const bare = { ready: a.ready, closed: a.closed, datagrams: { readable: a.datagrams.readable }, close: () => a.close() } as WebTransportLike
    expect(() => new WebTransportDatagramTransport(bare)).toThrow(/neither datagrams\.createWritable\(\) nor datagrams\.writable/)
  })
})

describe('teardown duty (§4.5)', () => {
  it('a graceful close by the peer fails live client calls with UNAVAILABLE', async () => {
    const net = wireEnds()
    const stream = net.conn.newStream(echo.live, {})
    await stream.send({ text: 'x' })
    expect(await stream.recv()).toEqual({ text: 'echo:x' })

    net.b.close() // the peer's WT_CLOSE_SESSION: `closed` resolves on this end
    const err = (await stream.recv().catch((e) => e)) as StatusError
    expect(err.code).toBe(Code.UNAVAILABLE)
    expect(err.message).toBe('UNAVAILABLE: transport closed') // code 0, no reason: no detail to carry
    await tick()
    expect(() => net.conn.newStream(echo.live, {})).toThrow(/connection is closed/)
    expect(net.a.state).toBe('closed') // the teardown reaches the session
  })

  it('a graceful close with an application code carries it into the teardown', async () => {
    const net = wireEnds()
    const stream = net.conn.newStream(echo.live, {})
    await stream.send({ text: 'x' })
    expect(await stream.recv()).toEqual({ text: 'echo:x' })

    net.b.close({ closeCode: 42, reason: 'bye' })
    const err = (await stream.recv().catch((e) => e)) as StatusError
    expect(err.code).toBe(Code.UNAVAILABLE)
    expect(err.message).toMatch(/code 42: bye/)
  })

  it('an abrupt close (`closed` rejects) carries its cause', async () => {
    const net = wireEnds()
    const stream = net.conn.newStream(echo.live, {})
    await stream.send({ text: 'x' })
    expect(await stream.recv()).toEqual({ text: 'echo:x' })

    net.a.abort(new Error('connection reset'))
    const err = (await stream.recv().catch((e) => e)) as StatusError
    expect(err.code).toBe(Code.UNAVAILABLE)
    expect(err.message).toMatch(/connection reset/)
    await tick()
    expect(() => net.conn.newStream(echo.live, {})).toThrow(/connection is closed/)
  })

  it('a failed handshake (`ready` rejects) tears the endpoint down with its cause', async () => {
    const net = wireEnds({ connect: false })
    const p = net.conn.invoke(echo.once, { text: 'x' }).catch((e) => e)
    await tick()
    expect(net.a.sent).toHaveLength(0) // parked on the gate
    net.a.abort(new Error('handshake failed'))
    const err = (await p) as StatusError
    expect(err.code).toBe(Code.UNAVAILABLE) // the parked send fails with the teardown's own code
    expect(err.message).toMatch(/handshake failed/)
    await tick()
    expect(() => net.conn.newStream(echo.live, {})).toThrow(/connection is closed/)
  })

  it('a session already dead when wrapped tears down at once', async () => {
    const [a] = mockPair()
    a.abort(new Error('refused')) // it failed before the adapter existed
    const conn = new Conn(new WebTransportDatagramTransport(a))
    // Racing the teardown or after it, the call fails the same way.
    const racing = (await conn.invoke(echo.once, { text: 'x' }).catch((e) => e)) as StatusError
    expect(racing.code).toBe(Code.UNAVAILABLE)
    expect(racing.message).toMatch(/refused/)
    await tick()
    expect(() => conn.newStream(echo.live, {})).toThrow(/connection is closed/)
  })

  it('conn.close() tears the whole endpoint down, session included', async () => {
    const net = wireEnds()
    expect(await net.conn.invoke(echo.once, { text: 'hi' })).toEqual({ text: 'echo:hi' })
    net.conn.close()
    expect(net.a.state).toBe('closed') // one close reaches the session
    expect(net.a.closeInfo).toEqual({}) // with no close info: code 0
    expect(net.b.state).toBe('closed') // and the peer end saw it
    await net.serving
  })

  it('conn.close() before the handshake aborts it', async () => {
    const net = wireEnds({ connect: false })
    const p = net.conn.invoke(echo.once, { text: 'x' }).catch((e) => e)
    net.conn.close()
    expect(((await p) as StatusError).code).toBe(Code.UNAVAILABLE)
    expect(net.a.state).toBe('failed') // a session closed while connecting never establishes
  })
})

describe('nothing is delivered after close (§4.5)', () => {
  it('a datagram queued when the session died is never handed to the Conn', async () => {
    const [a] = mockPair()
    const transport = new WebTransportDatagramTransport(a)
    const conn = { handle: vi.fn(), close: vi.fn() }
    transport.attachConn(conn as unknown as Conn)
    a.connect()
    await tick()

    const env = encodeEnvelop([frame({ epoch: 1, flags: FlagPing })])
    a.deliver(env)
    await tick()
    expect(conn.handle).toHaveBeenCalledTimes(1) // a live session delivers

    // A burst, then the close, in one turn: the first datagram is read before
    // the session's `closed` settles, the second after — the adapter must
    // stop at the death latch, not drain the queue.
    a.deliver(env)
    a.deliver(env)
    a.close()
    await tick()
    expect(conn.close).toHaveBeenCalledTimes(1) // the §4.5 teardown, exactly once
    expect(conn.handle).toHaveBeenCalledTimes(2)
    a.deliver(env) // a dead mock delivers nothing either way
    await tick()
    expect(conn.handle).toHaveBeenCalledTimes(2)
  })

  it('a late datagram the peer sent before it saw the close is not delivered', async () => {
    const net = wireEnds()
    expect(await net.conn.invoke(echo.once, { text: 'live' })).toEqual({ text: 'echo:live' })
    expect(net.counts.once).toBe(1)
    const replay = net.a.sent[0] // the OPEN the server already handled
    expect(replay).toBeDefined()

    const spy = vi.spyOn(net.server, 'handle')
    net.b.close()
    await net.serving
    net.b.deliver(replay!.slice()) // the mock refuses it: the readable closed with the session
    await tick()
    expect(spy).not.toHaveBeenCalled()
    expect(net.counts.once).toBe(1) // the handler did not run a second time
    net.conn.close()
  })
})

describe('dialWebTransport (the one-line client path)', () => {
  interface Seen {
    url: string
    options?: unknown
  }

  // The runtime's global WebTransport, stubbed: a constructor may return an
  // object of its own, so `new` on this hands back a mock end instead of
  // opening a real session.
  function withGlobalWT<T>(wt: MockWebTransport | undefined, fn: (seen: () => Seen | undefined) => T): T {
    const g = globalThis as { WebTransport?: unknown }
    const had = Object.hasOwn(g, 'WebTransport')
    const saved = g.WebTransport
    let seen: Seen | undefined
    if (wt === undefined) delete g.WebTransport
    else {
      g.WebTransport = function (url: string, options?: unknown) {
        seen = { url, options }
        return wt
      }
    }
    try {
      return fn(() => seen)
    } finally {
      if (had) g.WebTransport = saved
      else delete g.WebTransport
    }
  }

  it('hands back a Conn, ready to call, with one options bag split three ways', async () => {
    const [a, b] = mockPair()
    const far = serveMock(b, true)

    // The certificate pin is the session's, maxMessageSize the adapter's
    // (§4.4), reliable the Conn's (§4.3): one bag, three consumers, no key in
    // common. requireUnreliable is on unless switched off — datagrams are
    // what this adapter is made of.
    const hash = { algorithm: 'sha-256', value: new Uint8Array(32) }
    const conn = withGlobalWT(a, (seen) => {
      const c = dialWebTransport('https://host:4433/rpc', { serverCertificateHashes: [hash], maxMessageSize: 1 << 20, reliable: true })
      expect(seen()).toEqual({ url: 'https://host:4433/rpc', options: { requireUnreliable: true, serverCertificateHashes: [hash] } })
      return c
    })
    expect(conn.reliable).toBe(true)

    // It returns before the handshake: the call queues rather than fails.
    const early = conn.invoke(echo.once, { text: 'hi' })
    a.connect()
    b.connect()
    expect(await early).toEqual({ text: 'echo:hi' })

    conn.close() // one close: the Conn, the transport, the session
    await far.serving
    expect(a.state).toBe('closed')
  })

  it('passes the session options through as given', () => {
    const [a] = mockPair()
    withGlobalWT(a, (seen) => {
      const c = dialWebTransport('https://host/rpc', { requireUnreliable: false, allowPooling: true, congestionControl: 'low-latency', protocols: ['drpc'] })
      expect(seen()?.options).toEqual({ requireUnreliable: false, allowPooling: true, congestionControl: 'low-latency', protocols: ['drpc'] })
      c.close()
    })
  })

  it('names the way out where the runtime has no global WebTransport', () => {
    withGlobalWT(undefined, () => {
      expect(() => dialWebTransport('https://host/rpc')).toThrow(/no global WebTransport/)
    })
  })
})
