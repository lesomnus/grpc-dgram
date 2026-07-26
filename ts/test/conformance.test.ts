// Cross-language conformance: a TypeScript client drives a REAL Go
// drpc.Server over UDP, proving the two implementations agree on the wire
// format AND the behavior — method dispatch, the OPEN/CLOSE/seq/epoch state
// machine, the proto codec, all four RPC shapes, and the wire v1.1 surfaces
// (binary metadata, status details, per-stream flow control, compression).
// The TS client uses descriptors derived from the generated EchoService
// (fromService); the Go server is conformance/udpserver, serving internal/echo
// over transport/udp. Skipped when `go` is unavailable.
//
// This is the runtime counterpart to the static §5 golden-byte vectors: those
// pin the encoding, this pins the conversation.
//
// The Go fixture announces TWO endpoints: the unreliable one (drpc's default
// mode) and a second one whose per-frame mode annotation is overridden to
// reliable, because per-stream flow control (§4.2.1) exists in reliable mode
// only and cannot otherwise be exercised across implementations. Loopback UDP
// carries the handful of small datagrams the reliable cases send without loss
// or reordering, so no separate reliable transport (and no extra dependency)
// is needed on either side.
//
// Every case below is written so a Go/TS disagreement CANNOT pass: the bytes
// each side expects are hard-coded on BOTH sides rather than compared against
// what the other side just sent. That matters most for binary metadata, where
// Go holds raw octets in a string and TS holds their base64 — a mirror-shaped
// assertion ("what I sent came back") would pass even if both implementations
// were wrong about which of the two goes on the wire.

import { create, fromBinary } from '@bufbuild/protobuf'
import { createClient } from '@connectrpc/connect'
import { execFileSync, spawn, type ChildProcessWithoutNullStreams } from 'node:child_process'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'
import { afterAll, beforeAll, describe, expect, it } from 'vitest'
import { Conn, statusDetails } from '../src/conn'
import { unaryMethod } from '../src/desc'
import { decodeBase64, type Metadata } from '../src/metadata'
import { Code } from '../src/status'
import { createDrpcTransport } from '../src/transport/connect'
import { dialUdp, type UdpTransport } from '../src/transport/node-udp'
import { fromService } from '../src/transport/protobuf-es'
import type { Compressor } from '../src/util'
import { encodeFrame, FlagClose, FlagCompressed, FlagOpen, FlagWindow, shapeOf, type Frame } from '../src/wire'
import { EchoRequestSchema, EchoResponseSchema, EchoService } from '../src/testing/gen/echo/echo_pb.js'

const repoRoot = resolve(process.cwd(), '..')
const Echo = fromService(EchoService)

const ascii = (s: string): Uint8Array => new TextEncoder().encode(s)

// ---------------------------------------------------------------------------
// the octets both implementations pin, independently
// ---------------------------------------------------------------------------
//
// The same constants appear in conformance/udpserver/main.go, spelled in Go's
// representation (raw bytes inside a string). Neither side derives them from
// the other: the Go handler fails the call when what it receives differs, and
// the assertions here fail when what arrives differs. Both the raw octets and
// their base64 are written out, so a base64-for-raw mix-up on EITHER side is
// caught — the TS API holds a "-bin" value as base64 while the wire holds the
// octets, and only pinning both forms can tell the two apart.
//
// The binary values are deliberately illegal UTF-8 (0xff/0xfe never occur in
// UTF-8; 0x80/0xc0 are a lone continuation byte and a truncated lead byte), so
// a peer that treated them as text would replace bytes with U+FFFD and could
// not reproduce them.

const KEY_REQ_BIN = 'x-conf-req-bin'
const KEY_REQ_TEXT = 'x-conf-req-text'
const KEY_HDR_BIN = 'x-conf-hdr-bin'
const KEY_HDR_TEXT = 'x-conf-hdr-text'
const KEY_ECHO_BIN = 'x-conf-echo-bin'
const KEY_TRL_BIN = 'x-conf-trl-bin'
const KEY_TRL_TEXT = 'x-conf-trl-text'

