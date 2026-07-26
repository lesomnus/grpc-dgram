// startWasmServer: the page-side wiring of a Go drpc.Server compiled to
// GOOS=js GOARCH=wasm, in one call —
//
//   const conn = new Conn(await startWasmServer('/app.wasm'))
//
// It is sugar over PortTransport and nothing more: it starts the instance,
// waits for it to publish its entry point (jsport.Gateway.Serve, published as
// DefaultEntryPoint), hands that entry point one end of a fresh
// MessageChannel, and returns a transport over the other. Every other shape —
// a Worker, an iframe, two TS endpoints — keeps the manual path in ./index,
// which this file neither extends nor replaces.
//
// It exists because the hand-written version of it has four hazards in it,
// and each of them fails in a way that points somewhere else:
//
//   - Readiness. The entry point exists only once the instance's main has
//     published it, and go.run()'s promise settles at EXIT, not at startup:
//     there is nothing to await but the property appearing. So an accessor
//     goes on globalThis[entryPoint] BEFORE go.run, and the publish cannot be
//     missed — js.Global().Set reaches JS as Reflect.set, which does trigger
//     an accessor. Publishing IS the readiness signal; no second magic name
//     exists on either side.
//   - Death on the way up. A Go panic RESOLVES go.run() (wasm_exec exits with
//     code 2 and resolves), so a settled run() before readiness is a failure,
//     never a success — and waiting on readiness alone would park forever,
//     since a reliable channel runs with every protocol timer off (§10.6) and
//     nothing else would ever time it out. Readiness is raced against the
//     exit, and against a wall clock for the module that simply never
//     publishes.
//   - A broken build. A dev server answers one with a 500 whose body is the
//     compiler output, and instantiateStreaming discards it: what reaches the
//     console is "the MIME type was not application/wasm", the one message
//     that does not say what stopped compiling. The response is checked, and
//     its body is the error.
//   - Teardown (§4.5). An instance that exits or panics posts no goodbye and
//     a MessagePort whose peer stopped existing looks exactly like one whose
//     peer is merely quiet, so go.run()'s promise is the only evidence that
//     ever arrives. It is wired to close(cause) here, which is the whole
//     reason a live call on this channel ever fails — and the corpse is
//     defused on the way (see makeInert), because the goodbye that close()
//     posts is itself an event a dead instance still tries to handle.
//
// And the part that confuses everyone: a MessageChannel is ONE channel with
// two entangled, symmetric ends — what is posted into one arrives at the
// other, and the only thing telling them apart is which one you give away.
// The helper makes the channel, so port1/port2 never appear in user code.
//
// One instance serves as many connections as it is given ports: one port is
// one peer (§6.4), with its own epoch, sid space, flow-control windows and
// per-peer resource caps, and a teardown that reaches only it. wasmServer()
// hands back the instance this file started so a second connection — for
// another view, or a Worker — costs a line instead of a rediscovered
// handshake.
//
// What it does not own is the instance's lifetime. Nothing here can stop a Go
// program once go.run has started it — wasm_exec has no kill switch, and
// os.Exit is the instance's own decision — so the failure paths are split by
// that line: everything that fails before go.run leaves nothing running, and
// a failure after it (a module that never publishes) rejects with an instance
// still alive, which only a page reload, a worker terminate or the module's
// own shutdown entry point can end. What it does clean up is what it made: the
// accessor, on every path including the successful one; and the channel, on
// every path that does not hand it over — once handed over, one end belongs to
// the transport (which closes it) and the other to the instance, so a start
// that succeeded owns neither.
//
// Dependency-free like the adapter it wraps: globalThis.Go comes from the
// toolchain's wasm_exec.js, which the page loads as a classic script and this
// file never imports — it is version-coupled to the compiler that produced
// the module, so vendoring it here would pin the wrong one.

import { noop, unrefTimer } from '../../util'
import { PortTransport, type PortOptions } from './index'

