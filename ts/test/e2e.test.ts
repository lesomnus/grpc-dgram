// End-to-end over a reliable in-memory channel: the four RPC types, EOF
// semantics, metadata, deadlines, cancellation, and reliable-mode fail-loud
// (PROTOCOL.md §8, §10.6, §11) — the TS twin of the Go e2e/mode suites.

import { describe, expect, it } from 'vitest'
import { EndOfStreamError } from '../src/conn'
import { unaryMethod } from '../src/desc'
import { Code, StatusError, statusError } from '../src/status'
import { FlagOpen, frame } from '../src/wire'
import { echo, jsonCodec, makeNet, tick, type TestRes } from '../src/testing'

describe('unary', () => {
  it('round-trips', async () => {
    const net = makeNet({ reliable: true })
    const res = await net.conn.invoke(echo.once, { text: 'hi' })
    expect(res).toEqual({ text: 'echo:hi' })
    expect(net.counts.once).toBe(1)
  })

  it('propagates handler status errors', async () => {
    const net = makeNet({ reliable: true })
    net.server.register(unaryMethod<{ text: string }, TestRes>('/test.Echo/Fail', { request: jsonCodec(), response: jsonCodec() }), () => {
      throw statusError(Code.INVALID_ARGUMENT, 'bad request')
    })
    const err = await net.conn
      .invoke(unaryMethod<{ text: string }, TestRes>('/test.Echo/Fail', { request: jsonCodec(), response: jsonCodec() }), { text: 'x' })
      .catch((e) => e)
    expect(err).toBeInstanceOf(StatusError)
    expect(err.code).toBe(Code.INVALID_ARGUMENT)
    expect(err.desc).toBe('bad request')
  })

  it('unknown method draws UNIMPLEMENTED (§13)', async () => {
    const net = makeNet({ reliable: true })
    const bogus = unaryMethod<TestRes, TestRes>('/test.Echo/Nope', { request: jsonCodec(), response: jsonCodec() })
    const err = await net.conn.invoke(bogus, { text: 'x' }).catch((e) => e)
    expect(err.code).toBe(Code.UNIMPLEMENTED)
  })

  it('unknown codec draws UNIMPLEMENTED (§12)', async () => {
    const net = makeNet({ reliable: true })
    const err = await net.conn
      .invoke(echo.once, { text: 'x' }, { codec: { name: 'nope', request: jsonCodec(), response: jsonCodec() } })
      .catch((e) => e)
    expect(err.code).toBe(Code.UNIMPLEMENTED)
    expect(err.desc).toContain('nope')
  })

  it('a registered named codec resolves (§12)', async () => {
    const net = makeNet({
      reliable: true,
      serverOpts: {
        codecs: {
          json2: { resolve: (desc) => ({ request: desc.request, response: desc.response }) },
        },
      },
    })
    const res = await net.conn.invoke(echo.once, { text: 'hi' }, { codec: { name: 'json2', request: jsonCodec(), response: jsonCodec() } })
    expect(res).toEqual({ text: 'echo:hi' })
  })

  it('populates header and trailer callbacks regardless of status', async () => {
    const net = makeNet({ reliable: true })
    net.server.register(unaryMethod<TestRes, TestRes>('/test.Echo/Md', { request: jsonCodec(), response: jsonCodec() }), (_req, ctx) => {
      ctx.setHeader({ 'x-h': ['1'] })
      ctx.setTrailer({ 'x-t': ['2'] })
      throw statusError(Code.ABORTED, 'nope')
    })
    let header, trailer
    const err = await net.conn
      .invoke(unaryMethod<TestRes, TestRes>('/test.Echo/Md', { request: jsonCodec(), response: jsonCodec() }), { text: '' }, {
        onHeader: (md) => (header = md),
        onTrailer: (md) => (trailer = md),
      })
      .catch((e) => e)
    expect(err.code).toBe(Code.ABORTED)
    expect(header).toEqual({ 'x-h': ['1'] })
    expect(trailer).toEqual({ 'x-t': ['2'] })
  })

  it('request metadata rides the OPEN and reaches the handler (§11)', async () => {
    const net = makeNet({ reliable: true })
    let seen
    net.server.register(unaryMethod<TestRes, TestRes>('/test.Echo/Meta', { request: jsonCodec(), response: jsonCodec() }), (_req, ctx) => {
      seen = ctx.metadata
      return { text: 'ok' }
    })
    await net.conn.invoke(unaryMethod<TestRes, TestRes>('/test.Echo/Meta', { request: jsonCodec(), response: jsonCodec() }), { text: '' }, {
      metadata: { auth: ['tok'] },
    })
    expect(seen).toEqual({ auth: ['tok'] })
  })

  it('reliable mode injects no default deadline (§10.6)', async () => {
    const net = makeNet({ reliable: true })
    let sawDeadline: number | undefined = -1
    net.server.register(unaryMethod<TestRes, TestRes>('/test.Echo/Dl', { request: jsonCodec(), response: jsonCodec() }), (_req, ctx) => {
      sawDeadline = ctx.deadline
      return { text: 'ok' }
    })
    await net.conn.invoke(unaryMethod<TestRes, TestRes>('/test.Echo/Dl', { request: jsonCodec(), response: jsonCodec() }), { text: '' })
    expect(sawDeadline).toBeUndefined()
    // The OPEN carried no timeout field.
    const open = net.sentC2S.find((f) => (f.flags & FlagOpen) !== 0)
    expect(open?.timeoutMs).toBeUndefined()
  })

  it('an explicit deadline is honored and propagated in reliable mode (§10.2)', async () => {
    const net = makeNet({ reliable: true })
    net.server.register(unaryMethod<TestRes, TestRes>('/test.Echo/Slow', { request: jsonCodec(), response: jsonCodec() }), async (_req, ctx) => {
      await new Promise<void>((res) => ctx.signal.addEventListener('abort', () => res()))
      return { text: 'late' }
    })
    const err = await net.conn
      .invoke(unaryMethod<TestRes, TestRes>('/test.Echo/Slow', { request: jsonCodec(), response: jsonCodec() }), { text: '' }, { timeoutMs: 30 })
      .catch((e) => e)
    expect(err.code).toBe(Code.DEADLINE_EXCEEDED)
    const open = net.sentC2S.find((f) => (f.flags & FlagOpen) !== 0)
    expect(open?.timeoutMs).toBeGreaterThan(0)
  })
})

