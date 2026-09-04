// open() (src/wasm) in both of its modes, against a FAKE Go class and a fake
// worker: no toolchain, and every failure reachable in a millisecond.
//
// What open() actually does is the glue around an instance — instantiate,
// catch the publish, hand over ports, wire the death — and none of that is Go
// code. The far end of every connection is a real Server behind a real
// PortGateway, so a call is a genuine round trip through the wire and the §4.5
// teardown rather than a mock answering. The end-to-end proof against a Go
// server compiled to js/wasm is test/wasm.test.ts, which drives this same
// entry with `{ worker: false }`.
//
// The worker path is exercised with a fake worker that speaks ./protocol,
// because node cannot run a DOM module worker; the worker's own half is
// src/wasm/worker.test.ts, and the DOM Worker constructor itself is the one
// line in this file that no test in this package executes.

import { afterEach, describe, expect, it, vi } from 'vitest'
import { Server } from '../server'
import { Code, type StatusError } from '../status'
import { echo, emptyModule, FakeGo, registerEcho, tick } from '../testing'
import { PortGateway } from '../transport/port'
import { DefaultEntryPoint, open, type OpenOptions, type WasmSock, type WasmWorker } from './index'
import { isPageMessage, type ServeMessage, type StartMessage } from './protocol'

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

const socks: WasmSock[] = []
const gateways: PortGateway[] = []

// serving turns a FakeGo into a working server: its main publishes an entry
// point that serves each handed port off a real Server.
function serving(go: FakeGo) {
  const gw = new PortGateway()
  const server = new Server(gw)
  const counts = registerEcho(server)
  gateways.push(gw)
  go.onRun = () =>
    go.publish((port: MessagePort) => {
      gw.bind(port)
      void gw.servePeer(server, port)
    })
  return { gw, server, counts }
}

// The name a second gateway in the same instance publishes under
// (jsport.WithEntryPoint), and `servingAlso` is that gateway. Only the FIRST
// entry point is what readiness means, so `after` is the turns of the event
// loop the program spends before this one appears — a program that does
// anything asynchronous between its two gateways.
const secondEntryPoint = 'drpcAdminServe'

function servingAlso(go: FakeGo, opts: { after?: number } = {}) {
  const gw = new PortGateway()
  const server = new Server(gw)
  const counts = registerEcho(server)
  gateways.push(gw)
  const publish = () =>
    go.publishAt(secondEntryPoint, (port: MessagePort) => {
      gw.bind(port)
      void gw.servePeer(server, port)
    })
  const readiness = go.onRun
  go.onRun = (g) => {
    readiness(g)
    if (opts.after === undefined) publish()
    else setTimeout(publish, opts.after)
  }
  return { gw, server, counts }
}

// here opens an instance in this realm and records the sock, so afterEach
// releases both ends of every channel it dialled.
async function here(go: FakeGo, opts: OpenOptions = {}): Promise<WasmSock> {
  const sock = await open(emptyModule, { worker: false, go, ...opts })
  socks.push(sock)
  return sock
}

// FakeWorker is the shipped worker as the page sees it: it answers `start`
// with `ready`, takes the port off each `serve`, and serves it off a real
// Server. Anything it does not recognize is DROPPED into `stray` — which is
// what a real worker does with traffic it has no listener for, and what makes
// the ordering tests below able to fail: a dial that posted frames at the
// worker instead of at a transferred port would land there and never be
// answered.
class FakeWorker implements WasmWorker {
  readonly starts: StartMessage[] = []
  // Every `serve` it was posted, message only: what the page asked for, as
  // opposed to the ports it was handed (`transferred`).
  readonly serves: ServeMessage[] = []
  readonly stray: unknown[] = []
  readonly transferred: unknown[] = []
  readonly counts: ReturnType<typeof registerEcho>
  terminated = 0
  // Off, a port is held until bind() — an instance that has not finished
  // binding the port it was handed, which is the normal case for a call opened
  // on the tick open() resolved.
  autoBind = true
  // Off, `ready` is withheld until ready() — a start that has not finished.
  autoReady = true

  private readonly gw = new PortGateway()
  private readonly server: Server
  private readonly pending: MessagePort[] = []
  private readonly listeners = new Map<string, ((ev: unknown) => void)[]>()

  constructor() {
    this.server = new Server(this.gw)
    this.counts = registerEcho(this.server)
    gateways.push(this.gw)
  }

  postMessage(message: unknown, transfer?: unknown[]): void {
    if (!isPageMessage(message)) {
      this.stray.push(message)
      return
    }
    if (message.drpc === 'start') {
      this.starts.push(message)
      if (this.autoReady) this.ready()
      return
    }
    const port = transfer?.[0]
    if (!(port instanceof MessagePort)) {
      this.stray.push(message)
      return
    }
    this.serves.push(message)
    this.transferred.push(port)
    this.pending.push(port)
    if (this.autoBind) this.bind()
  }

  // bind is the Go side finally taking the ports it was given.
  bind(): void {
    for (const port of this.pending.splice(0)) {
      this.gw.bind(port)
      void this.gw.servePeer(this.server, port)
    }
  }

  ready(): void {
    this.emit({ drpc: 'ready' })
  }

