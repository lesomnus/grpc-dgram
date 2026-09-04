// serveIn (src/wasm/worker.ts) against a fake worker scope: node cannot run a
// DOM module worker, and a file this package SHIPS that nothing can test is
// how the whole thing breaks — a page that gets a bad worker sees a timeout
// and nothing else, with no way to tell a broken module from a broken worker.
//
// The scope stands in for `self`: it collects what the worker posts back and
// dispatches what the page posts in, with `ports` on the event, which is where
// a transferred port really arrives. Everything on the far side of those ports
// is real — a Server behind a PortGateway, driven through a real Conn — so a
// call here is a genuine round trip, and the goodbye is the actual §4.5
// teardown rather than an assertion about a byte.
//
// The two rules under test are ordering rules, and both are silent when
// broken: a listener registered one await too late loses the start it was
// waiting for, and a port handed over before the instance exists loses the
// first call of every connection.

import { afterEach, describe, expect, it, vi } from 'vitest'
import { Conn } from '../conn'
import { Server } from '../server'
import { Code, type StatusError } from '../status'
import { echo, emptyModule, FakeGo, registerEcho, tick } from '../testing'
import { PortGateway, PortTransport } from '../transport/port'
import type { PageMessage, StartMessage, WorkerMessage } from './protocol'
import { serveIn, type WorkerScope } from './worker'

// FakeScope is a dedicated worker's global scope as serveIn sees it. `post` is
// the page posting in; `sent` is everything the worker posted back.
class FakeScope implements WorkerScope {
  readonly sent: WorkerMessage[] = []
  private readonly listeners: ((ev: unknown) => void)[] = []

  addEventListener(type: string, fn: (ev: unknown) => void): void {
    if (type === 'message') this.listeners.push(fn)
  }

  postMessage(message: unknown): void {
    this.sent.push(message as WorkerMessage)
  }

  // post dispatches one message event synchronously, which is what makes the
  // ordering tests mean anything: two posts in one task are two dispatches
  // with nothing able to happen in between.
  post(data: PageMessage, ports: MessagePort[] = []): void {
    for (const fn of [...this.listeners]) fn({ data, ports })
  }

  // waitFor drains both queues until the worker has posted what is asked for.
  // Macrotasks too: WebAssembly.instantiate settles off-thread, so a start
  // that only unwound promise chains would look like one that never answered.
  async waitFor(drpc: WorkerMessage['drpc']): Promise<WorkerMessage> {
    for (let i = 0; i < 50; i++) {
      const msg = this.sent.find((m) => m.drpc === drpc)
      if (msg !== undefined) return msg
      await new Promise((res) => setTimeout(res, 0))
      await tick()
    }
    throw new Error(`the worker never posted {drpc:'${drpc}'}; it posted ${JSON.stringify(this.sent)}`)
  }
}

const instances: EchoGo[] = []
const gateways: PortGateway[] = []
const opened: MessagePort[] = []
const conns: Conn[] = []

// The name the next instance's main publishes under — the Go side of the
// entry-point contract, which a test sets when it starts the worker under
// another name.
let publishAs = 'drpcServe'

// EchoGo is the Go half in one class: wasm_exec's Go, plus a main that
// publishes an entry point serving each handed port off a real Server. serveIn
// finds it as globalThis.Go and nowhere else — the start message carries no Go
// instance, because a Go belongs to the realm that built it.
class EchoGo extends FakeGo {
  readonly counts: ReturnType<typeof registerEcho>

  constructor() {
    super()
    this.entryPoint = publishAs
    const gw = new PortGateway()
    const server = new Server(gw)
    this.counts = registerEcho(server)
    gateways.push(gw)
    this.onRun = () =>
      this.publish((port: MessagePort) => {
        gw.bind(port)
        void gw.servePeer(server, port)
      })
    instances.push(this)
  }
}

