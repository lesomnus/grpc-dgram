// startWasmServer (src/transport/port/wasm.ts) against a FAKE Go class: no
// toolchain, and every failure reachable in a millisecond. What the helper
// actually does is the glue around an instance — instantiate, catch the
// publish, hand over one end of a channel, wire the exit to close(cause) —
// and none of that is Go code. The fake publishes the way the real thing
// does, `Reflect.set(globalThis, name, fn)`, which is what
// `js.Global().Set(name, fn)` reaches JS as and is the reason an accessor can
// catch it at all; its run() promise is controllable, so the two ways an
// instance dies (a resolution — which is what a Go panic does — and a trap)
// are both testable here.
//
// The happy paths run a real Server behind a PortGateway on the far end, so a
// call over the returned transport is a genuine round trip through the wire
// and the §4.5 teardown, not a mock answering. The end-to-end proof against a
// Go server compiled to js/wasm is test/wasm.test.ts.

import { afterEach, describe, expect, it, vi } from 'vitest'
import { Conn } from '../../conn'
import { Server } from '../../server'
import { Code, type StatusError } from '../../status'
import { echo, registerEcho, tick } from '../../testing'
import { noop } from '../../util'
import { PortGateway, PortTransport } from './index'
import { DefaultEntryPoint, startWasmServer, wasmServer, type GoLike } from './wasm'

// The smallest thing WebAssembly.instantiate accepts: the 8-byte header of an
// empty module — no imports, no exports, nothing to run. The helper only ever
// hands the instance to go.run(), and the fake ignores it.
const emptyModule = new Uint8Array([0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00])

// FakeGo is wasm_exec's Go class reduced to what the helper touches, plus the
// controls a test needs: what the instance does when it starts (onRun, i.e.
// main), and how it ends.
class FakeGo implements GoLike {
  readonly importObject: WebAssembly.Imports = {}
  entryPoint = DefaultEntryPoint
  runs = 0
  // wasm_exec's re-entry point, which every js.Func the instance registered
  // calls. The real one throws "Go program has already exited" once run() has
  // settled; this one records that it was reached.
  _resume = (): void => {
    this.resumes++
  }
  resumes = 0
  // onRun stands in for main(): a server publishes, a broken one exits.
  onRun: (go: FakeGo) => void = noop
  private readonly exited: Promise<void>
  private stop!: () => void
  private crash!: (e: unknown) => void

  constructor() {
    this.exited = new Promise((res, rej) => {
      this.stop = res
      this.crash = rej
    })
  }

  run(_instance: WebAssembly.Instance): Promise<void> {
    this.runs++
    this.onRun(this)
    return this.exited
  }