  // died is the worker's §4.5 report; the real one says goodbye on every port
  // first, which is worker.test.ts's business, not this file's.
  died(message: string): void {
    this.emit({ drpc: 'exited', message })
  }

  failed(message: string): void {
    this.emit({ drpc: 'error', message })
  }

  // emit delivers one message event to the page, on a later turn: a real
  // worker is a thread away, and nothing here may look synchronous that is
  // not.
  emit(data: unknown): void {
    queueMicrotask(() => {
      for (const fn of this.listeners.get('message') ?? []) fn({ data })
    })
  }

  raise(type: string, ev: unknown): void {
    for (const fn of this.listeners.get(type) ?? []) fn(ev)
  }

  get listenerCount(): number {
    let n = 0
    for (const l of this.listeners.values()) n += l.length
    return n
  }

  addEventListener(type: string, fn: (ev: unknown) => void): void {
    const l = this.listeners.get(type) ?? []
    l.push(fn)
    this.listeners.set(type, l)
  }

  removeEventListener(type: string, fn: (ev: unknown) => void): void {
    const l = (this.listeners.get(type) ?? []).filter((x) => x !== fn)
    if (l.length === 0) this.listeners.delete(type)
    else this.listeners.set(type, l)
  }

  terminate(): void {
    this.terminated++
  }
}

async function withWorker(w: FakeWorker, opts: OpenOptions = {}): Promise<WasmSock> {
  const sock = await open(emptyModule, { worker: w, ...opts })
  socks.push(sock)
  return sock
}

// The workers open() spawned for ITSELF, with the arguments it spawned them
// with. A worker open() made is the one case a worker handed to it cannot
// reach — it is the only kind open() may terminate — so globalThis.Worker is
// stubbed with this and the constructor lookup in spawn() is the real one.
const spawned: { url: unknown; opts: unknown; worker: FakeWorker }[] = []
// What the next spawned worker does with the start it is given; off, it has
// not answered yet and the test decides how it ends.
let spawnReady = true

class SpawnedWorker extends FakeWorker {
  constructor(url: unknown, opts: unknown) {
    super()
    this.autoReady = spawnReady
    spawned.push({ url, opts, worker: this })
  }
}

afterEach(() => {
  // Both ends of every channel: the sock owns the ones it dialled, the gateway
  // the ones the instance was given. A live MessagePort keeps node's event
  // loop alive and the run would never exit.
  for (const sock of socks.splice(0)) sock.close()
  for (const gw of gateways.splice(0)) gw.close()
  spawned.splice(0)
  spawnReady = true
  // A dialled entry point is LEFT on globalThis, unlike the start's: the
  // gateway behind it publishes once and the next dial finds it by reading. It
  // belongs to the instance rather than to open(), so the test clears it.
  Reflect.deleteProperty(globalThis, secondEntryPoint)
  vi.unstubAllGlobals()
  // The accessor is open()'s own scaffolding and no path may leave it behind:
  // a leftover one would swallow the next instance's publish into a promise
  // nobody is awaiting, and the start after it would time out with the module
  // working perfectly.
  for (const name of [DefaultEntryPoint, 'todoServe']) {
    expect(Object.hasOwn(globalThis, name)).toBe(false)
  }
})

// ---------------------------------------------------------------------------
// the instance in this realm
// ---------------------------------------------------------------------------

describe('the two-line path', () => {
  it('dials a connection already talking to the instance', async () => {
    const go = new FakeGo()
    const net = serving(go)
    const sock = await here(go)
    const conn = sock.dial()

    expect(go.runs).toBe(1)
    expect(sock.worker).toBeUndefined()
    expect(conn.reliable).toBe(true) // discovered from the adapter (§4.3)
    expect(await conn.invoke(echo.once, { text: 'hi' })).toEqual({ text: 'echo:hi' })
    expect(net.counts.once).toBe(1)
    conn.close()
  })

  it('fetches a URL and instantiates what it gets back', async () => {
    const urls: string[] = []
    vi.stubGlobal('fetch', (url: string) => {
      urls.push(url)
      return Promise.resolve(new Response(emptyModule, { headers: { 'content-type': 'application/wasm' } }))
    })
    const go = new FakeGo()
    serving(go)
    const sock = await open('/app.wasm', { worker: false, go })
    socks.push(sock)
    expect(urls).toEqual(['/app.wasm'])
    expect(await sock.dial().invoke(echo.once, { text: 'url' })).toEqual({ text: 'echo:url' })
  })

  it('instantiates a module the server mislabelled', async () => {
    // A static server that never heard of wasm answers application/octet-
    // stream, which instantiateStreaming refuses outright. Buffering is what
    // makes such a server work anyway.
    vi.stubGlobal('fetch', () => Promise.resolve(new Response(emptyModule, { headers: { 'content-type': 'application/octet-stream' } })))
    const go = new FakeGo()
    serving(go)
    const sock = await open('/app.wasm', { worker: false, go })
    socks.push(sock)
    expect(await sock.dial().invoke(echo.once, { text: 'mime' })).toEqual({ text: 'echo:mime' })
  })

  it('takes a compiled Module, so one build can run twice', async () => {
    const mod = await WebAssembly.compile(emptyModule)
    const go = new FakeGo()
    serving(go)
    const sock = await open(mod, { worker: false, go })
    socks.push(sock)
    expect(await sock.dial().invoke(echo.once, { text: 'mod' })).toEqual({ text: 'echo:mod' })
  })

  it('waits as long as it takes when readyTimeoutMs is off', async () => {
    // <= 0 is the documented way to wait forever, for a module whose main does
    // something slow before it can serve. Nothing else may end the wait: with
    // every protocol timer off (§10.6) a wrong answer here is a page that
    // hangs, so the only two arms left are the publish and the instance dying.
    const go = new FakeGo()
    const net = serving(go)
    const publish = go.onRun
    go.onRun = () => {
      setTimeout(() => publish(go), 30)
    }
    const sock = await here(go, { readyTimeoutMs: 0 })
    expect(await sock.dial().invoke(echo.once, { text: 'slow' })).toEqual({ text: 'echo:slow' })
    expect(net.counts.once).toBe(1)
  })

  it('serves under the entry point it was given', async () => {
    const go = new FakeGo()
    go.entryPoint = 'todoServe'
    serving(go)
    const sock = await here(go, { entryPoint: 'todoServe' })
    expect(await sock.dial().invoke(echo.once, { text: 'named' })).toEqual({ text: 'echo:named' })
    // Nothing was left under the default name either — afterEach checks both.
    expect(Object.hasOwn(globalThis, DefaultEntryPoint)).toBe(false)
  })
})