// The second gateway's name, and how many turns of the event loop its main
// spends before publishing it. 0 publishes in the same main as readiness; a
// positive number is the program doing something that yields in between — a
// fetch, a database opening — which is what lets a page reach dial() first.
const adminEntryPoint = 'drpcAdminServe'
let adminDelay: number | undefined

// TwoGatewayGo is one instance serving TWO servers, the shape of issue #1: a
// second drpc.Server under a name of its own (jsport.WithEntryPoint), sharing
// the module, the runtime and the lifetime. Only the first is readiness.
class TwoGatewayGo extends EchoGo {
  readonly adminCounts: ReturnType<typeof registerEcho>

  constructor() {
    super()
    const gw = new PortGateway()
    const server = new Server(gw)
    this.adminCounts = registerEcho(server)
    gateways.push(gw)
    const publishAdmin = () =>
      this.publishAt(adminEntryPoint, (port: MessagePort) => {
        gw.bind(port)
        void gw.servePeer(server, port)
      })
    const readiness = this.onRun
    this.onRun = (go) => {
      readiness(go)
      if (adminDelay === undefined) return // a program with no second gateway
      if (adminDelay === 0) publishAdmin()
      else setTimeout(publishAdmin, adminDelay)
    }
  }
}

function start(scope: FakeScope, opts: Partial<StartMessage> = {}): void {
  scope.post({ drpc: 'start', app: emptyModule, wasmExec: '/wasm_exec.js', entryPoint: 'drpcServe', readyTimeoutMs: 1_000, ...opts })
}

// dial is the page's half of a `serve`: a fresh channel, one end posted in on
// the transfer list, a Conn over the other.
function dial(scope: FakeScope, entryPoint?: string): Conn {
  const ch = new MessageChannel()
  opened.push(ch.port1, ch.port2)
  const conn = new Conn(new PortTransport(ch.port1))
  conns.push(conn)
  scope.post({ drpc: 'serve', ...(entryPoint === undefined ? {} : { entryPoint }) }, [ch.port2])
  return conn
}

afterEach(() => {
  for (const conn of conns.splice(0)) conn.close()
  for (const gw of gateways.splice(0)) gw.close()
  for (const p of opened.splice(0)) p.close()
  instances.splice(0)
  publishAs = 'drpcServe'
  adminDelay = undefined
  // Unlike the start's, a dialled entry point is LEFT on globalThis: the
  // gateway behind it publishes once and the next dial has to find it by
  // reading. It is the instance's, not scaffolding, so the test owns cleaning
  // it up.
  Reflect.deleteProperty(globalThis, adminEntryPoint)
  vi.unstubAllGlobals()
  // The accessor the start installs is scaffolding; no path may leave it on
  // the worker's global scope.
  for (const name of ['drpcServe', 'todoServe']) expect(Object.hasOwn(globalThis, name)).toBe(false)
})

