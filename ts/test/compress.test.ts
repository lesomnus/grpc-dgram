// Message compression (PROTOCOL.md §12.1) — the TS twin of the Go
// compress_test.go coverage: the per-call `compressor` named on the OPEN, and
// the COMPRESSED modifier bit that says a given frame's payload actually is
// compressed.
//
//   - the compressor governs the WHOLE call, both directions, like the codec
//     (§12, §12.1): it is named on the OPEN only, and every message frame of
//     all four RPC types rides compressed;
//   - a payload that would GROW — empty, tiny, or high-entropy — is sent raw,
//     without the flag and without expansion (§12.1);
//   - an unknown compressor at the server draws T{UNIMPLEMENTED}, like an
//     unknown codec; the client's own registry guard fails the call locally
//     before anything reaches the wire;
//   - decompression is bounded by the receive cap and fails
//     RESOURCE_EXHAUSTED past it, so a compression bomb costs nothing;
//   - COMPRESSED is a MODIFIER, not a shape (§7.1): a compressed unary /
//     SendAndClose response rides the terminal frame, whose shape is still
//     CLOSE — the regression SHAPE_MASK exists for.
//
// 'gzip' is the interop baseline; the platform's CompressionStream provides it
// on the client, and a server serves exactly what it registered.

import { describe, expect, it } from 'vitest'
import { unaryMethod, type PayloadCodec } from '../src/desc'
import { Server, type ServerOptions } from '../src/server'
import { Code, type StatusError } from '../src/status'
import { getCompressor } from '../src/util'
import { FlagClose, FlagCompressed, FlagOpen, frame, isHeaderFrame, isOpen, isTerminal, shapeOf, type Frame } from '../src/wire'
import { echo, makeNet, registerEcho, tick, wireClone, type TestRes } from '../src/testing'

const gzip = getCompressor('gzip')

// The whole file needs the platform baseline; a runtime without it would make
// every assertion below vacuous rather than red.
if (gzip === undefined) throw new Error('this runtime provides no gzip CompressionStream: §12.1 interop baseline missing')

// gzipNet is a reliable pipe whose server serves the gzip baseline (§12.1) —
// the twin of Go's czPipe. Compression is orthogonal to the datagram
// machinery, so these tests want a lossless, timer-free channel: every
// recorded frame is one the core deliberately sent.
const gzipNet = (opts: ServerOptions = {}) => makeNet({ reliable: true, serverOpts: { compressors: { gzip }, ...opts } })

// big is highly compressible: gzip takes it from ~470 B to ~55 B, so "was this
// frame compressed?" is unambiguous on the wire.
const big = 'Royale with Cheese '.repeat(24)

const enc = (v: unknown) => new TextEncoder().encode(JSON.stringify(v))

// messages keeps the frames that carry a message. Payload presence is what
// makes a frame a message (§7.1): creation acks, half-closes, WINDOW grants
// and an error terminal carry none.
const messages = (fs: readonly Frame[]): Frame[] => fs.filter((f) => f.payload !== undefined)

// assertCompressed asserts that fs is exactly want message frames, each
// carrying COMPRESSED over a legal, payload-bearing shape (§7.1): the modifier
// never replaces the shape it rides on.
function assertCompressed(fs: readonly Frame[], want: number): void {
  expect(fs).toHaveLength(want)
  for (const f of fs) {
    expect(f.flags & FlagCompressed).toBe(FlagCompressed)
    expect([0, FlagClose, FlagOpen | FlagClose]).toContain(shapeOf(f))
  }
}

// assertCompressorOnOpenOnly pins §12.1's "named on OPEN only": the compressor
// addresses the call, not the frame, so no later client frame and no server
// frame repeats it.
function assertCompressorOnOpenOnly(net: ReturnType<typeof makeNet>, name: string): void {
  const open = net.sentC2S.find(isOpen)
  expect(open).toBeDefined()
  expect(open!.compressor).toBe(name)
  for (const f of net.sentC2S) {
    if (isOpen(f)) continue
    expect(f.compressor).toBe('')
  }
  for (const f of net.sentS2C) expect(f.compressor).toBe('')
}

// incompressible builds a deterministic high-entropy printable string. gzip's
// own framing costs more than Huffman coding saves on it, so §12.1's "skip
// compression that would expand the payload" applies. The fixture's
// precondition is asserted, not assumed (assertWouldGrow).
function incompressible(n: number): string {
  const alphabet = '0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ!#$%&()*+,-./:;<=>?@[]^_{|}~'
  let s = 0x5eed >>> 0
  let out = ''
  for (let i = 0; i < n; i++) {
    s = (s * 1664525 + 1013904223) >>> 0
    out += alphabet[s % alphabet.length]
  }
  return out
}

