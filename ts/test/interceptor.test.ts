// Interceptors (src/interceptor.ts): chain order, what the chain sees and
// may change, where the OPEN is built relative to it, and the one `next` a
// server stream interceptor gets for all three streaming shapes — the TS
// twin of the Go interceptor coverage.

import { describe, expect, it } from 'vitest'
import { clientStreamingMethod, unaryMethod } from '../src/desc'
import type { StreamClientInterceptor, StreamServerInterceptor, UnaryClientInterceptor, UnaryServerInterceptor } from '../src/interceptor'
import type { Metadata } from '../src/metadata'
import type { ServerContext } from '../src/server'
import { Code, StatusError, statusError } from '../src/status'
import { isOpen } from '../src/wire'
import { echo, jsonCodec, makeNet, type TestReq, type TestRes } from '../src/testing'

// recorder builds interceptors that log entry and exit around next, so the
// chain order can be asserted from the log alone.
function recorder(log: string[]) {
  return {
    unaryClient(name: string): UnaryClientInterceptor {
      return async (req, call, next) => {
        log.push(`>${name}`)
        try {
          return await next(req, call)
        } finally {
          log.push(`<${name}`)
        }
      }
    },
    streamClient(name: string): StreamClientInterceptor {
      return (call, next) => {
        log.push(`>${name}`)
        const s = next(call)
        log.push(`<${name}`)
        return s
      }
    },
    unaryServer(name: string): UnaryServerInterceptor {
      return async (req, ctx, next) => {
        log.push(`>${name}`)
        try {
          return await next(req, ctx)
        } finally {
          log.push(`<${name}`)
        }
      }
    },
    streamServer(name: string): StreamServerInterceptor {
      return async (stream, ctx, next) => {
        log.push(`>${name}`)
        try {
          return await next(stream, ctx)
        } finally {
          log.push(`<${name}`)
        }
      }
    },
  }
}

// A unary method whose response names the request metadata keys the handler
// saw — how the tests observe what rode the OPEN.
const meta = unaryMethod<TestReq, TestRes>('/test.Echo/Meta', { request: jsonCodec(), response: jsonCodec() })
const metaCount = clientStreamingMethod<TestReq, TestRes>('/test.Echo/MetaCount', { request: jsonCodec(), response: jsonCodec() })
const keysOf = (ctx: ServerContext) =>
  Object.keys(ctx.metadata ?? {})
    .sort()
    .join(',')