// DefaultEntryPoint is the global a wasm server publishes to say "I can serve
// now, hand me a port" — jsport.DefaultEntryPoint on the Go side. The two
// constants are the contract between the halves; keep them equal.
export const DefaultEntryPoint = 'drpcServe'

// DefaultReadyTimeoutMs bounds the wait for that publish, and nothing else:
// the clock starts once the module is instantiated and go.run has it, so a
// slow fetch or a dev server rebuilding on demand spends none of it. What it
// covers is main() reaching Serve, which is registration and no I/O — so this
// is generous by orders of magnitude, and still short enough that a module
// which never publishes reports itself instead of hanging a page forever.
export const DefaultReadyTimeoutMs = 10_000

// GoLike is the part of wasm_exec.js's Go class this helper touches. It is
// structural so a hand-built instance (one with args/env set, or a test
// double) fits without importing anything.
export interface GoLike {
  // The imports the compiled module needs; wasm_exec fills them in and binds
  // them to this instance, so an importObject belongs to exactly one run.
  readonly importObject: WebAssembly.Imports
  // Settles when the Go program exits — the §4.5 signal the host owes the
  // transport, since an exited instance posts no goodbye. It resolves for a
  // clean exit AND for a Go panic; only a wasm trap rejects it.
  run(instance: WebAssembly.Instance): Promise<void>
}

// WasmSource is everything the helper knows how to turn into an instance: a
// URL to fetch (as a string or a URL), a Response or the promise of one (so
// `fetch(u)` can be passed straight through), the module's bytes, or a Module
// compiled once and instantiated many times.
export type WasmSource = string | URL | Response | Promise<Response> | BufferSource | WebAssembly.Module

export interface WasmServerOptions extends PortOptions {
  // The global the instance publishes its port-taking function as; must match
  // the name the Go side serves under. Default DefaultEntryPoint.
  entryPoint?: string
  // A Go instance built by the caller — the way to pass argv or env, since
  // this helper sets neither. Default `new globalThis.Go()`.
  go?: GoLike
  // How long to wait for the publish; <= 0 waits forever. Default
  // DefaultReadyTimeoutMs.
  readyTimeoutMs?: number
}

// The function the instance publishes: it takes one port and serves one peer
// on it (PROTOCOL.md §6.4). jsport.Gateway.Serve binds the port before
// returning, so a call opened on this very tick loses nothing.
type ServeFn = (port: MessagePort) => void

// startWasmServer instantiates a Go wasm module, waits for it to publish its
// entry point, hands it one end of a fresh MessageChannel and returns a
// transport over the other — with the instance's exit already wired to
// close(cause), so a server that dies fails every live call saying why.
//
//   const conn = new Conn(await startWasmServer('/app.wasm'))
//
// Rejects, without leaving a global behind, if the module cannot be fetched
// or instantiated, if the instance dies before publishing, or if it has not
// published after readyTimeoutMs.
export async function startWasmServer(source: WasmSource, opts: WasmServerOptions = {}): Promise<PortTransport> {
  const name = opts.entryPoint ?? DefaultEntryPoint
  const go = opts.go ?? newGo()

  // Claimed before anything is fetched or run: the accessor has to be in
  // place before go.run for the publish to be catchable at all, and claiming
  // it first also means a second start on the same name is refused while this
  // one is still in the air rather than quietly stealing its publish.
  const entry = claimEntryPoint(name)
  try {
    const instance = await instantiate(source, go.importObject)
    const run = go.run(instance)
    const serve = await awaitPublish(entry.published, run, name, opts.readyTimeoutMs ?? DefaultReadyTimeoutMs)
    const live = register(name, serve, opts)
    const tx = live.connect(opts)
    // The §4.5 duty, which is why this helper returns a transport and not
    // just a port: there is no socket here to die, so the host says out loud
    // what only it can know. Without this a dead server leaves every call
    // hanging forever — reliable mode runs no timers at all (§10.6). Every
    // connection to this instance is failed, not just the first: they share
    // the process that stopped existing.
    const died = (cause: unknown): void => {
      makeInert(go)
      live.bury(cause)
    }
    void run.then(() => died(new Error('the wasm instance exited')), died)
    return tx
  } finally {
    // On every path: the accessor was ours, it was never meant to survive the
    // start, and a second startWasmServer must find the name as it was. The
    // entry point itself is kept in `started`, which is how a second
    // connection reaches this instance without the global (see wasmServer).
    entry.release()
  }
}

