// The gRPC-fidelity surface of wire v1.1 (PROTOCOL.md §5, §11, §16) — the TS
// twin of the Go compat_test.go / header_md_test.go coverage:
//
//   - the per-call size caps (MaxCallRecvMsgSize / MaxCallSendMsgSize), on
//     both roles, measured on the DECOMPRESSED received message and on the
//     COMPRESSED sent bytes, failing with RESOURCE_EXHAUSTED as grpc-go does;
//   - binary ("-bin") metadata carrying arbitrary octets verbatim, and the
//     validation that mirrors grpc-go's — a violation fails the call locally
//     with INTERNAL and never reaches the wire;
//   - google.rpc.Status details riding the terminal frame, surfacing on the
//     client, and shed as passengers when the channel refuses the frame;
//   - SendHeader flushing an H at once — on unary calls too, so a client's
//     header() returns before the response — and the double flush that is an
//     error, while the core's own creation ack is not a flush.

import { describe, expect, it } from 'vitest'
import { Conn, statusDetails } from '../src/conn'
import { encodeBase64 } from '../src/metadata'
import { Server } from '../src/server'
import { Code, MessageTooLargeError, statusError, type StatusError } from '../src/status'
import { getCompressor } from '../src/util'
import { encodeFrame, FlagCompressed, isHeaderFrame, isOpen, isTerminal, type Any, type Frame } from '../src/wire'
import { echo, makeNet, wireClone, type TestRes } from '../src/testing'

const cap4k = 4096
const big = 'a'.repeat(100_000)
const gzip = getCompressor('gzip')!

const enc = (v: unknown) => new TextEncoder().encode(JSON.stringify(v))

// randomText is deterministic high-entropy printable text: compression cannot
// bring it under a small cap, so §12.1's "send it raw" leaves it oversize.
function randomText(n: number): string {
  const alphabet = '0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ!#$%&()*+,-./:;<=>?@[]^_{|}~'
  let s = 0xd1ce >>> 0
  let out = ''
  for (let i = 0; i < n; i++) {
    s = (s * 1664525 + 1013904223) >>> 0
    out += alphabet[s % alphabet.length]
  }
  return out
}

// indexOfBytes finds needle inside haystack, or -1 — the "these octets really
// are on the wire" check for binary metadata.
function indexOfBytes(haystack: Uint8Array, needle: Uint8Array): number {
  outer: for (let i = 0; i + needle.length <= haystack.length; i++) {
    for (let j = 0; j < needle.length; j++) {
      if (haystack[i + j] !== needle[j]) continue outer
    }
    return i
  }
  return -1
}

const fail = async (p: Promise<unknown>): Promise<StatusError> => (await p.catch((e: unknown) => e)) as StatusError

// ---------------------------------------------------------------------------
// §16 / Appendix B — per-call size caps (grpc-go parity)
// ---------------------------------------------------------------------------

describe('receive size cap (§16)', () => {
  it('fails the call with ResourceExhausted from a call option, in grpc-go’s wording', async () => {
    const net = makeNet({ reliable: true })
    const err = await fail(net.conn.invoke(echo.once, { text: big }, { maxRecvMsgSize: cap4k }))
    expect(err.code).toBe(Code.RESOURCE_EXHAUSTED)
    expect(err.desc).toContain('received message larger than max')
  })

  it('applies from the endpoint option, and a call option overrides it', async () => {
    const net = makeNet({ reliable: true, connOpts: { maxRecvMsgSize: cap4k } })
    const err = await fail(net.conn.invoke(echo.once, { text: big }))
    expect(err.code).toBe(Code.RESOURCE_EXHAUSTED)

    const res = (await net.conn.invoke(echo.once, { text: big }, { maxRecvMsgSize: 1 << 20 })) as TestRes
    expect(res.text).toHaveLength(`echo:${big}`.length)
  })

  it('the server’s cap refuses the request, and the status travels as a terminal', async () => {
    const net = makeNet({ reliable: true, serverOpts: { maxRecvMsgSize: cap4k } })
    const err = await fail(net.conn.invoke(echo.once, { text: big }))
    expect(err.code).toBe(Code.RESOURCE_EXHAUSTED)
    // It is the SERVER that refused: the status was not synthesized locally.
    const term = net.sentS2C.find(isTerminal)
    expect(term).toBeDefined()
    expect(term!.code).toBe(Code.RESOURCE_EXHAUSTED)
    expect(net.counts.once).toBe(0) // the handler never saw the message
  })

  it('defaults to gRPC’s 4 MiB with nothing configured anywhere', async () => {
    const net = makeNet({ reliable: true })
    const err = await fail(net.conn.invoke(echo.once, { text: 'a'.repeat(4 * 1024 * 1024 + 16) }))
    expect(err.code).toBe(Code.RESOURCE_EXHAUSTED)
  })

  it('0 rejects everything, rather than reading as unlimited', async () => {
    const net = makeNet({ reliable: true })
    const err = await fail(net.conn.invoke(echo.once, { text: 'hi' }, { maxRecvMsgSize: 0 }))
    expect(err.code).toBe(Code.RESOURCE_EXHAUSTED)
  })
})

