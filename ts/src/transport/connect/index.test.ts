// The Connect-ES transport (src/transport/connect/index.ts): a standard Connect client
// (createClient) drives all four RPC types over a drpc Conn talking to a drpc
// Server, with header/trailer metadata and ConnectError mapping.

import { create } from '@bufbuild/protobuf'
import { Code as ConnectCode, ConnectError, createClient } from '@connectrpc/connect'
import { describe, expect, it } from 'vitest'
import { Conn } from '../../conn'
import { createDrpcTransport } from './index'
import { fromService } from '../protobuf-es'
import { Server } from '../../server'
import { Code, statusError } from '../../status'
import { EchoBatchResponseSchema, EchoRequestSchema, EchoResponseSchema, EchoService } from '../../testing/gen/echo/echo_pb.js'

const Echo = fromService(EchoService)

async function* iterate<T>(items: T[]): AsyncIterable<T> {
  for (const i of items) yield i
}

// A reliable in-memory pipe wired to a Connect client backed by a drpc Conn.
function connectClient() {
  let server!: Server
  let conn!: Conn
  conn = new Conn({ handle: async (f) => void (await server.handle(f, { peer: 'p' })) }, { reliable: true })
  server = new Server({ handle: async (f) => void (await conn.handle(f, {})) }, { reliable: true })

  server.register(Echo.once, (req, ctx) => {
    if (req.message === 'boom') throw statusError(Code.INVALID_ARGUMENT, 'bad request')
    if (ctx.metadata?.authorization !== undefined) {
      ctx.setHeader({ 'x-timing': ['header'] })
      ctx.setTrailer({ 'x-timing': ['trailer'] })
    }
    return create(EchoResponseSchema, { message: `echo:${req.message}`, sequence: 0 })
  })
  server.register(Echo.many, async (req, stream) => {
    for (let i = 0; i < req.repeat; i++) {
      await stream.send(create(EchoResponseSchema, { message: `${req.message}#${i}`, sequence: i }))
    }
  })
  server.register(Echo.buff, async (stream) => {
    const items = []
    for await (const req of stream) items.push(create(EchoResponseSchema, { message: req.message }))
    return create(EchoBatchResponseSchema, { items })
  })
  server.register(Echo.live, async (stream) => {
    for await (const req of stream) await stream.send(create(EchoResponseSchema, { message: `echo:${req.message}` }))
  })

  return { conn, client: createClient(EchoService, createDrpcTransport(conn)) }
}

describe('Connect client over drpc', () => {
  it('unary', async () => {
    const { client } = connectClient()
    const res = await client.once({ message: 'hi' })
    expect(res.message).toBe('echo:hi')
  })

  it('server streaming yields the exact sequence', async () => {
    const { client } = connectClient()
    const got: string[] = []
    for await (const res of client.many({ message: 'm', repeat: 3 })) got.push(res.message)
    expect(got).toEqual(['m#0', 'm#1', 'm#2'])
  })

  it('client streaming returns the single response', async () => {
    const { client } = connectClient()
    const res = await client.buff(iterate([{ message: 'a' }, { message: 'b' }, { message: 'c' }]))
    expect(res.items.map((i) => i.message)).toEqual(['a', 'b', 'c'])
  })

  it('bidi streaming echoes interleaved', async () => {
    const { client } = connectClient()
    const got: string[] = []
    for await (const res of client.live(iterate([{ message: 'x' }, { message: 'y' }]))) got.push(res.message)
    expect(got).toEqual(['echo:x', 'echo:y'])
  })

  it('maps a drpc StatusError to a ConnectError with the same code', async () => {
    const { client } = connectClient()
    const err = await client.once({ message: 'boom' }).catch((e) => e)
    expect(err).toBeInstanceOf(ConnectError)
    expect(err.code).toBe(ConnectCode.InvalidArgument)
    expect(err.rawMessage).toBe('bad request')
  })

  it('carries request metadata to the server and response header/trailer back', async () => {
    const { client } = connectClient()
    let header: Headers | undefined
    let trailer: Headers | undefined
    const res = await client.once(
      { message: 'hi' },
      {
        headers: { authorization: 'token' },
        onHeader: (h) => (header = h),
        onTrailer: (t) => (trailer = t),
      },
    )
    expect(res.message).toBe('echo:hi')
    expect(header?.get('x-timing')).toBe('header')
    expect(trailer?.get('x-timing')).toBe('trailer')
  })

  it('server metadata that is not a legal HTTP header does not crash the call', async () => {
    // drpc metadata is arbitrary strings (§11): a newline, an emoji, or an
    // invalid header name would make Headers.append throw a TypeError. The
    // transport must still deliver the message and keep the representable
    // metadata (audit regression).
    let server!: Server
    let conn!: Conn
    conn = new Conn({ handle: async (f) => void (await server.handle(f, { peer: 'p' })) }, { reliable: true })
    server = new Server({ handle: async (f) => void (await conn.handle(f, {})) }, { reliable: true })
    server.register(Echo.once, (_req, ctx) => {
      ctx.setHeader({ 'x-good': ['ok'], 'x-newline': ['a\nb'], 'x-emoji': ['\u{1F600}'] })
      ctx.setTrailer({ 'x-trailer': ['fine'], 'bad name': ['v'] })
      return create(EchoResponseSchema, { message: 'echo:hi' })
    })
    const client = createClient(EchoService, createDrpcTransport(conn))
    let header: Headers | undefined
    let trailer: Headers | undefined
    const res = await client.once({ message: 'hi' }, { onHeader: (h) => (header = h), onTrailer: (t) => (trailer = t) })
    expect(res.message).toBe('echo:hi') // the call succeeded, message delivered
    expect(header?.get('x-good')).toBe('ok') // representable metadata survives
    expect(header?.has('x-newline')).toBe(false) // unrepresentable dropped, not thrown
    expect(header?.has('x-emoji')).toBe(false)
    expect(trailer?.get('x-trailer')).toBe('fine')
  })

  it('a method the server never registered surfaces as UNIMPLEMENTED', async () => {
    // A bare server that registers only `many`, so `once` is unknown (§13).
    let server!: Server
    let conn!: Conn
    conn = new Conn({ handle: async (f) => void (await server.handle(f, { peer: 'p' })) }, { reliable: true })
    server = new Server({ handle: async (f) => void (await conn.handle(f, {})) }, { reliable: true })
    server.register(Echo.many, async () => {})
    const client = createClient(EchoService, createDrpcTransport(conn))
    const err = await client.once({ message: 'x' }).catch((e) => e)
    expect(err).toBeInstanceOf(ConnectError)
    expect(err.code).toBe(ConnectCode.Unimplemented)
  })
})