// wasmServer hands back an instance startWasmServer started, so a second
// connection to it is a line rather than a handshake rediscovered by hand:
//
//   const conn  = new Conn(await startWasmServer('/app.wasm'))
//   const other = new Conn(wasmServer().connect())   // independent peer, same server
//
// It throws for a name this file never started. An instance that has since
// died still answers — its `exited` is already resolved, which is what a page
// asks it — but `connect` and `openPort` refuse, because a connection to a
// corpse would hang forever rather than fail (§10.6: no timers). Pass the
// entry point when the page runs more than one instance.
export function wasmServer(entryPoint: string = DefaultEntryPoint): WasmServerHandle {
  const live = started.get(entryPoint)
  if (live === undefined) {
    throw new Error(`wasm: no instance started for globalThis.${entryPoint} — call startWasmServer first, or construct a PortTransport over a port you hold yourself`)
  }
  return live.handle
}

// WasmServerHandle is one running instance, seen from the page.
export interface WasmServerHandle {
  // The global this instance published under; the name it is known by here.
  readonly entryPoint: string
  // Resolves — never rejects — with the cause when the instance is gone; the
  // same cause its connections fail with. This is what the page waits on to
  // tear down anything it built around the instance (a Worker it gave a port
  // to, a UI that should say the server died).
  readonly exited: Promise<unknown>
  // connect opens another connection: a fresh channel, one end handed to the
  // instance as its own peer (§6.4), a transport over the other, and the same
  // death wiring the first one got. Throws once the instance has exited.
  connect(opts?: PortOptions): PortTransport
  // openPort is connect() without the transport: the raw end to transfer
  // somewhere else — `worker.postMessage({ port }, [port])` — where whoever
  // receives it wraps it in its own PortTransport. That side is then out of
  // this page's reach, so it owns its own §4.5 duty: nothing here can close a
  // port it has given away, and an instance that dies posts no goodbye. Wire
  // `exited` to whatever ends that side (terminating the worker is the honest
  // one) or the far end waits forever. Throws once the instance has exited.
  openPort(): MessagePort
}

// The instances this file has started, by entry point. Keyed that way because
// the entry point is already a globalThis name: a page that wants two
// instances gives them two names, and then has two handles.
const started = new Map<string, Live>()

interface Live {
  handle: WasmServerHandle
  connect(opts: PortOptions): PortTransport
  bury(cause: unknown): void
}

function register(name: string, serve: ServeFn, defaults: PortOptions): Live {
  const conns = new Set<PortTransport>()
  let dead: { cause: unknown } | undefined
  let announce!: (cause: unknown) => void
  const exited = new Promise<unknown>((res) => {
    announce = res
  })

  const alive = (): void => {
    if (dead !== undefined) {
      throw new Error(`wasm: the instance behind globalThis.${name} has exited${dead.cause instanceof Error ? `: ${dead.cause.message}` : ''}`)
    }
  }
  const openPort = (): MessagePort => {
    alive()
    const ch = new MessageChannel()
    serve(ch.port2) // one port is one peer (§6.4)
    return ch.port1
  }
  const connect = (opts: PortOptions = defaults): PortTransport => {
    alive()
    const tx = handOver(serve, opts)
    conns.add(tx)
    return tx
  }

  const live: Live = {
    handle: { entryPoint: name, exited, connect, openPort },
    connect,
    bury(cause: unknown) {
      if (dead !== undefined) return
      dead = { cause }
      // Left in the map rather than deleted: a later connect() must say the
      // instance died, which is a diagnosis, where a missing entry would only
      // say it was never started.
      for (const tx of conns) tx.close(cause)
      conns.clear()
      announce(cause)
    },
  }
  started.set(name, live)
  return live
}

