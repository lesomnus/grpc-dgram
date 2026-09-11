// Shared test plumbing: a JSON payload codec, an echo service, in-memory
// transports that round-trip every frame through the real wire codec (the
// same way the Go e2e pipes marshal real Envelops), and the fake Go runtime
// both src/wasm suites drive.

import { Conn, type ConnOptions } from '../conn'
import { bidiMethod, clientStreamingMethod, serverStreamingMethod, unaryMethod, type PayloadCodec } from '../desc'
import { Server, type ServerOptions } from '../server'
import type { FrameContext, FrameHandler } from '../seam'
import { decodeFrame, encodeFrame, type Frame } from '../wire'

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

// ---------------------------------------------------------------------------
// a single-delivery-loop transport
// ---------------------------------------------------------------------------

// Loop is one direction of a reliable adapter: frames are queued and handed
// to the peer by ONE pump, in order, awaiting each delivery (§4.2). That
// single loop is what makes head-of-line blocking possible at all — and
// therefore what the flow-control fix has to be measured against. makeNet's
// direct hand-off could never show it: there every call has its own caller —
// nor can it let a sender outrun the grants coming back to it, since each
// grant is delivered before the next send can start.
export class Loop {
  private readonly q: Frame[] = []
  private pumping = false
  private deliver: (f: Frame) => Promise<void> = async () => {}
  // Every frame offered to this direction, and how many the pump has handed
  // over so far (the quiescence signal `settle` waits on).
  readonly sent: Frame[] = []
  delivered = 0

  to(deliver: (f: Frame) => Promise<void>): void {
    this.deliver = deliver
  }

  get pending(): number {
    return this.q.length
  }

  push(f: Frame): void {
    const g = wireClone(f)
    this.sent.push(g)
    this.q.push(g)
    if (!this.pumping) void this.pump()
  }

  private async pump(): Promise<void> {
    this.pumping = true
    try {
      for (;;) {
        const f = this.q.shift()
        if (f === undefined) return
        try {
          await this.deliver(f)
        } catch {
          // Frame-level errors never tear the channel down (§4.2).
        }
        this.delivered++
      }
    } finally {
      this.pumping = false
    }
  }
}

export interface LoopNet {
  conn: Conn
  server: Server
  counts: ReturnType<typeof registerEcho>
  c2s: Loop
  s2c: Loop
  // settle runs microtask turns until both loops are drained and idle.
  settle: () => Promise<void>
}

export function makeLoopNet(opts: { connOpts?: ConnOptions; serverOpts?: ServerOptions; peer?: string } = {}): LoopNet {
  const peer = opts.peer ?? 'peer-1'
  const c2s = new Loop()
  const s2c = new Loop()
  const server = new Server({ handle: (f: Frame) => s2c.push(f) }, { reliable: true, ...opts.serverOpts })
  const counts = registerEcho(server)
  const conn = new Conn({ handle: (f: Frame) => c2s.push(f) }, { reliable: true, ...opts.connOpts })
  c2s.to((f) => server.handle(f, { peer }))
  s2c.to((f) => conn.handle(f, {}))

  const settle = async (): Promise<void> => {
    for (let i = 0; i < 500; i++) {
      const before = c2s.delivered + s2c.delivered
      await tick()
      if (c2s.pending === 0 && s2c.pending === 0 && c2s.delivered + s2c.delivered === before) return
    }
  }
  return { conn, server, counts, c2s, s2c, settle }
}

// ---------------------------------------------------------------------------
// wasm (src/wasm)
// ---------------------------------------------------------------------------

// The smallest thing WebAssembly.instantiate accepts: the 8-byte header of an
// empty module — no imports, no exports, nothing to run. The wasm entry only
// ever hands the instance to go.run(), and FakeGo ignores it.
export const emptyModule = new Uint8Array([0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00])

// FakeGo is wasm_exec's Go class reduced to what src/wasm touches, plus the
// controls a test needs: what the instance does when it starts (onRun, i.e.
// main), and how it ends. It lives here because both suites in src/wasm drive
// one — the in-realm path takes it as an option, the worker path finds it as
// globalThis.Go — and a second copy of it would be a second definition of what
// a wasm instance does.
//
// It publishes the way the real thing does, `Reflect.set(globalThis, name,
// fn)`, which is what `js.Global().Set(name, fn)` reaches JS as and the reason
// an accessor can catch it at all; its run() promise is controllable, so the
// two ways an instance dies — a resolution, which is what a Go panic does, and
// a trap — are both reachable.
export class FakeGo {
  readonly importObject: WebAssembly.Imports = {}
  entryPoint = 'drpcServe'
  runs = 0
  // wasm_exec's re-entry point, which every js.Func the instance registered
  // calls. The real one throws "Go program has already exited" once run() has
  // settled; this one records that it was reached.
  _resume = (): void => {
    this.resumes++
  }
  resumes = 0
  // The timers Go scheduled through wasm_exec's scheduleTimeoutEvent and has
  // not cleared: what an exit inside a time.Sleep leaves behind, and what
  // makeInert must cancel (see instance.ts).
  _scheduledTimeouts = new Map<number, ReturnType<typeof setTimeout>>()
  // onRun stands in for main(): a server publishes, a broken one exits.
  onRun: (go: FakeGo) => void = () => {}
  private readonly finished: Promise<void>
  private stop!: () => void
  private crash!: (e: unknown) => void

  constructor() {
    this.finished = new Promise((res, rej) => {
      this.stop = res
      this.crash = rej
    })
  }

  run(_instance: WebAssembly.Instance): Promise<void> {
    this.runs++
    this.onRun(this)
    return this.finished
  }

  // publish is js.Global().Set(name, fn) seen from JS.
  publish(fn: unknown): void {
    this.publishAt(this.entryPoint, fn)
  }

  // publishAt is a SECOND gateway publishing under a name of its own
  // (jsport.WithEntryPoint): the same js.Global().Set, another name, and only
  // the first one is what readiness was measured by.
  publishAt(name: string, fn: unknown): void {
    Reflect.set(globalThis, name, fn)
  }

  // exit RESOLVES run(), which is what wasm_exec does for a clean exit and
  // for a Go panic alike (it exits with code 2 and resolves).
  exit(): void {
    this.stop()
  }

  // trap rejects it, as an unrecoverable wasm error does.
  trap(e: unknown): void {
    this.crash(e)
  }
}