describe('server streaming', () => {
  it('delivers the exact sequence then EOF', async () => {
    const net = makeNet({ reliable: true })
    const stream = net.conn.newStream(echo.many, {})
    await stream.send({ text: 'm', n: 4 })
    const got: string[] = []
    for await (const res of stream) got.push(res.text)
    expect(got).toEqual(['m#0', 'm#1', 'm#2', 'm#3'])
    expect(await stream.recv()).toBeUndefined() // EOF is sticky
  })

  it('latches header metadata before the first message (§11)', async () => {
    const net = makeNet({ reliable: true })
    net.server.register(echo.many, async (req, stream, ctx) => {
      await ctx.sendHeader({ 'x-h': ['v'] })
      await stream.send({ text: req.text })
      ctx.setTrailer({ 'x-t': ['w'] })
    })
    const stream = net.conn.newStream(echo.many, {})
    await stream.send({ text: 'a', n: 1 })
    expect(await stream.header()).toEqual({ 'x-h': ['v'] })
    expect(await stream.recv()).toEqual({ text: 'a' })
    expect(await stream.recv()).toBeUndefined()
    expect(stream.trailer()).toEqual({ 'x-t': ['w'] })
  })
})

describe('client streaming', () => {
  it('counts messages and returns the response on the terminal', async () => {
    const net = makeNet({ reliable: true })
    const stream = net.conn.newStream(echo.count, {})
    await stream.send({ text: 'a' })
    await stream.send({ text: 'b' })
    await stream.send({ text: 'c' })
    stream.closeSend()
    expect(await stream.recv()).toEqual({ text: '3' })
    expect(await stream.recv()).toBeUndefined()
    expect(net.counts.count).toBe(1)
  })

  it('send after closeSend is INTERNAL and ends the call (grpc-go parity)', async () => {
    const net = makeNet({ reliable: true })
    const stream = net.conn.newStream(echo.count, {})
    stream.closeSend()
    const err = await stream.send({ text: 'x' }).catch((e) => e)
    expect(err).toBeInstanceOf(StatusError)
    expect(err.code).toBe(Code.INTERNAL)
    // The misuse killed the call, as in grpc-go: recv reports the same status.
    const err2 = await stream.recv().catch((e) => e)
    expect(err2.code).toBe(Code.INTERNAL)
  })

  it('send after a finished call is EndOfStreamError', async () => {
    const net = makeNet({ reliable: true })
    const stream = net.conn.newStream(echo.count, {})
    await stream.send({ text: 'a' })
    stream.closeSend()
    expect(await stream.recv()).toEqual({ text: '1' })
    expect(await stream.recv()).toBeUndefined()
    const err = await stream.send({ text: 'y' }).catch((e) => e)
    expect(err).toBeInstanceOf(EndOfStreamError)
  })
})

