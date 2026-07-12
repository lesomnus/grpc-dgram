// Real protoc-gen-es output, not a hand-built descriptor: this test imports
// the EchoService generated (by `buf generate` with @bufbuild/protoc-gen-es)
// from the SAME echo.proto the Go implementation uses, feeds it straight to
// fromService, and round-trips genuine generated messages over the transport.
// If this passes, "usable with protobuf-es generated code" is literally true.
//
// The generated sources live in test/gen/ (checked in; regenerate with
// `buf generate ../proto --template <protoc-gen-es template>`).

import { create } from '@bufbuild/protobuf'
import { describe, expect, it } from 'vitest'
import { Conn } from '../src/conn'
import { fromService } from '../src/transport/protobuf-es'
import { Server } from '../src/server'
import { isOpen, type Frame } from '../src/wire'
import { EchoBatchResponseSchema, EchoRequestSchema, EchoResponseSchema, EchoService } from './gen/echo/echo_pb.js'

// Derive every method from the generated service — nothing hand-written.
const Echo = fromService(EchoService)

function pipe() {
  let server!: Server
  let conn!: Conn
  const sentC2S: Frame[] = []
  conn = new Conn({ handle: async (f) => void (sentC2S.push(f), await server.handle(f, { peer: 'p' })) }, { reliable: true })
  server = new Server({ handle: async (f) => void (await conn.handle(f, {})) }, { reliable: true })
  return { conn, server, sentC2S }
}

describe('generated EchoService → fromService', () => {
  it('derives paths and streaming kinds matching the .proto (and the Go server)', () => {
    expect(Echo.once).toMatchObject({ path: '/echo.EchoService/Once', clientStreams: false, serverStreams: false })
    expect(Echo.many).toMatchObject({ path: '/echo.EchoService/Many', clientStreams: false, serverStreams: true })
    expect(Echo.buff).toMatchObject({ path: '/echo.EchoService/Buff', clientStreams: true, serverStreams: false })
    expect(Echo.live).toMatchObject({ path: '/echo.EchoService/Live', clientStreams: true, serverStreams: true })
    expect(Echo.noop).toMatchObject({ path: '/echo.EchoService/Noop', clientStreams: false, serverStreams: false })
  })

  it('round-trips a unary call with real generated messages and genuine proto bytes', async () => {
    const net = pipe()
    // The handler is typed: req is EchoRequest, must return EchoResponse.
    net.server.register(Echo.once, (req) => create(EchoResponseSchema, { message: `echo:${req.message}`, sequence: 1 }))

    const res = await net.conn.invoke(Echo.once, create(EchoRequestSchema, { message: 'hi' }))
    expect(res.message).toBe('echo:hi')
    expect(res.sequence).toBe(1)

    // The OPEN payload is the real proto encoding of EchoRequest{message:"hi"}:
    // field 2 (message), string, "hi" → 12 02 68 69. Not JSON — the wire codec
    // a Go server would also read (§12).
    const open = net.sentC2S.find((f) => isOpen(f))!
    expect([...(open.payload ?? [])]).toEqual([0x12, 0x02, 0x68, 0x69])
  })

  it('round-trips a server-streaming call', async () => {
    const net = pipe()
    net.server.register(Echo.many, async (req, stream) => {
      for (let i = 0; i < req.repeat; i++) {
        await stream.send(create(EchoResponseSchema, { message: req.message, sequence: i }))
      }
    })
    const stream = net.conn.newStream(Echo.many, {})
    await stream.send(create(EchoRequestSchema, { message: 'm', repeat: 3 }))
    const seqs: number[] = []
    for await (const res of stream) seqs.push(res.sequence)
    expect(seqs).toEqual([0, 1, 2])
  })

  it('round-trips a client-streaming call (Buff → EchoBatchResponse)', async () => {
    const net = pipe()
    // Buff's proto response is EchoBatchResponse; the handler return type is
    // derived accordingly (a wrong return type would not compile).
    net.server.register(Echo.buff, async (stream) => {
      const items = []
      for await (const req of stream) items.push(create(EchoResponseSchema, { message: req.message }))
      return create(EchoBatchResponseSchema, { items })
    })
    const stream = net.conn.newStream(Echo.buff, {})
    await stream.send(create(EchoRequestSchema, { message: 'x' }))
    await stream.send(create(EchoRequestSchema, { message: 'y' }))
    stream.closeSend()
    const res = await stream.recv()
    expect(res?.items.map((i) => i.message)).toEqual(['x', 'y'])
    expect(await stream.recv()).toBeUndefined()
  })

  it('round-trips a bidi call (Live)', async () => {
    const net = pipe()
    net.server.register(Echo.live, async (stream) => {
      for await (const req of stream) await stream.send(create(EchoResponseSchema, { message: `echo:${req.message}` }))
    })
    const stream = net.conn.newStream(Echo.live, {})
    await stream.send(create(EchoRequestSchema, { message: 'a' }))
    expect((await stream.recv())?.message).toBe('echo:a')
    await stream.send(create(EchoRequestSchema, { message: 'b' }))
    expect((await stream.recv())?.message).toBe('echo:b')
    stream.closeSend()
    expect(await stream.recv()).toBeUndefined()
  })
})