// makeInert defuses the corpse before the goodbye is posted to it. An exited
// instance leaves its js.Funcs registered on the port it was handed — os.Exit
// runs nothing, so nothing detaches them — and wasm_exec re-enters the dead
// runtime for every event that still arrives, throwing "Go program has already
// exited" out of an event handler. The two events a dying endpoint produces
// are exactly the two this helper causes: the goodbye close() posts, and the
// channel closing under it. A page logs that and carries on; node treats an
// exception from an event handler as fatal and kills the process. Nothing can
// unregister those listeners, so the re-entry point is turned into the no-op
// it should have been — legal only because run() has settled, after which
// every call into it would have thrown anyway. Structural, since _resume is
// wasm_exec's own field and not part of GoLike.
function makeInert(go: GoLike): void {
  const inner = go as { _resume?: () => void }
  if (typeof inner._resume === 'function') inner._resume = noop
}

// ---------------------------------------------------------------------------
// the entry point
// ---------------------------------------------------------------------------

// The entry points this helper is currently waiting on. Two overlapping
// starts on one name cannot both catch a publish — whichever instance
// publishes first would be handed to whichever accessor is installed, which
// is a crossed wire, not an error anyone would find — so the second is
// refused, the way jsport.Serve refuses to overwrite an entry point that
// exists.
const awaiting = new Set<string>()

interface EntryPoint {
  // Resolves with the value the instance published.
  published: Promise<unknown>
  // Puts globalThis back the way it was found. Idempotent.
  release(): void
}

function claimEntryPoint(name: string): EntryPoint {
  if (awaiting.has(name)) {
    throw new Error(`wasm: another startWasmServer is already waiting for globalThis.${name}; give one of them its own entryPoint`)
  }
  const g = globalThis as unknown as Record<string, unknown>
  // Whatever was there — normally nothing — is restored at the end rather
  // than overwritten: the page may host something else under this name, and
  // one helper call is not a reason to take it permanently.
  const prev = Object.getOwnPropertyDescriptor(g, name)

  let value: unknown
  let publish!: (v: unknown) => void
  const published = new Promise<unknown>((res) => {
    publish = res
  })
  Object.defineProperty(g, name, {
    configurable: true,
    // Reads answer undefined until the instance publishes, because that is
    // what jsport.Serve checks before publishing: an accessor answering
    // anything else would look exactly like an entry point already taken, and
    // the server would refuse to start.
    get: () => value,
    set: (v: unknown) => {
      value = v
      publish(v)
    },
  })
  awaiting.add(name)

  let released = false
  return {
    published,
    release() {
      if (released) return
      released = true
      awaiting.delete(name)
      if (prev === undefined) Reflect.deleteProperty(g, name)
      else Object.defineProperty(g, name, prev)
    },
  }
}

// awaitPublish resolves with the published entry point, or rejects with the
// reason it will never arrive. All three arms matter: readiness is the only
// success, a settled run() before it is death on the way up, and the timer
// covers the module that neither publishes nor exits.
async function awaitPublish(published: Promise<unknown>, run: Promise<void>, name: string, timeoutMs: number): Promise<ServeFn> {
  let timer: ReturnType<typeof setTimeout> | undefined
  const arms: Promise<unknown>[] = [
    published,
    // A Go panic prints its trace and RESOLVES go.run() (wasm_exec exits with
    // code 2), so both settlements of this promise are failures here — the
    // instance is gone and the entry point will never appear.
    run.then(
      () => {
        throw new Error(`wasm: the instance exited before publishing globalThis.${name} — see the console for what it printed`)
      },
      (e: unknown) => {
        throw new Error(`wasm: the instance failed before publishing globalThis.${name}`, { cause: e })
      },
    ),
  ]
  if (timeoutMs > 0) {
    arms.push(
      new Promise((_, rej) => {
        timer = setTimeout(() => rej(new Error(`wasm: no globalThis.${name} after ${timeoutMs} ms — is that the name the server serves under?`)), timeoutMs)
        // A pending start must not pin a node process the way it would not
        // pin a browser tab.
        unrefTimer(timer)
      }),
    )
  }
  try {
    const serve = await Promise.race(arms)
    if (typeof serve !== 'function') {
      throw new Error(`wasm: globalThis.${name} was set to a ${typeof serve}, not a function taking one port`)
    }
    return serve as ServeFn
  } finally {
    if (timer !== undefined) clearTimeout(timer)
  }
}