describe('teardown is wired for you (§4.5)', () => {
  it('an instance that exits fails every live call, saying so', async () => {
    const go = new FakeGo()
    serving(go)
    const conn = (await here(go)).dial()
    const stream = conn.newStream(echo.live, {})
    await stream.send({ text: 'x' })
    expect(await stream.recv()).toEqual({ text: 'echo:x' })

    // os.Exit runs nothing — no goodbye — and a MessagePort whose peer
    // stopped existing looks exactly like a quiet one. With every protocol
    // timer off (§10.6), this wiring is the only thing that ever fails the
    // call that was in flight.
    go.exit()
    const err = (await stream.recv().catch((e) => e)) as StatusError
    expect(err.code).toBe(Code.UNAVAILABLE)
    expect(err.message).toMatch(/the wasm instance exited/)
  })

  it('defuses the corpse before saying goodbye to it', async () => {
    // The instance's js.Funcs are still registered on its port — os.Exit
    // detaches nothing — and the goodbye close() posts is one more event
    // wasm_exec would re-enter the dead runtime for, throwing "Go program has
    // already exited" out of an event handler: a console error in a page, a
    // dead process in node. The re-entry point is neutralized first, so every
    // stale callback is the no-op it should have been.
    const go = new FakeGo()
    serving(go)
    const conn = (await here(go)).dial()
    expect(await conn.invoke(echo.once, { text: 'hi' })).toEqual({ text: 'echo:hi' })

    go._resume() // while it is alive, the re-entry point is the real one
    expect(go.resumes).toBe(1)

    go.exit()
    await tick()
    go._resume() // and afterwards, whatever still calls it gets a no-op
    expect(go.resumes).toBe(1)
  })

  it('a trap arrives as the cause instead', async () => {
    const go = new FakeGo()
    serving(go)
    const conn = (await here(go)).dial()
    const stream = conn.newStream(echo.live, {})
    await stream.send({ text: 'x' })
    expect(await stream.recv()).toEqual({ text: 'echo:x' })

    go.trap(new Error('unreachable executed'))
    const err = (await stream.recv().catch((e) => e)) as StatusError
    expect(err.code).toBe(Code.UNAVAILABLE)
    expect(err.message).toMatch(/unreachable executed/)
  })

  it('close() fails what was in flight instead of letting it hang', async () => {
    const go = new FakeGo()
    serving(go)
    const sock = await here(go)
    const conn = sock.dial()
    const stream = conn.newStream(echo.live, {})
    await stream.send({ text: 'x' })
    expect(await stream.recv()).toEqual({ text: 'echo:x' })

    sock.close()
    const err = (await stream.recv().catch((e) => e)) as StatusError
    expect(err.code).toBe(Code.UNAVAILABLE)
    expect(err.message).toMatch(/sock was closed/)
    // Idempotent, and it says the instance is out of reach even though this
    // realm cannot actually stop it.
    sock.close()
    expect(() => sock.dial()).toThrow(/this sock is closed/)
    expect(String(await sock.exited)).toMatch(/sock was closed/)
  })
})