describe('send size cap (§16)', () => {
  it('fails before anything reaches the wire, in grpc-go’s wording', async () => {
    const net = makeNet({ reliable: true })
    const err = await fail(net.conn.invoke(echo.once, { text: big }, { maxSendMsgSize: cap4k }))
    expect(err.code).toBe(Code.RESOURCE_EXHAUSTED)
    expect(err.desc).toContain('trying to send message larger than max')
    // Refused locally: the OPEN never left, so the server never saw the call.
    expect(net.sentC2S).toEqual([])
    expect(net.counts.once).toBe(0)
  })

  it('applies from the endpoint option', async () => {
    const net = makeNet({ reliable: true, connOpts: { maxSendMsgSize: cap4k } })
    const err = await fail(net.conn.invoke(echo.once, { text: big }))
    expect(err.code).toBe(Code.RESOURCE_EXHAUSTED)
    expect(net.sentC2S).toEqual([])
  })

  it('the server’s cap fails the response, and the terminal carries the status', async () => {
    const net = makeNet({ reliable: true, serverOpts: { maxSendMsgSize: cap4k } })
    const stream = net.conn.newStream(echo.many, {})
    await stream.send({ text: big, n: 1 })
    const err = await fail(stream.recv())
    expect(err.code).toBe(Code.RESOURCE_EXHAUSTED)
    const term = net.sentS2C.find(isTerminal)
    expect(term).toBeDefined()
    expect(term!.code).toBe(Code.RESOURCE_EXHAUSTED)
  })

  it('measures the COMPRESSED bytes: a message that compresses under the cap is sendable', async () => {
    const net = makeNet({ reliable: true, serverOpts: { compressors: { gzip } } })
    expect(enc({ text: big }).length).toBeGreaterThan(cap4k) // the message itself exceeds the cap

    const res = (await net.conn.invoke(echo.once, { text: big }, { compressor: 'gzip', maxSendMsgSize: cap4k })) as TestRes
    expect(res.text).toBe(`echo:${big}`)

    const open = net.sentC2S.find(isOpen)!
    expect(open.flags & FlagCompressed).toBe(FlagCompressed)
    // What the cap measured is what the frame carried.
    expect(open.payload!.length).toBeLessThan(cap4k)
  })

  it('incompressible past the cap still fails, with nothing on the wire', async () => {
    const net = makeNet({ reliable: true, serverOpts: { compressors: { gzip } } })
    const err = await fail(net.conn.invoke(echo.once, { text: randomText(100_000) }, { compressor: 'gzip', maxSendMsgSize: cap4k }))
    expect(err.code).toBe(Code.RESOURCE_EXHAUSTED)
    expect(net.sentC2S).toEqual([])
  })
})

// ---------------------------------------------------------------------------
// §11 / §5 — metadata representation and validation
// ---------------------------------------------------------------------------