describe('bidi', () => {
  it('echoes interleaved and ends after half-close', async () => {
    const net = makeNet({ reliable: true })
    const stream = net.conn.newStream(echo.live, {})
    await stream.send({ text: 'x' })
    expect(await stream.recv()).toEqual({ text: 'echo:x' })
    await stream.send({ text: 'y' })
    expect(await stream.recv()).toEqual({ text: 'echo:y' })
    stream.closeSend()
    expect(await stream.recv()).toBeUndefined()
  })

  it('supports server push without any client message (eager OPEN, §8)', async () => {
    const net = makeNet({ reliable: true })
    net.server.register(echo.live, async (stream) => {
      await stream.send({ text: 'push1' })
      await stream.send({ text: 'push2' })
    })
    const stream = net.conn.newStream(echo.live, {})
    expect(await stream.recv()).toEqual({ text: 'push1' })
    expect(await stream.recv()).toEqual({ text: 'push2' })
    expect(await stream.recv()).toBeUndefined()
  })

  it('client abort cancels the handler and surfaces CANCELLED', async () => {
    const net = makeNet({ reliable: true })
    let handlerAborted: StatusError | undefined
    net.server.register(echo.live, async (stream, ctx) => {
      try {
        await stream.recv()
      } catch (e) {
        handlerAborted = e as StatusError
        throw e
      }
      void ctx
    })
    const ctl = new AbortController()
    const stream = net.conn.newStream(echo.live, { signal: ctl.signal })
    await tick()
    ctl.abort()
    const err = await stream.recv().catch((e) => e)
    expect(err.code).toBe(Code.CANCELLED)
    await tick()
    expect(handlerAborted?.code).toBe(Code.CANCELLED)
    expect(net.sentS2C.some((f) => f.code === Code.CANCELLED)).toBe(true) // T{CANCELLED} from the unwinding handler
  })

  it('stream.cancel() aborts without a pre-made controller', async () => {
    const net = makeNet({ reliable: true })
    const stream = net.conn.newStream(echo.live, {})
    await tick()
    stream.cancel()
    const err = await stream.recv().catch((e) => e)
    expect(err.code).toBe(Code.CANCELLED)
  })
})