describe('a start that cannot succeed says why, and never hangs', () => {
  it('rejects when the instance exits before publishing', async () => {
    const go = new FakeGo()
    go.onRun = (g) => g.exit() // main returned, or a panic: both resolve run()
    await expect(open(emptyModule, { worker: false, go })).rejects.toThrow(/exited before publishing globalThis\.drpcServe/)
  })

  it('rejects when the instance traps before publishing, carrying the trap', async () => {
    const go = new FakeGo()
    const boom = new Error('CompileError: junk after section')
    go.onRun = (g) => g.trap(boom)
    const err = (await open(emptyModule, { worker: false, go }).catch((e) => e)) as Error
    expect(err.message).toMatch(/failed before publishing globalThis\.drpcServe/)
    expect(err.cause).toBe(boom)
  })

  it('gives up on a module that never publishes, naming what it waited for', async () => {
    const go = new FakeGo() // neither publishes nor exits
    await expect(open(emptyModule, { worker: false, go, readyTimeoutMs: 20 })).rejects.toThrow(/no globalThis\.drpcServe after 20 ms/)
    expect(go.runs).toBe(1) // it did start; nothing here can stop it again
  })

  it('reports a broken build with the compiler output the server sent', async () => {
    // The dev server rebuilds on demand and answers a failure with a 500
    // whose body is what the compiler said — the only thing in the whole
    // failure that names the line, and what instantiateStreaming discards.
    vi.stubGlobal('fetch', () => Promise.resolve(new Response('# example/wasm\n./main.go:42:2: undefined: gw.Serve\n', { status: 500, statusText: 'Internal Server Error' })))
    const go = new FakeGo()
    await expect(open('/app.wasm', { worker: false, go })).rejects.toThrow(/undefined: gw\.Serve/)
    expect(go.runs).toBe(0) // nothing was started, so nothing is left running
  })

  it('says how to get wasm_exec.js when the realm has no Go runtime', async () => {
    // The one file this package cannot ship: it is version-coupled to the
    // compiler that built the module. The error is the only place anybody will
    // be told where to find it, so it carries the command.
    vi.stubGlobal('fetch', () => Promise.resolve(new Response('not found', { status: 404, statusText: 'Not Found' })))
    const err = (await open(emptyModule, { worker: false }).catch((e) => e)) as Error
    expect(err.message).toMatch(/GET \/wasm_exec\.js failed \(404 Not Found\)/)
    expect(err.message).toMatch(/cp "\$\(go env GOROOT\)\/lib\/wasm\/wasm_exec\.js" \.\/public\//)
  })

  it('rejects when the entry point is set to something that is not a function', async () => {
    const go = new FakeGo()
    go.onRun = (g) => g.publish('ready!')
    await expect(open(emptyModule, { worker: false, go })).rejects.toThrow(/set to a string, not a function/)
  })

  it('refuses a second start while the first is still waiting for the name', async () => {
    // Two accessors cannot both catch one publish: whichever instance
    // published first would be handed to whichever accessor is installed —
    // a crossed wire, not an error anyone would find.
    const first = open(emptyModule, { worker: false, go: new FakeGo(), readyTimeoutMs: 20 })
    await expect(open(emptyModule, { worker: false, go: new FakeGo() })).rejects.toThrow(/already waiting for globalThis\.drpcServe/)
    await expect(first).rejects.toThrow(/no globalThis\.drpcServe/)
  })

  it('refuses a Go instance it would have to post to a worker', async () => {
    // A Go instance belongs to the realm that built it; it cannot be cloned
    // into a worker. Ignoring it would run a second, empty runtime instead —
    // with the argv and env it was given silently gone.
    await expect(open(emptyModule, { go: new FakeGo() })).rejects.toThrow(/pass \{ worker: false \} with it/)
  })

  it('says so where there is no Worker to spawn', async () => {
    // node, and anything else without the DOM constructor: the default path
    // has to name the way out rather than throw "Worker is not defined".
    await expect(open(emptyModule)).rejects.toThrow(/this realm has no Worker/)
  })
})

describe('the global is scaffolding, not state', () => {
  it('runs a second instance on the same entry point, with no crossed wires', async () => {
    const a = new FakeGo()
    const na = serving(a)
    const connA = (await here(a)).dial()
    const b = new FakeGo()
    const nb = serving(b)
    const connB = (await here(b)).dial()

    expect(await connA.invoke(echo.once, { text: 'a' })).toEqual({ text: 'echo:a' })
    expect(await connB.invoke(echo.once, { text: 'b' })).toEqual({ text: 'echo:b' })
    // Each call reached exactly one server: the second start caught its own
    // instance's publish, not the first's leftover function.
    expect(na.counts.once).toBe(1)
    expect(nb.counts.once).toBe(1)

    // And the two are independent: killing one leaves the other serving.
    a.exit()
    const err = (await connA.invoke(echo.once, { text: 'gone' }).catch((e) => e)) as StatusError
    expect(err.code).toBe(Code.UNAVAILABLE)
    expect(await connB.invoke(echo.once, { text: 'still' })).toEqual({ text: 'echo:still' })
  })

  it('leaves a global it found exactly as it was', async () => {
    // The page may host something else under this name; catching one publish
    // is not a reason to take it permanently.
    const sentinel = (): void => {}
    Reflect.set(globalThis, DefaultEntryPoint, sentinel)
    try {
      const go = new FakeGo()
      serving(go)
      await here(go)
      expect(Reflect.get(globalThis, DefaultEntryPoint)).toBe(sentinel)
    } finally {
      Reflect.deleteProperty(globalThis, DefaultEntryPoint)
    }
  })

  it('gives the name back on a failure path too', async () => {
    // Same duty where it is easiest to forget: a start that never got an entry
    // point still borrowed the property, and the page's own value has to
    // survive it. jsport.Gateway.Serve is the other half of this — it
    // unpublishes only what is still its own, precisely because the property
    // has normally gone back to the page by the time it does.
    const sentinel = (): void => {}
    Reflect.set(globalThis, DefaultEntryPoint, sentinel)
    try {
      const go = new FakeGo()
      go.onRun = (g) => g.exit()
      await expect(open(emptyModule, { worker: false, go })).rejects.toThrow(/exited before publishing/)
      expect(Reflect.get(globalThis, DefaultEntryPoint)).toBe(sentinel)
    } finally {
      Reflect.deleteProperty(globalThis, DefaultEntryPoint)
    }
  })

  it('reads undefined while it is waiting, which is what the Go side checks', async () => {
    // The other direction of the readiness contract. jsport.Gateway.Serve
    // refuses to publish over a name that is already set, and the accessor
    // installed here IS a value on globalThis — so if its getter answered
    // anything but undefined, a server started with Serve would refuse to
    // start, with the two halves each waiting for the other.
    const go = new FakeGo()
    const net = serving(go)
    const publish = go.onRun
    let seen: unknown = 'never read'
    go.onRun = () => {
      seen = Reflect.get(globalThis, DefaultEntryPoint) // Serve's own check
      expect(Object.hasOwn(globalThis, DefaultEntryPoint)).toBe(true)
      publish(go)
    }
    const sock = await here(go)
    expect(seen).toBeUndefined()
    expect(await sock.dial().invoke(echo.once, { text: 'both ways' })).toEqual({ text: 'echo:both ways' })
    expect(net.counts.once).toBe(1)
  })
})

describe('one instance, many connections', () => {
  it('dial() again for an independent peer on the same instance', async () => {
    const go = new FakeGo()
    const net = serving(go)
    const sock = await here(go)
    const first = sock.dial()
    const second = sock.dial()

    // Two peers, not two views of one: the gateway keyed each port on its own
    // key (§6.4), so their calls are attributed separately.
    expect(await first.invoke(echo.once, { text: 'a' })).toEqual({ text: 'echo:a' })
    expect(await second.invoke(echo.once, { text: 'b' })).toEqual({ text: 'echo:b' })
    expect(net.counts.once).toBe(2)

    // And one going away is one teardown: closing the second must not touch
    // the first, which is the whole claim of per-peer state.
    second.close()
    await tick()
    expect(await first.invoke(echo.once, { text: 'still here' })).toEqual({ text: 'echo:still here' })
    first.close()
  })

  it('fails every connection when the instance dies, not just the first', async () => {
    const go = new FakeGo()
    serving(go)
    const sock = await here(go)
    const first = sock.dial()
    const second = sock.dial()
    const live = second.newStream(echo.live)
    await live.send({ text: 'hold' })
    expect(await live.recv()).toEqual({ text: 'echo:hold' })

    go.exit()
    await tick()

    // §4.5: with no protocol timers running, this teardown is the only thing
    // that can ever fail these calls — on every connection the instance had.
    for (const conn of [first, second]) {
      const err = (await conn.invoke(echo.once, { text: 'anyone' }).catch((e: unknown) => e)) as StatusError
      expect(err.code).toBe(Code.UNAVAILABLE)
    }
    expect(String(await sock.exited)).toMatch(/the wasm instance exited/)
  })

  it('refuses a connection to an instance that has exited, with the diagnosis', async () => {
    const go = new FakeGo()
    serving(go)
    const sock = await here(go)
    sock.dial().close()
    go.exit()
    await tick()

    // Not "no such instance": a connection to a corpse would hang forever
    // rather than fail, so the error has to say which of the two happened.
    expect(() => sock.dial()).toThrow(/the wasm instance has exited/)
  })
})

// ---------------------------------------------------------------------------
// one instance, many servers
// ---------------------------------------------------------------------------

// A second gateway in the same program (jsport.WithEntryPoint) is a different
// axis from a second dial(): another drpc.Server with its own registry and its
// own interceptors, sharing the module, the runtime, the memory and the
// lifetime. The page reaches it by name.
describe('one instance, many servers', () => {
  it('dials the second entry point, and the two are different servers', async () => {
    const go = new FakeGo()
    const control = serving(go)
    const admin = servingAlso(go)
    const sock = await here(go)

    const c = sock.dial()
    const a = sock.dial({ entryPoint: secondEntryPoint })
    expect(await c.invoke(echo.once, { text: 'c' })).toEqual({ text: 'echo:c' })
    expect(await a.invoke(echo.once, { text: 'a' })).toEqual({ text: 'echo:a' })

    // Two registries, not one served twice.
    expect(control.counts.once).toBe(1)
    expect(admin.counts.once).toBe(1)
  })

  it('waits for a second gateway that has not published yet', async () => {
    // The point of the whole feature. Readiness is the FIRST entry point, so a
    // program that yields to the event loop between its two gateways — a
    // fetch, a database opening, a sleep — is reachable here before the second
    // has run, and whether it is depends on Go's scheduler. Refusing the dial
    // would make that scheduling decide whether the page works.
    const go = new FakeGo()
    serving(go)
    const admin = servingAlso(go, { after: 30 })
    const sock = await here(go)
    expect(Object.hasOwn(globalThis, secondEntryPoint)).toBe(false) // not there yet

    // Synchronous even so: the port queues the call until the far side binds it.
    const a = sock.dial({ entryPoint: secondEntryPoint })
    expect(await a.invoke(echo.once, { text: 'early' })).toEqual({ text: 'echo:early' })
    expect(admin.counts.once).toBe(1)
  })

  it('two dials to one unpublished name share the wait', async () => {
    // The accessor may be installed once, so a second dial cannot claim the
    // name for itself; both want the same answer and both must get it.
    const go = new FakeGo()
    serving(go)
    servingAlso(go, { after: 30 })
    const sock = await here(go)

    const [first, second] = [sock.dial({ entryPoint: secondEntryPoint }), sock.dial({ entryPoint: secondEntryPoint })]
    expect(await first.invoke(echo.once, { text: 'a' })).toEqual({ text: 'echo:a' })
    expect(await second.invoke(echo.once, { text: 'b' })).toEqual({ text: 'echo:b' })
  })

  it('leaves the dialled name on globalThis for the next dial to read', async () => {
    // Unlike the start's, which open() takes back off. This one is the
    // gateway's: it published once and will not do it again.
    const go = new FakeGo()
    serving(go)
    servingAlso(go, { after: 10 })
    const sock = await here(go)
    sock.dial({ entryPoint: secondEntryPoint }).close()
    await new Promise((res) => setTimeout(res, 30))
    expect(typeof (globalThis as Record<string, unknown>)[secondEntryPoint]).toBe('function')

    const again = sock.dial({ entryPoint: secondEntryPoint })
    expect(await again.invoke(echo.once, { text: 'again' })).toEqual({ text: 'echo:again' })
  })

  it('fails only that connection when the name never arrives', async () => {
    // Bounded by the same readyTimeoutMs the start used. The instance is alive
    // and every other connection to it is untouched — and with no protocol
    // timers (§10.6) nothing but this would ever end the call.
    const go = new FakeGo()
    const control = serving(go)
    const sock = await here(go, { readyTimeoutMs: 30 })
    const ok = sock.dial()
    const nowhere = sock.dial({ entryPoint: 'drpcNoSuchServer' })

    const err = (await nowhere.invoke(echo.once, { text: 'nobody' }).catch((e: unknown) => e)) as StatusError
    expect(err.code).toBe(Code.UNAVAILABLE)
    expect(String(err.cause ?? err)).toMatch(/drpcNoSuchServer/)

    expect(await ok.invoke(echo.once, { text: 'fine' })).toEqual({ text: 'echo:fine' })
    expect(control.counts.once).toBe(1)
    expect(Object.hasOwn(globalThis, 'drpcNoSuchServer')).toBe(false) // no accessor left behind
  })

  it('ends a waiting connection when the instance dies under it', async () => {
    const go = new FakeGo()
    serving(go)
    servingAlso(go, { after: 10_000 }) // never, for this test's purposes
    const sock = await here(go)
    const waiting = sock.dial({ entryPoint: secondEntryPoint })

    go.exit()
    const err = (await waiting.invoke(echo.once, { text: 'anyone' }).catch((e: unknown) => e)) as StatusError
    expect(err.code).toBe(Code.UNAVAILABLE)
  })

  it('does not resolve a name to something inherited from the prototype chain', async () => {
    // `toString` is a function on every realm, through Object.prototype rather
    // than as globalThis's own. A plain lookup would find it and CALL it with
    // a port; a mistyped entryPoint has to report instead.
    const go = new FakeGo()
    serving(go)
    const sock = await here(go, { readyTimeoutMs: 20 })
    const conn = sock.dial({ entryPoint: 'toString' })
    const err = (await conn.invoke(echo.once, { text: 'x' }).catch((e: unknown) => e)) as StatusError
    expect(err.code).toBe(Code.UNAVAILABLE)
    expect(String(err.cause ?? err)).toMatch(/no globalThis\.toString after 20 ms/)
    // And the shadow the wait cast over it is gone.
    expect(Object.hasOwn(globalThis, 'toString')).toBe(false)
    expect(typeof globalThis.toString).toBe('function')
  })

  it('carries the entry point to the worker, and omits it when there is none', async () => {
    // The page's half of the contract; the routing itself is the worker's, in
    // worker.test.ts. Omitted rather than filled in with the started name:
    // which name that is belongs to the realm holding the instance.
    const w = new FakeWorker()
    const sock = await withWorker(w)
    sock.dial()
    sock.dial({ entryPoint: secondEntryPoint })
    expect(w.serves).toEqual([{ drpc: 'serve' }, { drpc: 'serve', entryPoint: secondEntryPoint }])
  })
})

// ---------------------------------------------------------------------------
// the instance in a worker
// ---------------------------------------------------------------------------

describe('the worker path', () => {
  it('starts the instance with every default already resolved', async () => {
    const w = new FakeWorker()
    await withWorker(w, { entryPoint: 'todoServe', readyTimeoutMs: 250, wasmExec: '/static/wasm_exec.js' })
    // Resolved here rather than defaulted again on the far side: the two
    // halves ship separately, so a default the worker filled in would be the
    // one place they could silently disagree.
    expect(w.starts).toEqual([{ drpc: 'start', app: emptyModule, entryPoint: 'todoServe', readyTimeoutMs: 250, wasmExec: '/static/wasm_exec.js' }])
  })

  it('dials over a transferred port, never over the worker itself', async () => {
    const w = new FakeWorker()
    const sock = await withWorker(w)
    expect(sock.worker).toBe(w)
    const conn = sock.dial()
    expect(await conn.invoke(echo.once, { text: 'hi' })).toEqual({ text: 'echo:hi' })
    expect(w.counts.once).toBe(1)

    // The port rides the transfer list and nothing else does: a MessagePort
    // cannot be cloned into a message, and a plain object can never be
    // mistaken for a frame on the worker's own channel.
    expect(w.transferred).toHaveLength(1)
    expect(w.transferred[0]).toBeInstanceOf(MessagePort)
    expect(w.stray).toEqual([])
    conn.close()
  })

  it('delivers the first call of a dial() the instance has not bound yet', async () => {
    // Rule 1, and the reason dial() is synchronous. What is transferred is a
    // MessagePort, and a MessagePort queues everything posted into it until
    // its owner binds it — which the Go adapter does when it is handed one. So
    // a call opened on the tick open() resolved is delivered late, never
    // dropped. Handing the worker itself to a PortTransport would lose it: a
    // worker's global scope drops what arrives before a handler is registered.
    const w = new FakeWorker()
    w.autoBind = false
    const sock = await withWorker(w)
    const conn = sock.dial()
    const call = conn.invoke(echo.once, { text: 'early' })
    await tick() // the whole OPEN is on the wire, with nobody bound to receive it

    w.bind() // the instance takes the port only now
    expect(await call).toEqual({ text: 'echo:early' })
    conn.close()
  })

  it('dial() again for an independent peer, through a second port', async () => {
    const w = new FakeWorker()
    const sock = await withWorker(w)
    const first = sock.dial()
    const second = sock.dial()
    expect(await first.invoke(echo.once, { text: 'a' })).toEqual({ text: 'echo:a' })
    expect(await second.invoke(echo.once, { text: 'b' })).toEqual({ text: 'echo:b' })
    expect(w.transferred).toHaveLength(2) // one port is one peer (§6.4)
    expect(w.counts.once).toBe(2)

    second.close()
    await tick()
    expect(await first.invoke(echo.once, { text: 'still here' })).toEqual({ text: 'echo:still here' })
    first.close()
  })

  it('rejects with what the worker said when the instance cannot start', async () => {
    // The page can see neither the fetch nor go.run() from where it stands, so
    // the worker's sentence is the whole diagnosis; a page that only hung
    // would blame the wrong half.
    const w = new FakeWorker()
    w.autoReady = false
    const opening = open(emptyModule, { worker: w })
    await tick()
    w.failed('GET /app.wasm failed: 500 Internal Server Error\n./main.go:42:2: undefined: gw.Serve')
    await expect(opening).rejects.toThrow(/undefined: gw\.Serve/)
    // A worker it was handed is not one it may kill.
    expect(w.terminated).toBe(0)
    expect(w.listenerCount).toBe(0) // and nothing of ours is left on it
  })

  it('fails every connection when the worker reports the instance gone (§4.5)', async () => {
    const w = new FakeWorker()
    const sock = await withWorker(w)
    const conn = sock.dial()
    const stream = conn.newStream(echo.live, {})
    await stream.send({ text: 'x' })
    expect(await stream.recv()).toEqual({ text: 'echo:x' })

    w.died('the wasm instance exited')
    const err = (await stream.recv().catch((e) => e)) as StatusError
    expect(err.code).toBe(Code.UNAVAILABLE)
    expect(err.message).toMatch(/the wasm instance exited/)
    expect(String(await sock.exited)).toMatch(/the wasm instance exited/)
    expect(() => sock.dial()).toThrow(/the wasm instance has exited/)
  })

  it('reports a worker that could not be loaded at all', async () => {
    // A wrong workerUrl, or a module that threw on evaluation: the error event
    // is the only thing that ever arrives, and without it a bad path looks
    // exactly like a module that is slow to publish — for a readyTimeoutMs
    // that the worker, never having run, cannot count.
    const w = new FakeWorker()
    w.autoReady = false
    const opening = open(emptyModule, { worker: w })
    await tick()
    w.raise('error', { message: 'Failed to load worker.mjs' })
    await expect(opening).rejects.toThrow(/the worker itself failed: Failed to load worker\.mjs/)
  })

  it('terminates a worker it made, and never one it was given', async () => {
    const w = new FakeWorker()
    const sock = await withWorker(w)
    const conn = sock.dial()
    expect(await conn.invoke(echo.once, { text: 'hi' })).toEqual({ text: 'echo:hi' })

    sock.close()
    // Given to open(), so it may be hosting something else of the
    // application's own: killing it is not this sock's decision.
    expect(w.terminated).toBe(0)
    expect(w.listenerCount).toBe(0)
    // The connection is closed all the same, which is the half that matters:
    // a live call with nothing to end it would hang forever (§10.6).
    const err = (await conn.invoke(echo.once, { text: 'after' }).catch((e) => e)) as StatusError
    expect(err.code).toBe(Code.UNAVAILABLE)
    expect(err.message).toMatch(/sock was closed/)
  })

  it('close() after the instance is gone keeps the diagnosis, and still ends the worker', async () => {
    const w = new FakeWorker()
    const sock = await withWorker(w)
    const conn = sock.dial()
    w.died('the wasm instance exited')
    await tick()

    // close() is only the page catching up with a death it has already been
    // told about: it may not overwrite the one sentence anybody will read, and
    // it must still be safe to call — a worker outlives the program that ran
    // in it, so `exited` is exactly what a page closes on.
    sock.close()
    expect(String(await sock.exited)).toMatch(/the wasm instance exited/)
    expect(() => sock.dial()).toThrow(/the wasm instance exited/)
    const err = (await conn.invoke(echo.once, { text: 'anyone' }).catch((e) => e)) as StatusError
    expect(err.code).toBe(Code.UNAVAILABLE)
    sock.close() // idempotent on a sock that was already dead when it was closed
  })

  it('leaves the connections alone when something else in the worker throws', async () => {
    // A worker open() started may still be the application's own, and an
    // uncaught exception in ITS code fires `error` on the worker object — which
    // a worker survives. After `ready` that is somebody else's bug, and it may
    // not tear healthy connections down any more than somebody else's message
    // may (§4.2); the instance's own death arrives as `exited`, with a goodbye
    // on every port. Before `ready` the same event is the only report a worker
    // that would not load ever makes, which is the case below.
    const w = new FakeWorker()
    const sock = await withWorker(w)
    const conn = sock.dial()
    w.raise('error', { message: 'Uncaught TypeError: render is not a function' })
    await tick()

    expect(await conn.invoke(echo.once, { text: 'still here' })).toEqual({ text: 'echo:still here' })
    expect(sock.dial()).toBeDefined()
    conn.close()
  })

  it('spawns the worker this package ships, and terminates the one it made', async () => {
    vi.stubGlobal('Worker', SpawnedWorker)
    const sock = await open(emptyModule)
    socks.push(sock)
    const made = spawned[0]
    expect(made).toBeDefined()
    // A module worker at a URL resolved from this entry's own — dist/wasm.mjs
    // beside dist/wasm/worker.mjs, which is the published layout that makes the
    // relative path right. Nothing in node can run the real one; what is pinned
    // here is the resolution rule and the type, and both are silent when wrong.
    expect(String(made!.url)).toMatch(/\/wasm\/worker\.mjs$/)
    expect(made!.opts).toEqual({ type: 'module' })
    expect(sock.worker).toBe(made!.worker)

    const conn = sock.dial()
    expect(await conn.invoke(echo.once, { text: 'hi' })).toEqual({ text: 'echo:hi' })
    sock.close()
    // Made here, so ending it is this sock's to do — and only after the
    // connections were closed, since terminate() discards the goodbye that
    // would otherwise have ended the call in flight (§10.6).
    expect(made!.worker.terminated).toBe(1)
    expect(made!.worker.listenerCount).toBe(0)
    const err = (await conn.invoke(echo.once, { text: 'after' }).catch((e) => e)) as StatusError
    expect(err.code).toBe(Code.UNAVAILABLE)
  })

  it('takes the URL a bundler produced instead, and ends that worker too on a failed start', async () => {
    // Terminating is the whole of what a host can do about an instance that is
    // already running: nothing stops a Go program once go.run has, so the realm
    // is what goes. A worker open() did not make is left alone on this path,
    // which the rejection case above asserts.
    vi.stubGlobal('Worker', SpawnedWorker)
    spawnReady = false
    const opening = open(emptyModule, { workerUrl: '/assets/drpc-worker-a1b2c3.js' })
    await tick()
    expect(String(spawned[0]?.url)).toBe('/assets/drpc-worker-a1b2c3.js')

    spawned[0]!.worker.failed('no globalThis.drpcServe after 10000 ms — is that the name the server serves under?')
    await expect(opening).rejects.toThrow(/no globalThis\.drpcServe after 10000 ms/)
    expect(spawned[0]!.worker.terminated).toBe(1)
    expect(spawned[0]!.worker.listenerCount).toBe(0)
  })

  it('ignores traffic on the worker that is not ours', async () => {
    // A worker started by open() may still be the application's own, doing
    // something else on the same channel. Anything untagged is somebody
    // else's, and it may not tear this down.
    const w = new FakeWorker()
    const sock = await withWorker(w)
    const conn = sock.dial()
    w.emit({ kind: 'progress', done: 3 })
    w.emit('a string')
    await tick()
    expect(await conn.invoke(echo.once, { text: 'still here' })).toEqual({ text: 'echo:still here' })
    conn.close()
  })
})

// ---------------------------------------------------------------------------
// the transport underneath is the shipped one
// ---------------------------------------------------------------------------

describe('port options reach the connection', () => {
  it('refuses an oversize envelop on a dialled connection (§4.4)', async () => {
    const go = new FakeGo()
    serving(go)
    const sock = await here(go, { maxMessageSize: 128 })
    const conn = sock.dial()
    const err = (await conn.invoke(echo.once, { text: 'x'.repeat(500) }).catch((e) => e)) as StatusError
    expect(err.code).toBe(Code.RESOURCE_EXHAUSTED)
    // The channel survives a refused send: a small call still works.
    expect(await conn.invoke(echo.once, { text: 'ok' })).toEqual({ text: 'echo:ok' })
  })

  it('hands each dialled connection its own ConnOptions', async () => {
    const go = new FakeGo()
    serving(go)
    const sock = await here(go)
    // Per dial, not per sock: two connections to one instance are two
    // independent peers (§6.4) and may be configured as such. Overriding the
    // mode the adapter reports (§4.3) is the visible proof they arrive — a
    // port is reliable, and only an option says otherwise.
    expect(sock.dial().reliable).toBe(true)
    expect(sock.dial({ reliable: false }).reliable).toBe(false)
  })
})