// ---------------------------------------------------------------------------
// the channel
// ---------------------------------------------------------------------------

// handOver makes the channel, keeps one end and gives the instance the other.
// The transport is constructed BEFORE the far end is handed over, so nothing
// the server posts on its first tick arrives unlistened.
function handOver(serve: ServeFn, opts: PortOptions): PortTransport {
  const ch = new MessageChannel()
  let tx: PortTransport | undefined
  try {
    tx = new PortTransport(ch.port1, opts)
    serve(ch.port2)
    return tx
  } catch (e) {
    // Nothing is left half-attached: the transport takes its listeners off
    // its end, and both ends are closed — a live MessagePort keeps the event
    // loop (and, in node, the whole process) alive.
    tx?.close(e)
    ch.port1.close()
    ch.port2.close()
    throw e
  }
}

// ---------------------------------------------------------------------------
// instantiation
// ---------------------------------------------------------------------------

function newGo(): GoLike {
  const ctor = (globalThis as unknown as { Go?: new () => GoLike }).Go
  if (typeof ctor !== 'function') {
    throw new Error(
      'wasm: globalThis.Go is undefined — load the toolchain\'s wasm_exec.js (a classic script: <script src="/wasm_exec.js"></script>) before calling, or pass your own instance as opts.go',
    )
  }
  return new ctor()
}

async function instantiate(source: WasmSource, imports: WebAssembly.Imports): Promise<WebAssembly.Instance> {
  if (source instanceof WebAssembly.Module) return WebAssembly.instantiate(source, imports)
  if (typeof source === 'string' || source instanceof URL) return fromResponse(fetch(String(source)), String(source), imports)
  if (source instanceof ArrayBuffer || ArrayBuffer.isView(source)) return (await WebAssembly.instantiate(source, imports)).instance
  return fromResponse(source, undefined, imports)
}

async function fromResponse(src: Response | Promise<Response>, url: string | undefined, imports: WebAssembly.Imports): Promise<WebAssembly.Instance> {
  const res = await src
  if (!res.ok) {
    // The body is the point: a dev server that rebuilds on demand answers a
    // broken build with a 500 whose body is the compiler output, and it is
    // the only thing in the whole failure that names the line that stopped
    // compiling. instantiateStreaming would throw it away.
    const body = await res.text().catch(() => '')
    throw new Error(`wasm: GET ${url ?? (res.url || 'the module')} failed: ${res.status} ${res.statusText}${body === '' ? '' : `\n${body}`}`)
  }
  // Streaming compilation only where the response is typed for it.
  // instantiateStreaming refuses anything but application/wasm, and a static
  // server that never heard of wasm answers application/octet-stream — but a
  // body can only be read once, so recovering from that refusal after the
  // fact would mean buffering a clone of the whole module up front on every
  // load, purely to keep a fallback that the header already decides. Read the
  // header instead, and buffer only when it says to.
  if (typeof WebAssembly.instantiateStreaming === 'function' && mimeOf(res) === 'application/wasm') {
    return (await WebAssembly.instantiateStreaming(res, imports)).instance
  }
  return (await WebAssembly.instantiate(await res.arrayBuffer(), imports)).instance
}

function mimeOf(res: Response): string {
  const ct = res.headers.get('content-type') ?? ''
  return (ct.split(';')[0] ?? '').trim().toLowerCase()
}