const REQ_BIN = new Uint8Array([0x00, 0x01, 0x02, 0x80, 0xfe, 0xff, 0x72, 0x65, 0x71, 0xc2, 0x00])
const REQ_BIN_B64 = 'AAECgP7/cmVxwgA='
const HDR_BIN = new Uint8Array([0x00, 0xff, 0xfe, 0x80, 0xc0, 0x64, 0x72, 0x70, 0x63, 0x7f, 0x0a])
const HDR_BIN_B64 = 'AP/+gMBkcnBjfwo='
const TRL_BIN = new Uint8Array([0xff, 0xd8, 0x00, 0x1b, 0x80, 0x74, 0x72, 0x6c, 0x72, 0xfe, 0x00])
const TRL_BIN_B64 = '/9gAG4B0cmxy/gA='

// Text values: printable ASCII (0x20..0x7E), all a non-"-bin" key may carry on
// either stack (§11). They must survive unchanged and un-base64'd.
const REQ_TEXT = `conformance request !"#$%&'()*+,-./09:;<=>?@AZ[\\]^_az{|}~ `
const HDR_TEXT = `conformance header !"#$%&'()*+,-./09:;<=>?@AZ[\\]^_az{|}~ `
const TRL_TEXT = 'conformance trailer 0x20..0x7E'

// The marshaled google.rpc.ErrorInfo the Go server attaches to its status
// details, byte for byte: field 1 (reason) then field 2 (domain), both
// length-delimited. The TS core carries no protobuf runtime and has no schema
// for google.rpc.*, so the Any's payload is pinned as bytes — which is also
// the strictest possible statement of "these details crossed intact".
const ERROR_INFO = new Uint8Array([0x0a, 0x0b, ...ascii('CONFORMANCE'), 0x12, 0x10, ...ascii('drpc.conformance')])

// gzipBody is ~2 kB of compressible text: over the 1200-byte datagram limit
// raw (both adapters refuse a larger envelop, §4.4), a few dozen bytes gzipped.
// A call carrying it can only complete if BOTH implementations compress.
const gzipBody = 'drpc-conformance-gzip-'.repeat(96)

// A compressor name the Go server does not have. Registering it locally is
// what makes the OPEN reach the server at all — an unknown name fails the call
// locally with INTERNAL before it starts (§12.1) — so the server's
// T{UNIMPLEMENTED} is what the call ends on. Identity: the core skips a
// compression that would not shrink the payload, so nothing on the wire
// depends on the implementation.
const passthrough: Compressor = { compress: (d) => d, decompress: (d) => d }

const canGzip = typeof CompressionStream !== 'undefined' && typeof DecompressionStream !== 'undefined'

function hasGo(): boolean {
  try {
    execFileSync('go', ['version'], { stdio: 'ignore' })
    return true
  } catch {
    return false
  }
}

// startGoServer builds conformance/udpserver once, spawns it, and resolves
// with the two UDP ports it announces on stdout.
async function startGoServer(bin: string): Promise<{ ports: { unreliable: number; reliable: number }; proc: ChildProcessWithoutNullStreams }> {
  const proc = spawn(bin, [], { cwd: repoRoot })
  const ports = await new Promise<{ unreliable: number; reliable: number }>((res, rej) => {
    let buf = ''
    const to = setTimeout(() => rej(new Error(`go server did not announce its ports; stderr:\n${errBuf}`)), 8000)
    let errBuf = ''
    proc.stderr.on('data', (d) => (errBuf += String(d)))
    proc.stdout.on('data', (d) => {
      buf += String(d)
      const u = buf.match(/^PORT (\d+)$/m)
      const r = buf.match(/^PORT_RELIABLE (\d+)$/m)
      if (u && r) {
        clearTimeout(to)
        res({ unreliable: Number(u[1]), reliable: Number(r[1]) })
      }
    })
    proc.on('exit', (code) => rej(new Error(`go server exited early (${code}); stderr:\n${errBuf}`)))
  })
  return { ports, proc }
}

// ---------------------------------------------------------------------------
// wire recorder
// ---------------------------------------------------------------------------