describe('client unary', () => {
  it('element 0 runs outermost; the last element is handed the invoker', async () => {
    const log: string[] = []
    const r = recorder(log)
    const net = makeNet({ reliable: true, connOpts: { unaryInterceptors: [r.unaryClient('1'), r.unaryClient('2'), r.unaryClient('3')] } })
    const res = await net.conn.invoke(echo.once, { text: 'hi' })
    expect(res).toEqual({ text: 'echo:hi' })
    expect(log).toEqual(['>1', '>2', '>3', '<3', '<2', '<1'])
    expect(net.counts.once).toBe(1)
  })

  it('sees the merged options, and what it adds rides the OPEN (§11)', async () => {
    const seen: (Metadata | undefined)[] = []
    const net = makeNet({
      reliable: true,
      connOpts: {
        defaultCallOptions: { metadata: { 'x-default': ['d'] } },
        unaryInterceptors: [
          async (req, call, next) => {
            seen.push(call.opts.metadata)
            call.opts = { ...call.opts, metadata: { ...call.opts.metadata, authorization: ['bearer t'] } }
            return next(req, call)
          },
        ],
      },
      register: (s) => s.register(meta, (_req, ctx) => ({ text: keysOf(ctx) })),
    })
    // Endpoint defaults, then per-call options over them (a shallow merge:
    // a per-call metadata replaces the default's, as it did before chains).
    expect((await net.conn.invoke(meta, { text: '' })).text).toBe('authorization,x-default')
    const opts = { metadata: { 'x-call': ['c'] } }
    expect((await net.conn.invoke(meta, { text: '' }, opts)).text).toBe('authorization,x-call')
    expect(seen).toEqual([{ 'x-default': ['d'] }, { 'x-call': ['c'] }])
    // The chain worked on the merged copy; the caller's object is untouched.
    expect(opts).toEqual({ metadata: { 'x-call': ['c'] } })
  })

  it('metadata the chain adds is validated where the stream is built', async () => {
    const net = makeNet({
      reliable: true,
      connOpts: {
        unaryInterceptors: [
          async (req, call, next) => {
            call.opts.metadata = { 'Bad Key': ['x'] }
            return next(req, call)
          },
        ],
      },
    })
    const err = await net.conn.invoke(echo.once, { text: 'hi' }).catch((e) => e)
    expect(err).toBeInstanceOf(StatusError)
    expect(err.code).toBe(Code.INTERNAL)
    expect(net.sentC2S).toHaveLength(0)
  })

  it('T_call is applied before the chain and a replaced deadline reaches the server (§10.2)', async () => {
    const seenTimeouts: (number | undefined)[] = []
    const deadlines: (number | undefined)[] = []
    let override: number | undefined
    const net = makeNet({
      reliable: false,
      connOpts: {
        unaryInterceptors: [
          async (req, call, next) => {
            seenTimeouts.push(call.opts.timeoutMs)
            if (override !== undefined) call.opts.timeoutMs = override
            return next(req, call)
          },
        ],
      },
      register: (s) =>
        s.register(meta, (_req, ctx) => {
          deadlines.push(ctx.deadline)
          return { text: '' }
        }),
    })
    try {
      await net.conn.invoke(meta, { text: '' })
      override = 60_000
      const before = Date.now()
      await net.conn.invoke(meta, { text: '' })
      expect(seenTimeouts).toEqual([5_000, 5_000]) // T_call, both times
      expect(deadlines[0]).toBeDefined()
      expect(deadlines[1]).toBeGreaterThanOrEqual(before + 60_000 - 1_000)
    } finally {
      net.conn.close()
      await net.server.stop()
    }
  })

  it('may answer without calling next — nothing reaches the wire', async () => {
    const net = makeNet({ reliable: true, connOpts: { unaryInterceptors: [async () => ({ text: 'cached' })] } })
    const res = await net.conn.invoke(echo.once, { text: 'hi' })
    expect(res).toEqual({ text: 'cached' })
    expect(net.sentC2S).toHaveLength(0)
    expect(net.counts.once).toBe(0)
  })

  it('may call next again — a retry is a fresh stream', async () => {
    let attempts = 0
    const flaky = unaryMethod<TestReq, TestRes>('/test.Echo/Flaky', { request: jsonCodec(), response: jsonCodec() })
    const net = makeNet({
      reliable: true,
      connOpts: {
        unaryInterceptors: [
          async (req, call, next) => {
            for (let i = 0; ; i++) {
              try {
                return await next(req, call)
              } catch (e) {
                if (i < 2 && e instanceof StatusError && e.code === Code.UNAVAILABLE) continue
                throw e
              }
            }
          },
        ],
      },
      register: (s) =>
        s.register(flaky, (req) => {
          if (attempts++ === 0) throw statusError(Code.UNAVAILABLE, 'not yet')
          return { text: `ok:${req.text}` }
        }),
    })
    const res = await net.conn.invoke(flaky, { text: 'x' })
    expect(res).toEqual({ text: 'ok:x' })
    expect(attempts).toBe(2)
    expect(net.sentC2S.filter(isOpen)).toHaveLength(2)
  })

  it('may translate the status', async () => {
    const bad = unaryMethod<TestReq, TestRes>('/test.Echo/Bad', { request: jsonCodec(), response: jsonCodec() })
    const net = makeNet({
      reliable: true,
      connOpts: {
        unaryInterceptors: [
          async (req, call, next) => {
            try {
              return await next(req, call)
            } catch (e) {
              if (e instanceof StatusError && e.code === Code.INVALID_ARGUMENT) throw statusError(Code.FAILED_PRECONDITION, `wrapped: ${e.desc}`)
              throw e
            }
          },
        ],
      },
      register: (s) =>
        s.register(bad, () => {
          throw statusError(Code.INVALID_ARGUMENT, 'no')
        }),
    })
    const err = await net.conn.invoke(bad, { text: '' }).catch((e) => e)
    expect(err.code).toBe(Code.FAILED_PRECONDITION)
    expect(err.desc).toBe('wrapped: no')
  })

  it('an empty chain is no chain', async () => {
    const net = makeNet({ reliable: true, connOpts: { unaryInterceptors: [], streamInterceptors: [] } })
    expect(await net.conn.invoke(echo.once, { text: 'hi' })).toEqual({ text: 'echo:hi' })
  })
})

