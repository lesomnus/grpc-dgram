// Cross-language conformance: a TypeScript client drives a REAL Go
// drpc.Server over UDP (unreliable mode), proving the two implementations
// agree on the wire format AND the behavior — method dispatch, the
// OPEN/CLOSE/seq/epoch state machine, the proto codec, and all four RPC
// shapes. The TS client uses descriptors derived from the generated
// EchoService (fromService); the Go server is conformance/udpserver, serving
// internal/echo over transport/udp. Skipped when `go` is unavailable.
//
// This is the runtime counterpart to the static §5 golden-byte vectors: those
// pin the encoding, this pins the conversation.

import { create } from '@bufbuild/protobuf'
import { execFileSync, spawn, type ChildProcessWithoutNullStreams } from 'node:child_process'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'
import { afterAll, beforeAll, describe, expect, it } from 'vitest'
import { Conn } from '../src/conn'
import { dialUdp } from '../src/node-udp'
import { fromService } from '../src/protobufes'
import { EchoRequestSchema, EchoResponseSchema, EchoService } from './gen/echo/echo_pb.js'

const repoRoot = resolve(process.cwd(), '..')
const Echo = fromService(EchoService)

function hasGo(): boolean {
  try {
    execFileSync('go', ['version'], { stdio: 'ignore' })
    return true
  } catch {
    return false
  }
}

// startGoServer builds conformance/udpserver once, spawns it, and resolves
// with the UDP port it announces on stdout.
async function startGoServer(bin: string): Promise<{ port: number; proc: ChildProcessWithoutNullStreams }> {
  const proc = spawn(bin, [], { cwd: repoRoot })
  const port = await new Promise<number>((res, rej) => {
    let buf = ''
    const to = setTimeout(() => rej(new Error(`go server did not announce a port; stderr:\n${errBuf}`)), 8000)
    let errBuf = ''
    proc.stderr.on('data', (d) => (errBuf += String(d)))
    proc.stdout.on('data', (d) => {
      buf += String(d)
      const m = buf.match(/PORT (\d+)/)
      if (m) {
        clearTimeout(to)
        res(Number(m[1]))
      }
    })
    proc.on('exit', (code) => rej(new Error(`go server exited early (${code}); stderr:\n${errBuf}`)))
  })
  return { port, proc }
}

describe.skipIf(!hasGo())('cross-language conformance (TS client ↔ Go server over UDP)', () => {
  let bin: string
  let proc: ChildProcessWithoutNullStreams
  let conn: Conn

  beforeAll(async () => {
    bin = join(mkdtempSync(join(tmpdir(), 'drpc-conf-')), 'udpserver')
    execFileSync('go', ['build', '-o', bin, './conformance/udpserver'], { cwd: repoRoot, stdio: 'pipe' })
    const started = await startGoServer(bin)
    proc = started.proc
    const transport = await dialUdp(started.port)
    conn = new Conn(transport) // discovers unreliable mode from the transport
  }, 60_000)

  afterAll(() => {
    conn?.close()
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
})
