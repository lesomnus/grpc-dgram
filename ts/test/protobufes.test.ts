// The protobuf-es binding (src/transport/protobuf-es/index.ts): a real protobuf-es v2 message
// round-trips over the transport through a descriptor DERIVED from a
// protobuf-es method descriptor — no hand-written path, streaming kind, or
// codec. Uses the well-known StringValue wrapper as a stand-in for a generated
// message (both are real DescMessages at runtime, so toBinary/fromBinary and
// the wire bytes are genuine proto, not JSON).

import { create, type DescMethod, type DescService } from '@bufbuild/protobuf'
import { StringValueSchema, type StringValue } from '@bufbuild/protobuf/wkt'
import { describe, expect, it } from 'vitest'
import { Conn } from '../src/conn'
import { fromMethod } from '../src/transport/protobuf-es'
import { Server } from '../src/server'
import { isOpen, type Frame } from '../src/wire'

// A fake method descriptor with the shape protoc-gen-es emits (service
// typeName + proto method name + kind + input/output message schemas). The
// kind literal is preserved so the derived descriptor discriminates unary vs
// streaming, exactly as a generated `service.method.<name>` entry would.
function fakeMethod<K extends DescMethod['methodKind']>(kind: K, name: string) {
  return {
    kind: 'rpc',
    name,
    localName: name[0]!.toLowerCase() + name.slice(1),
    parent: { typeName: 'test.StrEcho' } as DescService,
    methodKind: kind,
    input: StringValueSchema,
    output: StringValueSchema,
  } as unknown as DescMethod & { methodKind: K; input: typeof StringValueSchema; output: typeof StringValueSchema }
}

// A synchronous reliable in-memory pipe, decoupled through the real wire codec.
function pipe() {
  let server!: Server
  let conn!: Conn
  const sentC2S: Frame[] = []
  conn = new Conn(
    { handle: async (f) => void (sentC2S.push(f), await server.handle(f, { peer: 'p' })) },
    { reliable: true },
  )
  server = new Server({ handle: async (f) => void (await conn.handle(f, {})) }, { reliable: true })
  return { conn, server, sentC2S }
}

describe('fromMethod derivation (§13)', () => {
  it('derives the wire path and streaming kind from the descriptor', () => {
    expect(fromMethod(fakeMethod('unary', 'Once'))).toMatchObject({ path: '/test.StrEcho/Once', clientStreams: false, serverStreams: false })
    expect(fromMethod(fakeMethod('server_streaming', 'Many'))).toMatchObject({ clientStreams: false, serverStreams: true })
    expect(fromMethod(fakeMethod('client_streaming', 'Count'))).toMatchObject({ clientStreams: true, serverStreams: false })
    expect(fromMethod(fakeMethod('bidi_streaming', 'Live'))).toMatchObject({ clientStreams: true, serverStreams: true })
  })

  it('uses the proto method name for the path, not the localName', () => {
    // A generated service exposes `service.method.once` (localName) but the
    // wire path must carry `Once` (proto name), or a TS client and Go server
    // would address different methods.
    const desc = fromMethod(fakeMethod('unary', 'Once'))
    expect(desc.path).toBe('/test.StrEcho/Once')
    expect(desc.path).not.toContain('/once')
  })
})

describe('protobuf-es messages over the transport', () => {
  it('round-trips a unary call with genuine proto wire bytes', async () => {
    const net = pipe()
    const Once = fromMethod(fakeMethod('unary', 'Once'))
    net.server.register(Once, (req: StringValue) => create(StringValueSchema, { value: `echo:${req.value}` }))

    const res = await net.conn.invoke(Once, create(StringValueSchema, { value: 'hi' }))
    expect(res.value).toBe('echo:hi')

    // The OPEN payload is real protobuf (StringValue{value:"hi"} = 0a 02 68 69),
    // not JSON — the cross-implementation wire codec (§12).
    const open = net.sentC2S.find((f) => isOpen(f))!
    expect([...(open.payload ?? [])]).toEqual([0x0a, 0x02, 0x68, 0x69])
  })

  it('round-trips a bidi stream of protobuf-es messages', async () => {
    const net = pipe()
    const Live = fromMethod(fakeMethod('bidi_streaming', 'Live'))
    net.server.register(Live, async (stream) => {
      for await (const msg of stream as AsyncIterable<StringValue>) {
        await stream.send(create(StringValueSchema, { value: `echo:${msg.value}` }))
      }
    })
    const stream = net.conn.newStream(Live, {})
    await stream.send(create(StringValueSchema, { value: 'a' }))
    expect((await stream.recv())?.value).toBe('echo:a')
    await stream.send(create(StringValueSchema, { value: 'b' }))
    expect((await stream.recv())?.value).toBe('echo:b')
    stream.closeSend()
    expect(await stream.recv()).toBeUndefined()
  })
})