describe('client stream', () => {
  it('element 0 runs outermost; the stream is created by the innermost streamer', async () => {
    const log: string[] = []
    const r = recorder(log)
    const net = makeNet({ reliable: true, connOpts: { streamInterceptors: [r.streamClient('1'), r.streamClient('2')] } })
    const s = net.conn.newStream(echo.many)
    expect(log).toEqual(['>1', '>2', '<2', '<1'])
    await s.send({ text: 'm', n: 2 })
    s.closeSend()
    const got: string[] = []
    for (let m = await s.recv(); m !== undefined; m = await s.recv()) got.push(m.text)
    expect(got).toEqual(['m#0', 'm#1'])
  })

  it('metadata the chain adds rides the eager OPEN of a client-streaming call (§8, §11)', async () => {
    const net = makeNet({
      reliable: true,
      connOpts: {
        streamInterceptors: [
          (call, next) => {
            call.opts = { ...call.opts, metadata: { ...call.opts.metadata, authorization: ['bearer t'] } }
            return next(call)
          },
        ],
      },
      register: (s) =>
        s.register(metaCount, async (stream, ctx) => {
          for await (const _ of stream) {
            /* drain */
          }
          return { text: keysOf(ctx) }
        }),
    })
    const s = net.conn.newStream(metaCount, { metadata: { 'x-call': ['c'] } })
    // The OPEN went out at creation, before any send.
    expect(net.sentC2S.filter(isOpen)).toHaveLength(1)
    s.closeSend()
    expect(await s.recv()).toEqual({ text: 'authorization,x-call' })
  })

  it('may wrap the stream it returns', async () => {
    let sent = 0
    const net = makeNet({
      reliable: true,
      connOpts: {
        streamInterceptors: [
          (call, next) => {
            const s = next(call)
            return new Proxy(s, {
              get(target, prop, receiver) {
                if (prop === 'send') {
                  return async (m: unknown) => {
                    sent++
                    await target.send(m)
                  }
                }
                return Reflect.get(target, prop, receiver)
              },
            })
          },
        ],
      },
    })
    const s = net.conn.newStream(echo.live)
    await s.send({ text: 'a' })
    expect(await s.recv()).toEqual({ text: 'echo:a' })
    await s.send({ text: 'b' })
    expect(await s.recv()).toEqual({ text: 'echo:b' })
    s.closeSend()
    expect(await s.recv()).toBeUndefined()
    expect(sent).toBe(2)
  })

  it('a throw from the chain is the caller\'s, and nothing reaches the wire', () => {
    const net = makeNet({
      reliable: true,
      connOpts: {
        streamInterceptors: [
          () => {
            throw statusError(Code.PERMISSION_DENIED, 'no token')
          },
        ],
      },
    })
    expect(() => net.conn.newStream(echo.live)).toThrow(StatusError)
    expect(net.sentC2S).toHaveLength(0)
  })
})

describe('server unary', () => {
  it('element 0 runs outermost; the last element is handed the handler', async () => {
    const log: string[] = []
    const r = recorder(log)
    const net = makeNet({ reliable: true, serverOpts: { unaryInterceptors: [r.unaryServer('1'), r.unaryServer('2'), r.unaryServer('3')] } })
    const res = await net.conn.invoke(echo.once, { text: 'hi' })
    expect(res).toEqual({ text: 'echo:hi' })
    expect(log).toEqual(['>1', '>2', '>3', '<3', '<2', '<1'])
  })

  it('sees the call context, and a thrown status is the terminal (§11)', async () => {
    const seen: string[] = []
    const auth: UnaryServerInterceptor = (req, ctx, next) => {
      seen.push(`${ctx.method} ${String(ctx.peer)} ${ctx.desc.clientStreams}/${ctx.desc.serverStreams}`)
      if (ctx.metadata?.authorization === undefined) throw statusError(Code.PERMISSION_DENIED, 'no token')
      return next(req, ctx)
    }
    const net = makeNet({ reliable: true, serverOpts: { unaryInterceptors: [auth] } })
    const err = await net.conn.invoke(echo.once, { text: 'hi' }).catch((e) => e)
    expect(err).toBeInstanceOf(StatusError)
    expect(err.code).toBe(Code.PERMISSION_DENIED)
    expect(net.counts.once).toBe(0)
    expect(await net.conn.invoke(echo.once, { text: 'hi' }, { metadata: { authorization: ['t'] } })).toEqual({ text: 'echo:hi' })
    expect(net.counts.once).toBe(1)
    expect(seen).toEqual(['/test.Echo/Once peer-1 false/false', '/test.Echo/Once peer-1 false/false'])
  })

  it('may substitute the response', async () => {
    const net = makeNet({
      reliable: true,
      serverOpts: {
        unaryInterceptors: [
          async (req, ctx, next) => {
            const out = (await next(req, ctx)) as TestRes
            return { text: `wrapped:${out.text}` }
          },
        ],
      },
    })
    expect(await net.conn.invoke(echo.once, { text: 'hi' })).toEqual({ text: 'wrapped:echo:hi' })
  })
})

