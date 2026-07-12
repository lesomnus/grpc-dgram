// Shared test plumbing: a JSON payload codec, an echo service, and in-memory
// transports that round-trip every frame through the real wire codec (the
// same way the Go e2e pipes marshal real Envelops).

import { Conn, type ConnOptions } from '../src/conn'
import { bidiMethod, clientStreamingMethod, serverStreamingMethod, unaryMethod, type PayloadCodec } from '../src/desc'
import { Server, type ServerOptions } from '../src/server'
import type { FrameContext, FrameHandler } from '../src/seam'
import { decodeFrame, encodeFrame, type Frame } from '../src/wire'

export interface TestReq {
  text: string
  n?: number
}

export interface TestRes {
  text: string
}

export function jsonCodec<T>(): PayloadCodec<T> {
  return {
    marshal: (v) => new TextEncoder().encode(JSON.stringify(v ?? null)),
    unmarshal: (b) => JSON.parse(new TextDecoder().decode(b) || 'null') as T,
  }
}

export const echo = {
  once: unaryMethod<TestReq, TestRes>('/test.Echo/Once', { request: jsonCodec(), response: jsonCodec() }),
  many: serverStreamingMethod<TestReq, TestRes>('/test.Echo/Many', { request: jsonCodec(), response: jsonCodec() }),
  count: clientStreamingMethod<TestReq, TestRes>('/test.Echo/Count', { request: jsonCodec(), response: jsonCodec() }),
  live: bidiMethod<TestReq, TestRes>('/test.Echo/Live', { request: jsonCodec(), response: jsonCodec() }),
}

// registerEcho registers the echo service; the returned counters track
// handler executions for at-most-once assertions.
export function registerEcho(server: Server) {
  const counts = { once: 0, many: 0, count: 0, live: 0 }
  server.register(echo.once, (req) => {
    counts.once++
    return { text: `echo:${req.text}` }
  })
  server.register(echo.many, async (req, stream) => {
    counts.many++
    for (let i = 0; i < (req.n ?? 0); i++) {
      await stream.send({ text: `${req.text}#${i}` })
    }
  })
  server.register(echo.count, async (stream) => {
    counts.count++
    let n = 0
    for await (const _ of stream) n++
    return { text: String(n) }
  })
  server.register(echo.live, async (stream) => {
    counts.live++
    for await (const msg of stream) {
      await stream.send({ text: `echo:${msg.text}` })
    }
  })
  return counts
}

// wireClone round-trips a frame through the real encoding, exercising the
// wire codec on every delivered frame and decoupling sender/receiver state.
export function wireClone(f: Frame): Frame {
  return decodeFrame(encodeFrame(f))
}

export type Filter = (f: Frame) => boolean // true = deliver

export interface TestNet {
  conn: Conn
  server: Server
  counts: ReturnType<typeof registerEcho>
  peer: string
  // Loss injection; reassign per test phase. Return false to drop.
  c2s: { filter: Filter }
  s2c: { filter: Filter }
  // Every frame offered to each direction, pre-filter.
  sentC2S: Frame[]
  sentS2C: Frame[]
}

// makeNet wires a Conn and a Server through an in-memory channel with
// synchronous, in-order delivery (awaited handle per frame, like an adapter
// delivering an envelop) and per-direction loss filters.
export function makeNet(opts: { reliable: boolean; connOpts?: ConnOptions; serverOpts?: ServerOptions; register?: (server: Server) => void; peer?: string }): TestNet {
  const peer = opts.peer ?? 'peer-1'
  const net = {
    c2s: { filter: (() => true) as Filter },
    s2c: { filter: (() => true) as Filter },
    sentC2S: [] as Frame[],
    sentS2C: [] as Frame[],
  }

  let server: Server
  let conn: Conn

  const clientTx: FrameHandler = {
    async handle(f: Frame): Promise<void> {
      const g = wireClone(f)
      net.sentC2S.push(g)
      if (!net.c2s.filter(g)) return
      await server.handle(g, { peer })
    },
  }
  const serverTx: FrameHandler = {
    async handle(f: Frame, _ctx?: FrameContext): Promise<void> {
      const g = wireClone(f)
      net.sentS2C.push(g)
      if (!net.s2c.filter(g)) return
      await conn.handle(g, {})
    },
  }

  server = new Server(serverTx, { reliable: opts.reliable, ...opts.serverOpts })
  const counts = registerEcho(server)
  opts.register?.(server)
  conn = new Conn(clientTx, { reliable: opts.reliable, ...opts.connOpts })

  return { conn, server, counts, peer, ...net }
}

export const flagsOf = (f: Frame) => f.flags

export async function tick(): Promise<void> {
  // Settle promise chains without advancing timers.
  for (let i = 0; i < 20; i++) await Promise.resolve()
}