// assertWouldGrow checks the FIXTURE, not the core: gzipping raw really does
// produce at least as many bytes, so a raw frame below is the rule of §12.1
// and not an accident of the data.
async function assertWouldGrow(raw: Uint8Array): Promise<void> {
  const out = await gzip!.compress(raw)
  expect(out.length).toBeGreaterThanOrEqual(raw.length)
}

// ---------------------------------------------------------------------------
// §12.1: the compressor is named on the OPEN and governs the whole call in
// both directions, like the codec — for every RPC type of §8. The messages
// decode back exactly; the wire shows COMPRESSED on every message frame.
// ---------------------------------------------------------------------------

describe('gzip round-trip, all four RPC types (§12.1)', () => {
  it('unary: the request rides OPEN|CLOSE and the response rides T, both compressed', async () => {
    const net = gzipNet()
    const res = await net.conn.invoke(echo.once, { text: big }, { compressor: 'gzip' })
    expect(res).toEqual({ text: `echo:${big}` })

    const tx = messages(net.sentC2S)
    assertCompressed(tx, 1)
    expect(tx[0]!.flags).toBe(FlagOpen | FlagClose | FlagCompressed)
    expect(tx[0]!.payload!.length).toBeLessThan(enc({ text: big }).length) // the wire payload is the compressed one

    const rx = messages(net.sentS2C)
    assertCompressed(rx, 1)
    // COMPRESSED is a modifier: the terminal's SHAPE is still CLOSE (§7.1) —
    // a receiver comparing the whole bitmask would drop it and hang the call.
    expect(rx[0]!.flags).toBe(FlagClose | FlagCompressed)
    expect(shapeOf(rx[0]!)).toBe(FlagClose)
    expect(rx[0]!.code).toBe(Code.OK)

    assertCompressorOnOpenOnly(net, 'gzip')
  })

  it('server streaming: every response data frame is compressed, the empty terminal is not', async () => {
    const net = gzipNet()
    const stream = net.conn.newStream(echo.many, { compressor: 'gzip' })
    await stream.send({ text: big, n: 3 })
    for (let i = 0; i < 3; i++) expect(await stream.recv()).toEqual({ text: `${big}#${i}` })
    expect(await stream.recv()).toBeUndefined()

    assertCompressed(messages(net.sentC2S), 1)
    const rx = messages(net.sentS2C)
    assertCompressed(rx, 3)
    for (const f of rx) expect(f.flags).toBe(FlagCompressed) // a compressed data frame has shape 0

    // The payload-less terminal is never compressed: there is nothing to
    // compress, and the flag would lie about the frame (§12.1).
    const term = net.sentS2C.find(isTerminal)!
    expect(term.flags).toBe(FlagClose)

    assertCompressorOnOpenOnly(net, 'gzip')
  })

  it('client streaming: the requests are data frames, the response rides the terminal', async () => {
    const net = gzipNet()
    net.server.register(echo.count, async (stream) => {
      const parts: string[] = []
      for await (const msg of stream) parts.push(msg.text)
      return { text: parts.join('|') }
    })

    const stream = net.conn.newStream(echo.count, { compressor: 'gzip' })
    await stream.send({ text: big })
    await stream.send({ text: big })
    stream.closeSend()
    expect(await stream.recv()).toEqual({ text: `${big}|${big}` })
    expect(await stream.recv()).toBeUndefined()

    // The eager OPEN carries no payload (§8): the messages are data frames.
    const tx = messages(net.sentC2S)
    assertCompressed(tx, 2)
    for (const f of tx) expect(f.flags).toBe(FlagCompressed)

    const rx = messages(net.sentS2C)
    assertCompressed(rx, 1)
    expect(rx[0]!.flags).toBe(FlagClose | FlagCompressed) // SendAndClose response on the terminal
    expect(shapeOf(rx[0]!)).toBe(FlagClose)

    assertCompressorOnOpenOnly(net, 'gzip')
  })

  it('bidi: both directions ride compressed', async () => {
    const net = gzipNet()
    const stream = net.conn.newStream(echo.live, { compressor: 'gzip' })
    for (let i = 0; i < 2; i++) {
      await stream.send({ text: `${big}${i}` })
      expect(await stream.recv()).toEqual({ text: `echo:${big}${i}` })
    }
    stream.closeSend()
    expect(await stream.recv()).toBeUndefined()

    assertCompressed(messages(net.sentC2S), 2)
    assertCompressed(messages(net.sentS2C), 2)

    assertCompressorOnOpenOnly(net, 'gzip')
  })
})