describe('binary metadata (§11, §5)', () => {
  it('a "-bin" key carries arbitrary octets verbatim, in both directions', async () => {
    const net = makeNet({ reliable: true })
    const raw = new Uint8Array([0x00, 0x01, 0xff, 0xfe, 0x80, 0x7a, 0x7f])
    const b64 = encodeBase64(raw)
    // A zero-length value is a present value (§5), and it must survive too.
    const md = { 'trace-bin': [b64, ''], plain: ['printable'] }

    let seen
    net.server.register(echo.once, (_req, ctx) => {
      seen = ctx.metadata
      ctx.setHeader(ctx.metadata ?? {})
      ctx.setTrailer(ctx.metadata ?? {})
      return { text: 'ok' }
    })

    let header, trailer
    const res = (await net.conn.invoke(
      echo.once,
      { text: 'x' },
      { metadata: md, onHeader: (h) => (header = h), onTrailer: (t) => (trailer = t) },
    )) as TestRes
    expect(res.text).toBe('ok')

    // c -> s: the handler sees the octets it was sent, empty value included.
    expect(seen).toEqual(md)
    // s -> c: they survive the return trip on header and trailer alike.
    expect(header).toEqual(md)
    expect(trailer).toEqual(md)

    // ...and they were RAW BYTES on the wire: no base64, no UTF-8 coercion.
    // This is exactly what a proto string field could not hold, and why the
    // v1.1 wire made metadata values `bytes`.
    const wire = encodeFrame(net.sentC2S.find(isOpen)!)
    expect(indexOfBytes(wire, raw)).toBeGreaterThanOrEqual(0)
    expect(indexOfBytes(wire, new TextEncoder().encode(b64))).toBe(-1)
  })

  it('an unrepresentable "-bin" value (not base64) fails the call locally', async () => {
    // The one rule this port adds to grpc-go's: a JS string can only hold
    // octets as base64, so a mistyped binary value is reported instead of
    // silently truncated (§11, metadata.ts).
    const net = makeNet({ reliable: true })
    const err = await fail(net.conn.invoke(echo.once, { text: 'x' }, { metadata: { 'x-bin': ['not base64!'] } }))
    expect(err.code).toBe(Code.INTERNAL)
    expect(err.desc).toContain('not base64')
    expect(net.sentC2S).toEqual([])
  })
})

describe('metadata validation (§11)', () => {
  const bad: { name: string; md: Record<string, string[]>; want: string }[] = [
    { name: 'illegal key', md: { 'bad key': ['v'] }, want: 'illegal characters' },
    { name: 'empty key', md: { '': ['v'] }, want: 'empty key' },
    { name: 'non-printable text value', md: { text: ['a\u0000b'] }, want: 'non-printable' },
    { name: 'high-bit text value', md: { text: ['caf\u00e9'] }, want: 'non-printable' },
    // grpc-go lower-cases outgoing keys in FromOutgoingContext, so its
    // validation never sees the upper-case form. This port has no such
    // normalization layer — Metadata reaches the call as written (metadata.ts)
    // — so an upper-case key is a local failure here.
    { name: 'upper-case key (not normalized here)', md: { 'Mixed-Case': ['v'] }, want: 'illegal characters' },
  ]

  for (const tc of bad) {
    it(`${tc.name} fails the call locally with INTERNAL, naming what is wrong`, async () => {
      const net = makeNet({ reliable: true })
      const err = await fail(net.conn.invoke(echo.once, { text: 'x' }, { metadata: tc.md }))
      expect(err.code).toBe(Code.INTERNAL)
      expect(err.desc).toContain(tc.want)
      expect(net.sentC2S).toEqual([]) // the call must never reach the wire

      // Streaming calls are validated on the same path (§11: request MD rides
      // the OPEN, so it is checked before the call exists).
      let thrown: unknown
      try {
        net.conn.newStream(echo.live, { metadata: tc.md })
      } catch (e) {
        thrown = e
      }
      expect((thrown as StatusError | undefined)?.code).toBe(Code.INTERNAL)
      expect(net.sentC2S).toEqual([])
    })
  }

  it('the same octets under a "-bin" key are legal (§11: binary values are unvalidated)', async () => {
    const net = makeNet({ reliable: true })
    const octets = new Uint8Array([0x61, 0x00, 0x62, 0xc3, 0xa9])
    const res = (await net.conn.invoke(echo.once, { text: 'x' }, { metadata: { 'text-bin': [encodeBase64(octets)] } })) as TestRes
    expect(res.text).toBe('echo:x')
  })

  it('a handler’s invalid header fails the call, an invalid trailer is dropped (grpc-go parity)', async () => {
    const net = makeNet({ reliable: true })
    net.server.register(echo.once, (_req, ctx) => {
      // setTrailer has no error to return, so grpc-go drops what it cannot
      // send; setHeader throws, which is what keeps the terminal encodable.
      ctx.setTrailer({ 'bad name': ['v'], 'x-good': ['kept'] })
      ctx.setHeader({ 'x-newline': ['a\nb'] })
      return { text: 'never' }
    })
    let trailer
    const err = await fail(net.conn.invoke(echo.once, { text: 'x' }, { onTrailer: (t) => (trailer = t) }))
    expect(err.code).toBe(Code.INTERNAL)
    expect(err.desc).toContain('non-printable')
    expect(trailer).toBeUndefined() // the whole invalid trailer was dropped
  })
})