describe('the shipped worker', () => {
  it('honours a serve posted in the same task as the start', async () => {
    // Rule 2 from the inside. serveIn registers its listener synchronously, at
    // module evaluation, before any await — a module worker's top-level await
    // yields to the event loop, and both of these messages would be dispatched
    // into that yield and lost. Rule 1 covers the rest: the transferred port
    // queues the whole call until the instance is there to bind it.
    vi.stubGlobal('Go', EchoGo)
    const scope = new FakeScope()
    serveIn(scope)

    start(scope)
    const conn = dial(scope) // no instance yet, and no runtime either

    await scope.waitFor('ready')
    expect(await conn.invoke(echo.once, { text: 'early' })).toEqual({ text: 'echo:early' })
    expect(instances[0]?.counts.once).toBe(1)
  })

  it('answers ready once, and serves every port it is given after it', async () => {
    vi.stubGlobal('Go', EchoGo)
    const scope = new FakeScope()
    serveIn(scope)
    start(scope)
    await scope.waitFor('ready')

    const first = dial(scope)
    const second = dial(scope)
    expect(await first.invoke(echo.once, { text: 'a' })).toEqual({ text: 'echo:a' })
    expect(await second.invoke(echo.once, { text: 'b' })).toEqual({ text: 'echo:b' })
    // One port is one peer (§6.4): two connections to one instance, attributed
    // separately.
    expect(instances[0]?.counts.once).toBe(2)
    expect(scope.sent).toEqual([{ drpc: 'ready' }])
  })

  it('starts the instance with what the page resolved, and nothing of its own', async () => {
    vi.stubGlobal('Go', EchoGo)
    const scope = new FakeScope()
    serveIn(scope)
    publishAs = 'todoServe' // jsport.WithEntryPoint, on the Go side
    start(scope, { entryPoint: 'todoServe' })
    await scope.waitFor('ready')
    // Published under the name the page named, not the default: the two halves
    // ship separately, so every default is the page's to resolve.
    expect(await dial(scope).invoke(echo.once, { text: 'named' })).toEqual({ text: 'echo:named' })
  })

  it('answers a second start rather than leave the page waiting on it', async () => {
    vi.stubGlobal('Go', EchoGo)
    const scope = new FakeScope()
    serveIn(scope)
    start(scope)
    await scope.waitFor('ready')

    start(scope) // it has no entry point left to publish under, and one life
    await tick()
    expect(instances).toHaveLength(1)
    // Answered, not ignored: `ready` is the only thing that ever settles an
    // open(), so silence here is a page that hangs with no diagnosis. And the
    // answer names what the caller actually wanted — a second server in one
    // instance is a second entry point.
    expect(scope.sent).toHaveLength(2)
    const answer = scope.sent[1]!
    expect(answer.drpc).toBe('error')
    expect((answer as { message: string }).message).toMatch(/already runs an instance/)
    expect((answer as { message: string }).message).toMatch(/dial\(\{ entryPoint \}\)/)

    // The instance it already has is untouched by the refusal.
    expect(await dial(scope).invoke(echo.once, { text: 'still here' })).toEqual({ text: 'echo:still here' })
  })

  it('stays silent about a second start that overtakes the first answer', async () => {
    vi.stubGlobal('Go', EchoGo)
    const scope = new FakeScope()
    serveIn(scope)
    start(scope)
    start(scope) // in the same task: the first has not answered yet

    // Every message this worker posts reaches EVERY open() listening on it, so
    // an error posted now could reject the start that is actually running.
    // Before the first answer the duplicate is left to resolve off the
    // broadcast `ready` instead, which dials the one instance — what it wanted.
    await scope.waitFor('ready')
    await tick()
    expect(instances).toHaveLength(1)
    expect(scope.sent).toEqual([{ drpc: 'ready' }])
  })

  it('serves a port through the entry point the page named', async () => {
    // Issue #1: one instance, two servers, one worker. The page reaches the
    // second by name and the two are genuinely different registries — the
    // handler counters below are what tell them apart.
    adminDelay = 0
    vi.stubGlobal('Go', TwoGatewayGo)
    const scope = new FakeScope()
    serveIn(scope)
    start(scope)
    await scope.waitFor('ready')

    expect(await dial(scope).invoke(echo.once, { text: 'c' })).toEqual({ text: 'echo:c' })
    expect(await dial(scope, adminEntryPoint).invoke(echo.once, { text: 'a' })).toEqual({ text: 'echo:a' })

    const go = instances[0] as TwoGatewayGo
    expect(go.counts.once).toBe(1)
    expect(go.adminCounts.once).toBe(1)
  })

  it('waits for an entry point the instance has not published yet', async () => {
    // The reason a dial to an unpublished name is not simply refused. Only the
    // FIRST entry point is readiness, so a program that yields between its two
    // gateways is reachable here before the second has run — and whether that
    // happens is a matter of Go scheduling, which no page can be asked to
    // depend on. The port queues the whole call across the wait (rule 1).
    adminDelay = 30
    vi.stubGlobal('Go', TwoGatewayGo)
    const scope = new FakeScope()
    serveIn(scope)
    start(scope)
    await scope.waitFor('ready')

    expect(Object.hasOwn(globalThis, adminEntryPoint)).toBe(false) // not there yet
    const conn = dial(scope, adminEntryPoint)
    expect(await conn.invoke(echo.once, { text: 'late' })).toEqual({ text: 'echo:late' })
    expect((instances[0] as TwoGatewayGo).adminCounts.once).toBe(1)
  })

  it('says goodbye to a port whose entry point never arrives', async () => {
    // Bounded by the start's own readyTimeoutMs. The goodbye is what makes the
    // call FAIL rather than hang: with every protocol timer off (§10.6) it is
    // the only thing that ever ends it.
    vi.stubGlobal('Go', EchoGo) // one gateway; nothing will ever publish the other
    const scope = new FakeScope()
    serveIn(scope)
    start(scope, { readyTimeoutMs: 30 })
    await scope.waitFor('ready')

    const err = (await dial(scope, adminEntryPoint)
      .invoke(echo.once, { text: 'nobody' })
      .catch((e) => e)) as StatusError
    expect(err.code).toBe(Code.UNAVAILABLE)

    // One port, not the instance: the server it does run is untouched, and the
    // worker has nothing to report about a death that did not happen.
    expect(await dial(scope).invoke(echo.once, { text: 'fine' })).toEqual({ text: 'echo:fine' })
    expect(scope.sent).toEqual([{ drpc: 'ready' }])
  })

  it('says goodbye on every port when the instance dies, then reports it', async () => {
    // The whole reason this file is shipped rather than written by a page. Only
    // the worker can see go.run() settle, and with every protocol timer off
    // (§10.6) a peer that is not told waits forever. Both halves go out: the
    // goodbye each connection's own teardown hangs off (§4.5), and the message
    // the page fails its Conns with.
    vi.stubGlobal('Go', EchoGo)
    const scope = new FakeScope()
    serveIn(scope)
    start(scope)
    await scope.waitFor('ready')

    const first = dial(scope)
    const second = dial(scope)
    const stream = first.newStream(echo.live, {})
    await stream.send({ text: 'x' })
    expect(await stream.recv()).toEqual({ text: 'echo:x' })
    expect(await second.invoke(echo.once, { text: 'hi' })).toEqual({ text: 'echo:hi' })

    instances[0]!.exit()

    // The call that was in flight, and every other connection with it: they
    // share the process that stopped existing.
    const err = (await stream.recv().catch((e) => e)) as StatusError
    expect(err.code).toBe(Code.UNAVAILABLE)
    const other = (await second.invoke(echo.once, { text: 'anyone' }).catch((e) => e)) as StatusError
    expect(other.code).toBe(Code.UNAVAILABLE)
    expect(await scope.waitFor('exited')).toEqual({ drpc: 'exited', message: 'the wasm instance exited' })
  })

  it('carries a trap into the exit report', async () => {
    vi.stubGlobal('Go', EchoGo)
    const scope = new FakeScope()
    serveIn(scope)
    start(scope)
    await scope.waitFor('ready')

    instances[0]!.trap(new Error('unreachable executed'))
    // An Error survives structured clone as little more than its message, so
    // the sentence has to say what died as well as why.
    expect(await scope.waitFor('exited')).toEqual({ drpc: 'exited', message: 'the wasm instance failed: unreachable executed' })
  })

  it('reports a start that failed instead of hanging', async () => {
    // No globalThis.Go and no wasm_exec.js to be had. Without this message the
    // page would wait out readyTimeoutMs and report a module that never
    // published, when the truth is there was no runtime to publish with.
    vi.stubGlobal('fetch', () => Promise.resolve(new Response('not found', { status: 404, statusText: 'Not Found' })))
    const scope = new FakeScope()
    serveIn(scope)
    start(scope)

    const msg = (await scope.waitFor('error')) as { message: string }
    expect(msg.message).toMatch(/GET \/wasm_exec\.js failed \(404 Not Found\)/)
    // The one file this package cannot ship names its own way in.
    expect(msg.message).toMatch(/cp "\$\(go env GOROOT\)\/lib\/wasm\/wasm_exec\.js" \.\/public\//)
  })

  it('says goodbye to a port it can never serve', async () => {
    // A start that failed, or a serve that overtook one: holding the port
    // would leave the page's Conn waiting on a peer that will never exist, and
    // nothing else would ever end it (§10.6).
    vi.stubGlobal('fetch', () => Promise.resolve(new Response('nope', { status: 404, statusText: 'Not Found' })))
    const scope = new FakeScope()
    serveIn(scope)
    start(scope)
    await scope.waitFor('error')

    const conn = dial(scope)
    const err = (await conn.invoke(echo.once, { text: 'anyone' }).catch((e) => e)) as StatusError
    expect(err.code).toBe(Code.UNAVAILABLE)
  })

  it('says goodbye to a port it accepted before the start failed', async () => {
    // The port arrived while the start was still in the air, so it was taken
    // in rather than refused on sight, and the only thing that ever ends it is
    // bury() — which runs from the START's rejection handler. That handler is
    // registered first, when the start message is handled, and this port's is
    // registered second: reverse the two and the port is farewelled before it
    // is in the set, leaving the page's call on a peer that will never exist
    // with no timer to end it (§10.6).
    vi.stubGlobal('fetch', () => Promise.resolve(new Response('nope', { status: 404, statusText: 'Not Found' })))
    const scope = new FakeScope()
    serveIn(scope)
    start(scope)
    const conn = dial(scope) // same task as the start, long before it fails

    const err = (await conn.invoke(echo.once, { text: 'anyone' }).catch((e) => e)) as StatusError
    expect(err.code).toBe(Code.UNAVAILABLE)
    expect(await scope.waitFor('error')).toBeDefined()
  })

  it('says goodbye to a port that arrives as the instance is dying', async () => {
    // The narrow window: the run promise has settled but the teardown that
    // follows from it has not run yet, so this serve is taken for a live
    // instance and only serve() itself knows better. Whatever happens, the
    // port cannot be kept — nothing would ever end the call on the other side
    // of it — and the throw cannot escape, where an unhandled rejection would
    // take the worker down over one connection.
    vi.stubGlobal('Go', EchoGo)
    const scope = new FakeScope()
    serveIn(scope)
    start(scope)
    await scope.waitFor('ready')

    instances[0]!.exit()
    const conn = dial(scope) // same task as the exit
    const err = (await conn.invoke(echo.once, { text: 'too late' }).catch((e) => e)) as StatusError
    expect(err.code).toBe(Code.UNAVAILABLE)
  })

  it('drops traffic that is not ours', async () => {
    // The application may be using this worker for something else of its own.
    // A message this protocol does not know belongs to somebody else, and it
    // may not start, stop or crash anything (§4.2 takes the same view of a
    // frame it cannot read).
    vi.stubGlobal('Go', EchoGo)
    const scope = new FakeScope()
    serveIn(scope)
    scope.post({ kind: 'progress', done: 3 } as unknown as PageMessage)
    scope.post('a string' as unknown as PageMessage)
    await tick()
    expect(scope.sent).toEqual([])
    expect(instances).toHaveLength(0)

    start(scope)
    await scope.waitFor('ready')
    expect(await dial(scope).invoke(echo.once, { text: 'still here' })).toEqual({ text: 'echo:still here' })
  })

  it('does not bind the realm that merely imports it', () => {
    // The entry line at the bottom of worker.ts runs on import, and this suite
    // is one of the things that imports it: a guard that got this wrong would
    // put a message listener on the test runner's own global scope, and the
    // first `start` anybody posted there would run a wasm instance in it.
    expect((globalThis as { self?: unknown }).self).toBeUndefined()
  })
})