// ---------------------------------------------------------------------------
// §12.1: the decision is PER FRAME. A payload that compression would grow —
// empty, tiny, or high-entropy — travels raw: no COMPRESSED flag, and the
// bytes on the wire are the message itself, never a larger encoding of it.
// ---------------------------------------------------------------------------

describe('a payload that would grow stays raw (§12.1)', () => {
  // A codec whose messages marshal to zero bytes: a 0-byte message is
  // meaningful (§5, §7), and a gzip header here would turn "no bytes" into
  // "some bytes".
  const emptyCodec: PayloadCodec<null> = { marshal: () => new Uint8Array(0), unmarshal: () => null }
  const nop = unaryMethod<null, null>('/test.Echo/Nop', { request: emptyCodec, response: emptyCodec })

  it('an empty payload is never compressed, in either direction', async () => {
    const net = gzipNet()
    net.server.register(nop, () => null)

    expect(await net.conn.invoke(nop, null, { compressor: 'gzip' })).toBeNull()

    const tx = messages(net.sentC2S)
    expect(tx).toHaveLength(1)
    expect(tx[0]!.flags).toBe(FlagOpen | FlagClose) // present, empty, uncompressed
    expect(tx[0]!.payload).toHaveLength(0)

    const rx = messages(net.sentS2C)
    expect(rx).toHaveLength(1)
    expect(rx[0]!.flags).toBe(FlagClose)
    expect(rx[0]!.payload).toHaveLength(0)
  })

  it('a tiny payload is sent raw, byte-identically', async () => {
    const net = gzipNet()
    const raw = enc({ text: 'hi' })
    await assertWouldGrow(raw)

    expect(await net.conn.invoke(echo.once, { text: 'hi' }, { compressor: 'gzip' })).toEqual({ text: 'echo:hi' })

    const tx = messages(net.sentC2S)
    expect(tx).toHaveLength(1)
    expect(tx[0]!.flags).toBe(FlagOpen | FlagClose)
    expect(tx[0]!.payload).toEqual(raw)
    // The response is tiny too, so the server made the same decision.
    expect(messages(net.sentS2C)[0]!.flags).toBe(FlagClose)
  })

  it('an incompressible payload is sent raw and never expands', async () => {
    const net = gzipNet()
    const msg = incompressible(192)
    const raw = enc({ text: msg })
    await assertWouldGrow(raw)

    expect(await net.conn.invoke(echo.once, { text: msg }, { compressor: 'gzip' })).toEqual({ text: `echo:${msg}` })

    const tx = messages(net.sentC2S)
    expect(tx).toHaveLength(1)
    expect(tx[0]!.flags).toBe(FlagOpen | FlagClose) // compression that would expand is skipped
    expect(tx[0]!.payload).toEqual(raw)
  })
})

// ---------------------------------------------------------------------------
// §12.1: an unknown compressor at the server draws T{UNIMPLEMENTED}, exactly
// like an unknown codec (§12) — nothing silently degrades, and no handler
// runs. The client's own registry is checked locally first, so a compressor it
// cannot use never reaches the wire at all.
// ---------------------------------------------------------------------------