// ---------------------------------------------------------------------------
// §5 — google.rpc.Status details
// ---------------------------------------------------------------------------

// detailed builds the status a handler attaches rich details to: the
// DetailedStatusError shape the client reads back with statusDetails().
function detailed(code: Code, desc: string, details: Any[]): StatusError {
  const err = statusError(code, desc) as StatusError & { details?: Any[] }
  err.details = details
  return err
}

// capNet wires a Conn to a Server whose tx refuses any frame whose marshaled
// size exceeds `limit`, the way an adapter refuses an oversize datagram
// (§4.4). Refused frames are recorded as attempts, so a shed passenger is
// visible.
function capNet(limit: number) {
  const attempts: Frame[] = []
  const delivered: Frame[] = []
  let conn!: Conn
  const server = new Server(
    {
      handle: (f: Frame) => {
        const g = wireClone(f)
        attempts.push(g)
        if (encodeFrame(f).length > limit) throw new MessageTooLargeError(`frame does not fit ${limit} bytes`)
        delivered.push(g)
        return conn.handle(g, {})
      },
    },
    { reliable: true },
  )
  conn = new Conn({ handle: (f: Frame) => server.handle(wireClone(f), { peer: 'p' }) }, { reliable: true })
  return { conn, server, attempts, delivered }
}

describe('status details (§5)', () => {
  const detail: Any = { typeUrl: 'type.googleapis.com/test.Detail', value: new Uint8Array([1, 2, 3]) }

  it('ride the terminal frame and surface on the client', async () => {
    const net = makeNet({ reliable: true })
    net.server.register(echo.once, () => {
      throw detailed(Code.FAILED_PRECONDITION, 'nope', [detail])
    })

    const err = await fail(net.conn.invoke(echo.once, { text: 'x' }))
    expect(err.code).toBe(Code.FAILED_PRECONDITION)
    expect(err.desc).toBe('nope')
    expect(statusDetails(err)).toEqual([detail])

    // The details ride the frame, not the payload (§5).
    const term = net.sentS2C.find(isTerminal)!
    expect(term.details).toEqual([detail])
    expect(term.payload).toBeUndefined()
  })

  it('are shed when the channel refuses the terminal, which is never lost', async () => {
    const net = capNet(300)
    const fat: Any = { typeUrl: 'type.googleapis.com/test.Big', value: new Uint8Array(512) }
    net.server.register(echo.once, () => {
      throw detailed(Code.FAILED_PRECONDITION, 'nope', [fat])
    })

    const err = await fail(net.conn.invoke(echo.once, { text: 'x' }))
    // The status itself survived; only the passenger was dropped.
    expect(err.code).toBe(Code.FAILED_PRECONDITION)
    expect(err.desc).toBe('nope')
    expect(statusDetails(err)).toBeUndefined()

    const tried = net.attempts.filter(isTerminal)
    expect(tried).toHaveLength(2)
    expect(tried[0]!.details).toHaveLength(1) // refused with details...
    expect(tried[1]!.details).toBeUndefined() // ...accepted without them
    // A refused frame never reached the wire, so it burned no sequence number.
    expect(tried[1]!.seq).toBe(tried[0]!.seq)
    await net.server.stop()
  })

  it('a terminal that still does not fit is re-sent bare, as ResourceExhausted', async () => {
    const net = capNet(300)
    net.server.register(echo.once, () => ({ text: 'y'.repeat(1000) }))

    const err = await fail(net.conn.invoke(echo.once, { text: 'x' }))
    expect(err.code).toBe(Code.RESOURCE_EXHAUSTED)
    expect(err.desc).toContain('does not fit')

    const tried = net.attempts.filter(isTerminal)
    expect(tried).toHaveLength(2)
    expect(tried[0]!.payload).toBeDefined() // the oversize response...
    expect(tried[1]!.payload).toBeUndefined() // ...shed, so the terminal arrives
    await net.server.stop()
  })
})