  // publish is js.Global().Set(name, fn) seen from JS.
  publish(fn: unknown): void {
    Reflect.set(globalThis, this.entryPoint, fn)
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

const started: PortTransport[] = []
const gateways: PortGateway[] = []

// serving turns a FakeGo into a working server: its main publishes an entry
// point that serves each handed port off a real Server. The returned counters
// are what proves two instances are not crossing wires.
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

// start records the transport so afterEach can release its end of the channel.
async function start(go: FakeGo, opts: Parameters<typeof startWasmServer>[1] = {}): Promise<PortTransport> {
  const tx = await startWasmServer(emptyModule, { go, ...opts })
  started.push(tx)
  return tx
}

// track registers a transport a test made itself (a second connection, a
// transferred port) so afterEach releases its end of the channel too.
function track(tx: PortTransport): PortTransport {
  started.push(tx)
  return tx
}

afterEach(() => {
  // Both ends of every channel the helper made: the transport owns the one it
  // returned, the gateway the one the instance was given. A live MessagePort
  // keeps node's event loop alive and the run would never exit.
  for (const tx of started.splice(0)) tx.close()
  for (const gw of gateways.splice(0)) gw.close()
  vi.unstubAllGlobals()
  // The accessor is the helper's own scaffolding and no path may leave it
  // behind: a leftover one would swallow the next instance's publish into a
  // promise nobody is awaiting, and the start after it would time out with
  // the module working perfectly.
  for (const name of [DefaultEntryPoint, 'todoServe']) {
    expect(Object.hasOwn(globalThis, name)).toBe(false)
  }
})

describe('the two-line path', () => {
  it('returns a transport already talking to the instance', async () => {
    const go = new FakeGo()
    const net = serving(go)
    const tx = await start(go)
    const conn = new Conn(tx)

    expect(go.runs).toBe(1)
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
    const tx = await startWasmServer('/app.wasm', { go })
    started.push(tx)
    expect(urls).toEqual(['/app.wasm'])
    expect(await new Conn(tx).invoke(echo.once, { text: 'url' })).toEqual({ text: 'echo:url' })
  })

  it('instantiates a response the server mislabelled', async () => {
    // A static server that never heard of wasm answers application/octet-
    // stream, which instantiateStreaming refuses outright. Buffering is what
    // makes such a server work anyway.
    const go = new FakeGo()
    serving(go)
    const res = new Response(emptyModule, { headers: { 'content-type': 'application/octet-stream' } })
    const tx = await startWasmServer(res, { go })
    started.push(tx)
    expect(await new Conn(tx).invoke(echo.once, { text: 'mime' })).toEqual({ text: 'echo:mime' })
  })

  it('takes a compiled Module, so one build can run twice', async () => {
    const mod = await WebAssembly.compile(emptyModule)
    const go = new FakeGo()
    serving(go)
    const tx = await startWasmServer(mod, { go })
    started.push(tx)
    expect(await new Conn(tx).invoke(echo.once, { text: 'mod' })).toEqual({ text: 'echo:mod' })
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
    const tx = await start(go, { readyTimeoutMs: 0 })
    expect(await new Conn(tx).invoke(echo.once, { text: 'slow' })).toEqual({ text: 'echo:slow' })
    expect(net.counts.once).toBe(1)
  })

  it('serves under the entry point it was given', async () => {
    const go = new FakeGo()
    go.entryPoint = 'todoServe'
    serving(go)
    const tx = await start(go, { entryPoint: 'todoServe' })
    expect(await new Conn(tx).invoke(echo.once, { text: 'named' })).toEqual({ text: 'echo:named' })
    // Nothing was left under the default name either — afterEach checks both.
    expect(Object.hasOwn(globalThis, DefaultEntryPoint)).toBe(false)
  })
})

describe('teardown is wired for you (§4.5)', () => {
  it('an instance that exits fails every live call, saying so', async () => {
    const go = new FakeGo()
    serving(go)
    const conn = new Conn(await start(go))
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
    const tx = await start(go)
    const conn = new Conn(tx)
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
    const conn = new Conn(await start(go))
    const stream = conn.newStream(echo.live, {})
    await stream.send({ text: 'x' })
    expect(await stream.recv()).toEqual({ text: 'echo:x' })

    go.trap(new Error('unreachable executed'))
    const err = (await stream.recv().catch((e) => e)) as StatusError
    expect(err.code).toBe(Code.UNAVAILABLE)
    expect(err.message).toMatch(/unreachable executed/)
  })
})

describe('a start that cannot succeed says why, and never hangs', () => {
  it('rejects when the instance exits before publishing', async () => {
    const go = new FakeGo()
    go.onRun = (g) => g.exit() // main returned, or a panic: both resolve run()
    await expect(startWasmServer(emptyModule, { go })).rejects.toThrow(/exited before publishing globalThis\.drpcServe/)
  })

  it('rejects when the instance traps before publishing, carrying the trap', async () => {
    const go = new FakeGo()
    const boom = new Error('CompileError: junk after section')
    go.onRun = (g) => g.trap(boom)
    const err = (await startWasmServer(emptyModule, { go }).catch((e) => e)) as Error
    expect(err.message).toMatch(/failed before publishing globalThis\.drpcServe/)
    expect(err.cause).toBe(boom)
  })

  it('gives up on a module that never publishes, naming what it waited for', async () => {
    const go = new FakeGo() // neither publishes nor exits
    await expect(startWasmServer(emptyModule, { go, readyTimeoutMs: 20 })).rejects.toThrow(/no globalThis\.drpcServe after 20 ms/)
    expect(go.runs).toBe(1) // it did start; nothing here can stop it again
  })

  it('reports a broken build with the compiler output the server sent', async () => {
    // The dev server rebuilds on demand and answers a failure with a 500
    // whose body is what the compiler said — the only thing in the whole
    // failure that names the line, and what instantiateStreaming discards.
    const go = new FakeGo()
    const res = new Response('# example/wasm\n./main.go:42:2: undefined: gw.Serve\n', { status: 500, statusText: 'Internal Server Error' })
    await expect(startWasmServer(res, { go })).rejects.toThrow(/undefined: gw\.Serve/)
    expect(go.runs).toBe(0) // nothing was started, so nothing is left running
  })

  it('rejects when the entry point is set to something that is not a function', async () => {
    const go = new FakeGo()
    go.onRun = (g) => g.publish('ready!')
    await expect(startWasmServer(emptyModule, { go })).rejects.toThrow(/set to a string, not a function/)
  })

  it('refuses a second start while the first is still waiting for the name', async () => {
    // Two accessors cannot both catch one publish: whichever instance
    // published first would be handed to whichever accessor is installed —
    // a crossed wire, not an error anyone would find.
    const first = startWasmServer(emptyModule, { go: new FakeGo(), readyTimeoutMs: 20 })
    await expect(startWasmServer(emptyModule, { go: new FakeGo() })).rejects.toThrow(/already waiting for globalThis\.drpcServe/)
    await expect(first).rejects.toThrow(/no globalThis\.drpcServe/)
  })
})

describe('the global is scaffolding, not state', () => {
  it('runs a second instance on the same entry point, with no crossed wires', async () => {
    const a = new FakeGo()
    const na = serving(a)
    const connA = new Conn(await start(a))
    const b = new FakeGo()
    const nb = serving(b)
    const connB = new Conn(await start(b))

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
      await start(go)
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
      await expect(startWasmServer(emptyModule, { go })).rejects.toThrow(/exited before publishing/)
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
    const tx = await start(go)
    expect(seen).toBeUndefined()
    expect(await new Conn(tx).invoke(echo.once, { text: 'both ways' })).toEqual({ text: 'echo:both ways' })
    expect(net.counts.once).toBe(1)
  })
})

describe('one instance, many connections', () => {
  it('connect() opens an independent peer on the running instance', async () => {
    const go = new FakeGo()
    const net = serving(go)
    const first = new Conn(await start(go))

    const handle = wasmServer()
    expect(handle.entryPoint).toBe(DefaultEntryPoint)
    const second = new Conn(track(handle.connect()))

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
    const first = new Conn(await start(go))
    const second = new Conn(track(wasmServer().connect()))
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
    expect(String(await wasmServer().exited)).toMatch(/the wasm instance exited/)
  })

  it('refuses a connection to an instance that has exited, with the diagnosis', async () => {
    const go = new FakeGo()
    serving(go)
    new Conn(await start(go)).close()
    go.exit()
    await tick()

    // Not "no such instance": a connection to a corpse would hang forever
    // rather than fail, so the error has to say which of the two happened.
    expect(() => wasmServer().connect()).toThrow(/has exited/)
    expect(() => wasmServer().openPort()).toThrow(/has exited/)
  })

  it('openPort() hands out the raw end, for transferring elsewhere', async () => {
    const go = new FakeGo()
    const net = serving(go)
    new Conn(await start(go)).close()

    // What a page posts to a Worker. Nothing wraps it here — whoever receives
    // it owns its transport, and its §4.5 duty.
    const port = wasmServer().openPort()
    const conn = new Conn(track(new PortTransport(port)))
    expect(await conn.invoke(echo.once, { text: 'from elsewhere' })).toEqual({ text: 'echo:from elsewhere' })
    expect(net.counts.once).toBe(1)
    conn.close()
  })

  it('throws for a name it never started', () => {
    expect(() => wasmServer('nothingStartedHere')).toThrow(/call startWasmServer first/)
  })
})