describe('an unknown compressor (§12.1)', () => {
  it('draws T{UNIMPLEMENTED} at the server, before the request is decoded', async () => {
    const sent: Frame[] = []
    const server = new Server({ handle: (f: Frame) => void sent.push(wireClone(f)) }, { reliable: true, compressors: { gzip } })
    const counts = registerEcho(server)

    // A unary OPEN|CLOSE (§8) naming a compressor this build does not have.
    // Its payload is plain — the name alone must be refused.
    await server.handle(
      frame({
        epoch: 0xc0ffee,
        sid: 1,
        seq: 1,
        flags: FlagOpen | FlagClose,
        method: echo.once.path,
        compressor: 'brotli-9',
        payload: enc({ text: 'hi' }),
      }),
      { peer: 'p' },
    )
    await tick()

    const term = sent.find(isTerminal)
    expect(term).toBeDefined()
    expect(term!.flags).toBe(FlagClose)
    expect(term!.code).toBe(Code.UNIMPLEMENTED)
    expect(term!.desc).toContain('brotli-9')
    expect(term!.sid).toBe(1)
    expect(counts.once).toBe(0) // the handler must never run
    await server.stop()
  })

  it('is refused even when the platform has it, if the server never registered it', async () => {
    // A server serves exactly what it registered, the way a grpc-go server
    // serves exactly the compressors its binary imported (§12.1).
    const net = makeNet({ reliable: true })
    const err = (await net.conn.invoke(echo.once, { text: big }, { compressor: 'gzip' }).catch((e) => e)) as StatusError
    expect(err.code).toBe(Code.UNIMPLEMENTED)
    expect(err.desc).toContain('gzip')
  })

  it('fails the call locally at the client, with nothing on the wire', async () => {
    const net = gzipNet()
    const err = (await net.conn.invoke(echo.once, { text: big }, { compressor: 'brotli-9' }).catch((e) => e)) as StatusError
    expect(err.code).toBe(Code.INTERNAL)
    expect(err.desc).toContain('brotli-9')
    expect(net.sentC2S).toEqual([])
  })
})

// ---------------------------------------------------------------------------
// §12.1: a receiver MUST bound decompression by its receive size cap and fail
// RESOURCE_EXHAUSTED past it — the expansion is read one byte past the cap,
// never materialized, so a compression bomb costs nothing. Both roles.
// ---------------------------------------------------------------------------

describe('decompression is bounded by the receive cap (§12.1)', () => {
  const recvCap = 4096
  // 1 MiB that gzips to ~1 kB: the frame on the wire is tiny, the message
  // behind it is 256x the cap.
  const bomb = 'a'.repeat(1 << 20)

  it('at the client', async () => {
    const net = gzipNet()
    const err = (await net.conn.invoke(echo.once, { text: bomb }, { compressor: 'gzip', maxRecvMsgSize: recvCap }).catch((e) => e)) as StatusError
    expect(err.code).toBe(Code.RESOURCE_EXHAUSTED)
    // The cap must stop the DECOMPRESSION, not the decompressed message.
    expect(err.desc).toContain('after decompression')

    // The wire payload was far UNDER the cap; only the expansion is over it.
    const term = net.sentS2C.find(isTerminal)!
    expect(term.flags & FlagCompressed).toBe(FlagCompressed)
    expect(term.payload!.length).toBeLessThan(recvCap)
  })

  it('at the server', async () => {
    const net = gzipNet({ maxRecvMsgSize: recvCap })
    const err = (await net.conn.invoke(echo.once, { text: bomb }, { compressor: 'gzip' }).catch((e) => e)) as StatusError
    expect(err.code).toBe(Code.RESOURCE_EXHAUSTED)
    expect(err.desc).toContain('after decompression')
    // It is the SERVER that refused: the status travelled as a terminal (§8).
    const term = net.sentS2C.find(isTerminal)!
    expect(term.code).toBe(Code.RESOURCE_EXHAUSTED)
  })
})

// ---------------------------------------------------------------------------
// §12.1 / §8: a compressed call's frames still route by SHAPE. The creation
// ack of a streaming call carries no payload and no COMPRESSED bit, and the
// terminal that closes a compressed unary call is still a terminal.
// ---------------------------------------------------------------------------

describe('COMPRESSED is a modifier, not a shape (§7.1)', () => {
  it('the creation ack of a compressed streaming call is a plain H', async () => {
    const net = gzipNet()
    const stream = net.conn.newStream(echo.live, { compressor: 'gzip' })
    await tick()
    const ack = net.sentS2C.find(isHeaderFrame)
    expect(ack).toBeDefined()
    expect(ack!.flags).toBe(0)
    expect(ack!.compressor).toBe('')
    stream.cancel()
  })

  it('a compressed terminal is delivered, not dropped', async () => {
    const net = gzipNet()
    // The response is the only thing that could be lost by a receiver
    // comparing the whole bitmask instead of masking with SHAPE_MASK.
    const res = (await net.conn.invoke(echo.once, { text: big }, { compressor: 'gzip' })) as TestRes
    expect(res.text).toBe(`echo:${big}`)
    const term = net.sentS2C.find(isTerminal)!
    expect(term.flags).toBe(FlagClose | FlagCompressed)
    expect(shapeOf(term)).toBe(FlagClose)
    expect(term.payload).toBeDefined()
  })
})