describe('reliable-mode fail-loud (§10.6)', () => {
  it('a seq gap on the client fails the call with INTERNAL', async () => {
    const net = makeNet({ reliable: true })
    // Swallow the first server data frame after H: the client sees seq jump.
    let dropped = false
    net.s2c.filter = (f) => {
      if (!dropped && f.payload !== undefined && f.flags === 0) {
        dropped = true
        return false
      }
      return true
    }
    const stream = net.conn.newStream(echo.many, {})
    await stream.send({ text: 'm', n: 2 })
    const err = await stream.recv().catch((e) => e)
    expect(err).toBeInstanceOf(StatusError)
    expect(err.code).toBe(Code.INTERNAL)
  })

  it('a duplicated client frame fails the call at the server', async () => {
    const net = makeNet({ reliable: true })
    let handlerErr: unknown
    net.server.register(echo.live, async (stream) => {
      try {
        for await (const _ of stream) {
          /* consume */
        }
      } catch (e) {
        handlerErr = e
        throw e
      }
    })
    const stream = net.conn.newStream(echo.live, {})
    await stream.send({ text: 'x' })
    // Replay the data frame (seq dup) straight into the server.
    const dup = net.sentC2S.find((f) => f.payload !== undefined && f.flags === 0)!
    await net.server.handle(dup, { peer: net.peer })
    await tick()
    expect((handlerErr as StatusError).code).toBe(Code.INTERNAL)
  })
})

describe('registry (§13)', () => {
  it('freezes registration once serving starts', async () => {
    const net = makeNet({ reliable: true })
    await net.server.handle(frame({ epoch: 9, flags: 8 }), { peer: net.peer }) // any frame flips serving
    expect(() => net.counts).toBeDefined()
    expect(() =>
      net.server.register(unaryMethod<TestRes, TestRes>('/x/Y', { request: jsonCodec(), response: jsonCodec() }), () => ({ text: '' })),
    ).toThrow(/register called after/)
  })
})

describe('lifecycle', () => {
  it('conn.close fails live calls with UNAVAILABLE and refuses new ones', async () => {
    const net = makeNet({ reliable: true })
    const stream = net.conn.newStream(echo.live, {})
    await tick()
    net.conn.close()
    const err = await stream.recv().catch((e) => e)
    expect(err.code).toBe(Code.UNAVAILABLE)
    expect(() => net.conn.newStream(echo.live, {})).toThrow(/connection is closed/)
  })

  it('server.stop cancels handlers and refuses new OPENs with RESET', async () => {
    const net = makeNet({ reliable: true })
    const stream = net.conn.newStream(echo.live, {})
    await tick()
    const stopped = net.server.stop()
    const err = await stream.recv().catch((e) => e)
    expect(err.code).toBe(Code.UNAVAILABLE) // T{UNAVAILABLE "server stopped"}
    await stopped
    const s2 = net.conn.newStream(echo.live, {})
    await tick()
    const err2 = await s2.recv().catch((e) => e)
    expect(err2.code).toBe(Code.UNAVAILABLE) // RESET → "call reset by peer"
  })

  it('disconnectPeer cancels only that peer’s handlers (§4.5)', async () => {
    const net = makeNet({ reliable: true })
    const stream = net.conn.newStream(echo.live, {})
    await tick()
    net.server.disconnectPeer(net.peer, new Error('gone'))
    const err = await stream.recv().catch((e) => e)
    // The handler unwinds with UNAVAILABLE and its terminal reaches the
    // still-connected client transport.
    expect(err.code).toBe(Code.UNAVAILABLE)
  })

  it('gracefulStop waits for in-flight handlers', async () => {
    const net = makeNet({ reliable: true })
    const stream = net.conn.newStream(echo.live, {})
    await stream.send({ text: 'x' })
    await stream.recv()
    const done = net.server.gracefulStop()
    let resolved = false
    void done.then(() => (resolved = true))
    await tick()
    expect(resolved).toBe(false) // the bidi handler is still live
    stream.closeSend()
    await stream.recv()
    await done
  })
})