// ---------------------------------------------------------------------------
// §8, §11 — SendHeader
// ---------------------------------------------------------------------------

describe('sendHeader (§8, §11)', () => {
  it('flushes an H at once on a unary call: header() returns before the response', async () => {
    const net = makeNet({ reliable: true })
    let release!: () => void
    const gate = new Promise<void>((res) => (release = res))
    net.server.register(echo.once, async (_req, ctx) => {
      await ctx.sendHeader({ early: ['yes'] })
      await gate
      return { text: 'late' }
    })

    // The generated unary path cannot observe a header mid-call, so drive the
    // unary shape (§8) through the stream API directly.
    const stream = net.conn.newStream(echo.once, {})
    await stream.send({ text: 'x' })

    // The handler is still parked on the gate: nothing but the flush could
    // have released this.
    expect(await stream.header()).toEqual({ early: ['yes'] })

    // On the wire: exactly one H — a unary call gets no creation ack — and it
    // precedes the terminal (§8).
    const hs = net.sentS2C.filter(isHeaderFrame)
    expect(hs).toHaveLength(1)
    expect(hs[0]!.header).toEqual({ early: ['yes'] })
    expect(net.sentS2C.some(isTerminal)).toBe(false)

    release()
    expect(await stream.recv()).toEqual({ text: 'late' })
    expect(net.sentS2C.some(isTerminal)).toBe(true)
  })

  it('flushing twice is INTERNAL, and the losing metadata never reaches the client', async () => {
    const net = makeNet({ reliable: true })
    const errs: (StatusError | undefined)[] = []
    const capture = async (fn: () => void | Promise<void>): Promise<void> => {
      try {
        await fn()
        errs.push(undefined)
      } catch (e) {
        errs.push(e as StatusError)
      }
    }
    net.server.register(echo.once, async (_req, ctx) => {
      await capture(() => ctx.sendHeader({ flush: ['first'] }))
      await capture(() => ctx.sendHeader({ flush: ['second'] }))
      await capture(() => ctx.setHeader({ late: ['set'] })) // grpc-go's ErrIllegalHeaderWrite
      return { text: 'ok' }
    })

    let header
    const res = (await net.conn.invoke(echo.once, { text: 'x' }, { onHeader: (h) => (header = h) })) as TestRes
    expect(res.text).toBe('ok')
    expect(errs[0]).toBeUndefined()
    expect(errs[1]?.code).toBe(Code.INTERNAL)
    expect(errs[1]?.desc).toContain('multiple times')
    expect(errs[2]?.code).toBe(Code.INTERNAL)
    expect(header).toEqual({ flush: ['first'] })
  })

  it('the core’s creation ack is not a flush: a streaming handler’s first sendHeader still succeeds', async () => {
    const net = makeNet({ reliable: true })
    const errs: (StatusError | undefined)[] = []
    net.server.register(echo.many, async (_req, stream, ctx) => {
      // The core already sent the creation-ack H for this call (§8) — if that
      // counted as a flush, this would fail.
      try {
        await ctx.sendHeader({ flush: ['first'] })
        errs.push(undefined)
      } catch (e) {
        errs.push(e as StatusError)
      }
      try {
        await ctx.sendHeader({ flush: ['second'] })
        errs.push(undefined)
      } catch (e) {
        errs.push(e as StatusError)
      }
      await stream.send({ text: 'x' })
    })

    const stream = net.conn.newStream(echo.many, {})
    await stream.send({ text: 'go', n: 1 })
    expect(await stream.header()).toEqual({ flush: ['first'] })
    expect(await stream.recv()).toEqual({ text: 'x' })
    expect(await stream.recv()).toBeUndefined()

    expect(errs[0]).toBeUndefined()
    expect(errs[1]?.code).toBe(Code.INTERNAL)

    // On the wire: the ack H carries no header field — an ack must not pin the
    // header to empty (§8) — and the flushed H that follows carries it.
    const hs = net.sentS2C.filter(isHeaderFrame)
    expect(hs.length).toBeGreaterThanOrEqual(2)
    expect(hs[0]!.header).toBeUndefined()
    expect(hs[1]!.header).toEqual({ flush: ['first'] })
  })
})