// Wire is every frame this endpoint put on (tx) or took off (rx) the socket,
// in order. Flags, windows and compressor names are otherwise invisible from
// the API surface, and they are precisely what the two implementations have to
// agree on — "the call succeeded" cannot tell a missing window advertisement
// or a skipped compression from a correct one.
interface Wire {
  tx: Frame[]
  rx: Frame[]
}

// record wraps the transport's send and the Conn's receive with a tap. Both
// are ordinary method properties looked up at call time, so an instance-level
// wrapper sees every frame without touching src/.
function record(transport: UdpTransport, conn: Conn): Wire {
  const w: Wire = { tx: [], rx: [] }
  const send = transport.handle.bind(transport)
  transport.handle = (f) => {
    w.tx.push({ ...f }) // snapshot: flags may still be mutated by the sender
    return send(f)
  }
  const recv = conn.handle.bind(conn)
  conn.handle = (f, ctx) => {
    w.rx.push({ ...f })
    return recv(f, ctx)
  }
  return w
}

const openOf = (fs: readonly Frame[]): Frame => {
  const f = fs.find((x) => (x.flags & FlagOpen) !== 0)
  if (f === undefined) throw new Error('no OPEN frame was recorded')
  return f
}

const terminalOf = (fs: readonly Frame[]): Frame => {
  const f = fs.find((x) => (x.flags & FlagClose) !== 0 && x.code !== undefined)
  if (f === undefined) throw new Error('no terminal frame was recorded')
  return f
}

const grantsOf = (fs: readonly Frame[]): Frame[] => fs.filter((f) => shapeOf(f) === FlagWindow)

// indexOfBytes finds needle in hay, or -1. Used to assert what the ENCODED
// frame carries, which is the only place the metadata representation boundary
// is visible from this side.
function indexOfBytes(hay: Uint8Array, needle: Uint8Array): number {
  outer: for (let i = 0; i + needle.length <= hay.length; i++) {
    for (let j = 0; j < needle.length; j++) {
      if (hay[i + j] !== needle[j]) continue outer
    }
    return i
  }
  return -1
}