describe('server stream', () => {
  it('one next serves the three streaming shapes; ctx.desc tells them apart (§13)', async () => {
    const log: string[] = []
    const r = recorder(log)
    const shapes: string[] = []
    const observe: StreamServerInterceptor = async (stream, ctx, next) => {
      shapes.push(`${ctx.method} ${ctx.desc.clientStreams}/${ctx.desc.serverStreams}`)
      const out = await next(stream, ctx)
      // Client-streaming: what the chain resolves to is the response.
      if (ctx.desc.clientStreams && !ctx.desc.serverStreams) return { text: `wrapped:${(out as TestRes).text}` }
      return out
    }
    const net = makeNet({ reliable: true, serverOpts: { streamInterceptors: [r.streamServer('1'), observe, r.streamServer('3')] } })

    const many = net.conn.newStream(echo.many)
    await many.send({ text: 'm', n: 2 })
    many.closeSend()
    const got: string[] = []
    for (let m = await many.recv(); m !== undefined; m = await many.recv()) got.push(m.text)
    expect(got).toEqual(['m#0', 'm#1'])

    const count = net.conn.newStream(echo.count)
    await count.send({ text: 'a' })
    await count.send({ text: 'b' })
    count.closeSend()
    expect(await count.recv()).toEqual({ text: 'wrapped:2' })

    const live = net.conn.newStream(echo.live)
    await live.send({ text: 'x' })
    expect(await live.recv()).toEqual({ text: 'echo:x' })
    live.closeSend()
    expect(await live.recv()).toBeUndefined()

    expect(shapes).toEqual(['/test.Echo/Many false/true', '/test.Echo/Count true/false', '/test.Echo/Live true/true'])
    expect(log).toEqual(['>1', '>3', '<3', '<1', '>1', '>3', '<3', '<1', '>1', '>3', '<3', '<1'])
    expect(net.counts).toEqual({ once: 0, many: 1, count: 1, live: 1 })
  })

  it('may wrap the stream it hands on', async () => {
    let sent = 0
    const net = makeNet({
      reliable: true,
      serverOpts: {
        streamInterceptors: [
          (stream, ctx, next) =>
            next(
              new Proxy(stream, {
                get(target, prop, receiver) {
                  if (prop === 'send') {
                    return async (m: unknown) => {
                      sent++
                      await target.send(m)
                    }
                  }
                  return Reflect.get(target, prop, receiver)
                },
              }),
              ctx,
            ),
        ],
      },
    })
    const s = net.conn.newStream(echo.many)
    await s.send({ text: 'm', n: 3 })
    s.closeSend()
    let n = 0
    for (let m = await s.recv(); m !== undefined; m = await s.recv()) n++
    expect(n).toBe(3)
    expect(sent).toBe(3)
  })

  it('a thrown status is the terminal, and the handler never runs', async () => {
    const net = makeNet({
      reliable: true,
      serverOpts: {
        streamInterceptors: [
          () => {
            throw statusError(Code.PERMISSION_DENIED, 'no token')
          },
        ],
      },
    })
    const s = net.conn.newStream(echo.live)
    await s.send({ text: 'x' })
    const err = await s.recv().catch((e) => e)
    expect(err).toBeInstanceOf(StatusError)
    expect(err.code).toBe(Code.PERMISSION_DENIED)
    expect(net.counts.live).toBe(0)
  })
})