describe.skipIf(!hasGo())('cross-language conformance (TS client ↔ Go server over UDP)', () => {
  let bin: string
  let proc: ChildProcessWithoutNullStreams
  let conn: Conn
  let wire: Wire
  let relConn: Conn
  let relWire: Wire

  beforeAll(async () => {
    bin = join(mkdtempSync(join(tmpdir(), 'drpc-conf-')), 'udpserver')
    execFileSync('go', ['build', '-o', bin, './conformance/udpserver'], { cwd: repoRoot, stdio: 'pipe' })
    const started = await startGoServer(bin)
    proc = started.proc

    const transport = await dialUdp(started.ports.unreliable)
    conn = new Conn(transport, { compressors: { 'x-conformance-nope': passthrough } })
    wire = record(transport, conn) // discovers unreliable mode from the transport

    const relTransport = await dialUdp(started.ports.reliable)
    relConn = new Conn(relTransport, { reliable: true })
    relWire = record(relTransport, relConn)
  }, 60_000)

  afterAll(() => {
    conn?.close()
    relConn?.close()
    proc?.stdin.end() // stdin EOF → the Go server tears down cleanly
    proc?.kill()
  })

  it('unary Once: server applies CircularShift', async () => {
    // Go CircularShift("hello", 2) = "hello"[2:] + "hello"[:2] = "llohe".
    const res = await conn.invoke(Echo.once, create(EchoRequestSchema, { message: 'hello', circularShift: 2 }))
    expect(res.message).toBe('llohe')
    expect(res.sequence).toBe(0)
  })

  it('unary Noop: the Go server echoes the request unchanged', async () => {
    // Noop returns the EchoRequest itself (response type is EchoRequest), so
    // request fields must survive the TS→Go→TS round-trip verbatim.
    const res = await conn.invoke(Echo.noop, create(EchoRequestSchema, { message: 'verbatim', circularShift: 7, repeat: 3 }))
    expect(res.message).toBe('verbatim')
    expect(res.circularShift).toBe(7)
    expect(res.repeat).toBe(3)
  })

  it('server-streaming Many: repeated shifts with ascending sequence', async () => {
    // v="abc"; each of 3 iterations shifts left by 1: bca, cab, abc.
    const stream = conn.newStream(Echo.many, {})
    await stream.send(create(EchoRequestSchema, { message: 'abc', repeat: 3, circularShift: 1 }))
    const got: Array<{ m: string; s: number }> = []
    for await (const res of stream) got.push({ m: res.message, s: res.sequence })
    expect(got).toEqual([
      { m: 'bca', s: 0 },
      { m: 'cab', s: 1 },
      { m: 'abc', s: 2 },
    ])
  })

  it('client-streaming Buff: batch accumulates across sends with shared sequence', async () => {
    const stream = conn.newStream(Echo.buff, {})
    await stream.send(create(EchoRequestSchema, { message: 'ab', repeat: 1, circularShift: 1 }))
    await stream.send(create(EchoRequestSchema, { message: 'xy', repeat: 1, circularShift: 1 }))
    stream.closeSend()
    const res = await stream.recv()
    expect(res?.items.map((i) => ({ m: i.message, s: i.sequence }))).toEqual([
      { m: 'ba', s: 0 },
      { m: 'yx', s: 1 },
    ])
    expect(await stream.recv()).toBeUndefined()
  })

  it('bidi Live: interleaved echo with ascending sequence, EOF after half-close', async () => {
    const stream = conn.newStream(Echo.live, {})
    await stream.send(create(EchoRequestSchema, { message: 'hi', repeat: 1, circularShift: 1 }))
    expect(await stream.recv()).toMatchObject({ message: 'ih', sequence: 0 })
    await stream.send(create(EchoRequestSchema, { message: 'yo', repeat: 1, circularShift: 1 }))
    expect(await stream.recv()).toMatchObject({ message: 'oy', sequence: 1 })
    stream.closeSend()
    expect(await stream.recv()).toBeUndefined()
  })

  it('server header/trailer metadata reaches the TS client (§11)', async () => {
    // handleMd echoes the incoming md plus timing:header on the header frame
    // and timing:trailer on the terminal, but only when request md is present.
    const stream = conn.newStream(Echo.many, { metadata: { client: ['ts-conformance'] } })
    await stream.send(create(EchoRequestSchema, { message: 'z', repeat: 1, circularShift: 0 }))
    const header = await stream.header()
    expect(header?.timing).toEqual(['header'])
    for await (const _ of stream) {
      /* drain to completion */
    }
    expect(stream.trailer()?.timing).toEqual(['trailer'])
  })

  it('unary responses carry a proto Timestamp encoded by Go and decoded in TS', async () => {
    const res = await conn.invoke(Echo.once, create(EchoRequestSchema, { message: 'x', circularShift: 0 }))
    // Go sets DateCreated = timestamppb.Now(); protobuf-es decodes it.
    expect(res.dateCreated).toBeDefined()
    expect(typeof res.dateCreated!.seconds).toBe('bigint')
    expect(res.dateCreated!.seconds > 0n).toBe(true)
    // reference the response schema so the import is load-bearing
    expect(EchoResponseSchema.typeName).toBe('echo.EchoResponse')
  })

  // Boundary cases the happy-path shapes above never exercise — the wire
  // contract points that can only be confirmed where the two implementations
  // meet (the encoding is already pinned byte-for-byte by the §5 golden
  // vectors; these pin the live interpretation).

  it('status codes cross the boundary: a Go-returned non-OK status decodes exactly (§7)', async () => {
    // req.status injects an arbitrary status the Go handler returns via
    // req.Error() — exercising the CLOSE code+desc channel, which every
    // happy-path call (all OK) leaves untested cross-language.
    const err = await conn
      .invoke(Echo.once, create(EchoRequestSchema, { message: 'x', status: { code: Code.NOT_FOUND, message: 'not here' } }))
      .catch((e) => e)
    expect(err.code).toBe(Code.NOT_FOUND)
    expect(err.desc).toBe('not here')
  })

  it('an unregistered method draws UNIMPLEMENTED from the Go server (§9.4, §13)', async () => {
    // Method dispatch + the rejection-terminal path, neither hit by the
    // registered shapes. A raw codec suffices — Go rejects on method
    // resolution before decoding the payload.
    const raw = { marshal: (v: Uint8Array) => v, unmarshal: (b: Uint8Array) => b }
    const Nope = unaryMethod<Uint8Array, Uint8Array>('/echo.EchoService/DoesNotExist', { request: raw, response: raw })
    const err = await conn.invoke(Nope, new Uint8Array()).catch((e) => e)
    expect(err.code).toBe(Code.UNIMPLEMENTED)
  })

  it('edge payloads round-trip through the Go proto codec (§5 presence, §12)', async () => {
    // 0-byte string (payload present but empty), multi-byte UTF-8, and a
    // larger-but-in-datagram message — all with circular_shift 0 so the value
    // is echoed verbatim, proving payload encoding agrees both directions.
    const echoOf = async (message: string) =>
      (await conn.invoke(Echo.once, create(EchoRequestSchema, { message, circularShift: 0 }))).message
    expect(await echoOf('')).toBe('')
    expect(await echoOf('안녕 🌍 世界')).toBe('안녕 🌍 世界')
    const big = 'x'.repeat(500)
    expect(await echoOf(big)).toBe(big)
  })

  // -------------------------------------------------------------------------
  // wire v1.1
  // -------------------------------------------------------------------------

  it('binary metadata is RAW BYTES on the wire and base64 in the TS API (§11)', async () => {
    // The divergence this is built to catch: Go holds the octets of a "-bin"
    // value in a string, TS holds their base64. Both send the same bytes — but
    // an implementation that base64'd onto the wire (or UTF-8'd the octets)
    // would still round-trip against itself. So: the Go handler compares what
    // it received against ITS OWN hard-coded copy and fails the call on any
    // difference, and every expectation here is a literal, never "what I sent".
    const at = wire.tx.length
    let header: Metadata | undefined
    let trailer: Metadata | undefined
    const res = await conn.invoke(Echo.once, create(EchoRequestSchema, { message: 'conf/md' }), {
      metadata: { [KEY_REQ_BIN]: [REQ_BIN_B64], [KEY_REQ_TEXT]: [REQ_TEXT] },
      onHeader: (md) => (header = md),
      onTrailer: (md) => (trailer = md),
    })
    // TS → Go: the handler verified both values byte for byte before answering.
    // Any mismatch would have come back as FAILED_PRECONDITION naming the hex.
    expect(res.message).toBe('md-ok')

    // What actually went on the wire: the octets themselves, NOT their base64,
    // and the text value unencoded.
    const openBytes = encodeFrame(openOf(wire.tx.slice(at)))
    expect(indexOfBytes(openBytes, REQ_BIN)).toBeGreaterThanOrEqual(0)
    expect(indexOfBytes(openBytes, ascii(REQ_BIN_B64))).toBe(-1)
    expect(indexOfBytes(openBytes, ascii(REQ_TEXT))).toBeGreaterThanOrEqual(0)

    // Go → TS, on the header: exact base64 AND exact octets. The bytes are
    // illegal UTF-8, so a text-decoded value could not produce them.
    expect(header?.[KEY_HDR_BIN]).toEqual([HDR_BIN_B64])
    expect(decodeBase64(header![KEY_HDR_BIN]![0]!)).toEqual(HDR_BIN)
    expect(header?.[KEY_HDR_TEXT]).toEqual([HDR_TEXT]) // a text key is never base64
    // The value the server received, returned verbatim: it survived Go's
    // wire → metadata.MD → wire round unchanged.
    expect(header?.[KEY_ECHO_BIN]).toEqual([REQ_BIN_B64])
    expect(decodeBase64(header![KEY_ECHO_BIN]![0]!)).toEqual(REQ_BIN)

    // Go → TS, on the terminal's trailer: same contract.
    expect(trailer?.[KEY_TRL_BIN]).toEqual([TRL_BIN_B64])
    expect(decodeBase64(trailer![KEY_TRL_BIN]![0]!)).toEqual(TRL_BIN)
    expect(trailer?.[KEY_TRL_TEXT]).toEqual([TRL_TEXT])
  })

  it('metadata validation matches grpc-go: the call fails locally, never the codec (§11)', async () => {
    // Same rule as Go's validateMD, checked before the call exists — a "-bin"
    // key takes any octets, a text key takes printable ASCII only, and a key
    // outside [0-9a-z-_.] is refused whatever it carries.
    const req = create(EchoRequestSchema, { message: 'conf/md' })
    const fails = async (metadata: Metadata) => (await conn.invoke(Echo.once, req, { metadata }).catch((e) => e)).code
    expect(await fails({ 'x-conf-req-text': [' not printable'] })).toBe(Code.INTERNAL)
    expect(await fails({ 'X-Conf-Upper': ['v'] })).toBe(Code.INTERNAL)
    expect(await fails({ 'x-conf-req-bin': ['not base64!'] })).toBe(Code.INTERNAL)
  })

  it('status details ride the terminal frame and decode in TS (§5)', async () => {
    const at = wire.rx.length
    const err = await conn
      .invoke(Echo.once, create(EchoRequestSchema, { message: 'conf/details', circularShift: 3, repeat: 9 }))
      .catch((e) => e)
    expect(err.code).toBe(Code.FAILED_PRECONDITION)
    expect(err.desc).toBe('conformance: rich status details')

    const details = statusDetails(err)
    expect(details).toHaveLength(2)
    // A type this side has no schema for: the Any is pinned byte for byte,
    // type URL included.
    expect(details![0]!.typeUrl).toBe('type.googleapis.com/google.rpc.ErrorInfo')
    expect(details![0]!.value).toEqual(ERROR_INFO)
    // A type this side DOES have: decoded with protobuf-es and read back, so
    // the detail is proven to be a live message and not opaque bytes.
    expect(details![1]!.typeUrl).toBe('type.googleapis.com/echo.EchoRequest')
    const echoed = fromBinary(EchoRequestSchema, details![1]!.value)
    expect(echoed.message).toBe('conf/details')
    expect(echoed.circularShift).toBe(3)
    expect(echoed.repeat).toBe(9)

    // They travelled on the terminal frame itself (field 17), not anywhere else.
    expect(terminalOf(wire.rx.slice(at)).details).toHaveLength(2)
  })

  it('a unary SendHeader flushes an H frame before the response (§8, §11)', async () => {
    // The v1.1 change: SendHeader now flushes at once even on a unary call, so
    // Header() returns while the handler is still working. The Go handler
    // flushes, then sits for 300 ms; it also asserts a SECOND SendHeader is
    // refused (grpc-go's ErrIllegalHeaderWrite) and fails the call if it is not.
    const at = wire.rx.length
    const stream = conn.newStream(Echo.once, {})
    await stream.send(create(EchoRequestSchema, { message: 'conf/slow-header' }))
    let responded = false
    const pending = stream.recv().then((r) => {
      responded = true
      return r
    })
    const header = await stream.header()
    expect(header?.['x-conf-phase']).toEqual(['header'])
    expect(responded).toBe(false)
    expect(await pending).toMatchObject({ message: 'late' })

    // It really was its own H frame: no shape flags, no payload (§7) — and the
    // core's creation ack is not a header flush, so the first header-bearing
    // frame is the flushed one.
    const h = wire.rx.slice(at).find((f) => f.header !== undefined)!
    expect(shapeOf(h)).toBe(0)
    expect(h.payload).toBeUndefined()
  })

  it.skipIf(!canGzip)('gzip compresses both directions against the Go server (§12.1)', async () => {
    // The payload is larger than a datagram raw, so this call cannot complete
    // unless the TS client compressed its request AND the Go server compressed
    // its response — "it round-tripped" is therefore proof of both.
    expect(gzipBody.length).toBeGreaterThan(1200)
    const txAt = wire.tx.length
    const rxAt = wire.rx.length
    const res = await conn.invoke(Echo.once, create(EchoRequestSchema, { message: gzipBody, circularShift: 0 }), { compressor: 'gzip' })
    expect(res.message).toBe(gzipBody)

    // The OPEN names the compressor for the whole call, and carries a
    // COMPRESSED payload that is a fraction of the message (§7, §12.1).
    const open = openOf(wire.tx.slice(txAt))
    expect(open.compressor).toBe('gzip')
    expect(open.flags & FlagCompressed).toBe(FlagCompressed)
    expect(open.payload!.length).toBeLessThan(400)
    // The Go terminal came back compressed too, under the same call-wide name
    // (which no server frame repeats — it is stated once, on the OPEN).
    const term = terminalOf(wire.rx.slice(rxAt))
    expect(term.flags & FlagCompressed).toBe(FlagCompressed)
    expect(term.compressor).toBe('')
    expect(term.payload!.length).toBeLessThan(400)
  })

  it('without a compressor the same payload does not fit a datagram (§4.4)', async () => {
    // The negative control for the case above: compression is what made it
    // fit, not a coincidence of sizes.
    const err = await conn.invoke(Echo.once, create(EchoRequestSchema, { message: gzipBody, circularShift: 0 })).catch((e) => e)
    expect(err.code).toBe(Code.RESOURCE_EXHAUSTED)
  })

  it('a compressor the Go server does not have draws T{UNIMPLEMENTED} (§12.1)', async () => {
    const err = await conn
      .invoke(Echo.once, create(EchoRequestSchema, { message: 'x' }), { compressor: 'x-conformance-nope' })
      .catch((e) => e)
    expect(err.code).toBe(Code.UNIMPLEMENTED)
    expect(err.desc).toContain('x-conformance-nope')
  })

  // The same Go server, now driven by a STANDARD Connect client over the drpc
  // transport — proving createClient(Service, transport) interoperates with a
  // real Go drpc.Server end to end.
  describe('via a Connect client (createDrpcTransport)', () => {
    it('unary Once', async () => {
      const client = createClient(EchoService, createDrpcTransport(conn))
      const res = await client.once({ message: 'hello', circularShift: 2 })
      expect(res.message).toBe('llohe')
    })

    it('server-streaming Many', async () => {
      const client = createClient(EchoService, createDrpcTransport(conn))
      const got: Array<{ m: string; s: number }> = []
      for await (const res of client.many({ message: 'abc', repeat: 3, circularShift: 1 })) {
        got.push({ m: res.message, s: res.sequence })
      }
      expect(got).toEqual([
        { m: 'bca', s: 0 },
        { m: 'cab', s: 1 },
        { m: 'abc', s: 2 },
      ])
    })

    it('client-streaming Buff and bidi Live', async () => {
      const client = createClient(EchoService, createDrpcTransport(conn))
      async function* reqs(msgs: string[]) {
        for (const m of msgs) yield { message: m, repeat: 1, circularShift: 1 }
      }
      const batch = await client.buff(reqs(['ab', 'xy']))
      expect(batch.items.map((i) => i.message)).toEqual(['ba', 'yx'])

      const echoed: string[] = []
      for await (const res of client.live(reqs(['hi', 'yo']))) echoed.push(res.message)
      expect(echoed).toEqual(['ih', 'oy'])
    })
  })

  // Placed last on purpose: it asserts over EVERY frame this endpoint has
  // exchanged so far, i.e. every case above.
  it('flow control is reliable-only: the unreliable endpoint never mentions a window (§4.2.1)', async () => {
    expect(conn.reliable).toBe(false)
    const at = wire.tx.length
    await conn.invoke(Echo.once, create(EchoRequestSchema, { message: 'nowindow', circularShift: 0 }))
    // The OPEN advertises no receive window: in unreliable mode a full buffer
    // drops by policy (§4.2) and nothing paces a sender.
    expect(openOf(wire.tx.slice(at)).window).toBe(0)
    // And no frame of this session, in either direction, carries a window or
    // the WINDOW shape — including everything the Go server sent, so a server
    // that advertised one on its creation ack would be caught here.
    expect(grantsOf(wire.tx)).toEqual([])
    expect(grantsOf(wire.rx)).toEqual([])
    expect(wire.tx.filter((f) => f.window !== 0)).toEqual([])
    expect(wire.rx.filter((f) => f.window !== 0)).toEqual([])
  })

  // -------------------------------------------------------------------------
  // reliable mode: the same Go service on the second endpoint (§4.2.1)
  // -------------------------------------------------------------------------

  describe('reliable mode (per-stream flow control)', () => {
    it('client→server: the OPEN advertises a window, the ack answers with one, Go grants credit', async () => {
      expect(relConn.reliable).toBe(true)
      const txAt = relWire.tx.length
      const rxAt = relWire.rx.length

      // 40 messages under the 32-message window the ack advertises below: a
      // sender that got no credit parks at 32 and the call dies at T_stall, so
      // completing under a non-zero advertisement means Go granted and TS
      // consumed the grants. (A server advertising 0 would turn flow control
      // off instead of stalling — which is why the advertisement itself is
      // asserted, not just the completion.) repeat:0 keeps the batch response
      // empty, so the answer stays inside one datagram.
      const stream = relConn.newStream(Echo.buff, {})
      for (let i = 0; i < 40; i++) {
        await stream.send(create(EchoRequestSchema, { message: 'x', repeat: 0 }))
      }
      stream.closeSend()
      expect((await stream.recv())?.items).toEqual([])
      expect(await stream.recv()).toBeUndefined()

      const tx = relWire.tx.slice(txAt)
      const rx = relWire.rx.slice(rxAt)
      // The advertisement: the client's rx buffer, floored at W_init = 32.
      expect(openOf(tx).window).toBe(32)
      // The server's own, on the creation-ack H — which is NOT a header flush,
      // so its header field stays absent (§8).
      const ack = rx.find((f) => shapeOf(f) === 0 && f.payload === undefined)!
      expect(ack.window).toBe(32)
      expect(ack.header).toBeUndefined()

      const grants = grantsOf(rx)
      expect(grants.length).toBeGreaterThanOrEqual(1)
      for (const g of grants) {
        // A WINDOW frame is stateless: this sid, no seq, no payload, credit > 0.
        expect(g.sid).toBe(stream.sid)
        expect(g.seq).toBe(0)
        expect(g.payload).toBeUndefined()
        expect(g.window).toBeGreaterThan(0)
      }
      // Grants are batched at half the window, as HTTP/2 stacks do.
      expect(grants.reduce((n, g) => n + g.window, 0)).toBeGreaterThanOrEqual(8)
    }, 30_000)

    it('server→client: the TS client grants credit for what it consumed', async () => {
      const txAt = relWire.tx.length
      // 40 responses under the client's 32-message window: the Go server has
      // to park and wait for TS grants to finish the stream.
      const stream = relConn.newStream(Echo.many, {})
      await stream.send(create(EchoRequestSchema, { message: 'ab', repeat: 40, circularShift: 1 }))
      const got: string[] = []
      for await (const res of stream) got.push(res.message)
      expect(got).toHaveLength(40)
      expect(got.slice(0, 3)).toEqual(['ba', 'ab', 'ba'])

      const grants = grantsOf(relWire.tx.slice(txAt))
      expect(grants.length).toBeGreaterThanOrEqual(2) // 40 consumed, batched at 16
      for (const g of grants) {
        expect(g.sid).toBe(stream.sid)
        expect(g.seq).toBe(0)
        expect(g.payload).toBeUndefined()
        expect(g.window).toBeGreaterThan(0)
      }
    }, 30_000)

    it('a unary call on a reliable channel advertises a window too', async () => {
      const at = relWire.tx.length
      const res = await relConn.invoke(Echo.once, create(EchoRequestSchema, { message: 'hello', circularShift: 2 }))
      expect(res.message).toBe('llohe')
      const open = openOf(relWire.tx.slice(at))
      expect(open.window).toBe(32)
      // Reliable mode propagates no default deadline: T_call is an unreliable-
      // mode timer (§10.2).
      expect(open.timeoutMs).toBeUndefined()
    })
  })
})
