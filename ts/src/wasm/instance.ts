// One Go program compiled to GOOS=js GOARCH=wasm, seen from the realm that
// runs it: start it, catch the moment it can serve, hand it ports, and learn
// when it is gone.
//
// It is the same machinery whichever realm that is — the worker this package
// ships (./worker) or the page itself (`open(app, { worker: false })`) — so it
// lives apart from both, and it imports nothing else in this package. That is
// a requirement, not tidiness: the shipped worker carries this file, and it
// must not carry the drpc core with it. The worker never speaks the wire — the
// wire only ever crosses the ports handed to serve(), and both ends of every
// one of those belong to somebody else.
//
// It exists because the hand-written version of it has four hazards in it, and
// each of them fails in a way that points somewhere else:
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
//     ever arrives. It is what `exited` resolves with, which is the whole
//     reason a live call on such a channel ever fails — and the corpse is
//     defused before it settles (see makeInert), because the goodbye whoever
//     holds the ports will now post is itself an event a dead instance still
//     tries to handle.
//
// One instance serves as many connections as it is given ports: one port is
// one peer (PROTOCOL.md §6.4), with its own epoch, sid space, flow-control
// windows and per-peer resource caps, and a teardown that reaches only it.
//
// What it does not own is the instance's lifetime. Nothing here can stop a Go
// program once go.run has started it — wasm_exec has no kill switch, and
// os.Exit is the instance's own decision — so the failure paths are split by
// that line: everything that fails before go.run leaves nothing running, and
// a failure after it (a module that never publishes) rejects with an instance
// still alive, which only a page reload, a worker terminate or the module's
// own shutdown entry point can end. What it does clean up is what it made: the
// accessor, on every path including the successful one.

// noop and unrefTimer are spelled out here rather than imported from ../util,
// which has both: this file is the whole of what the shipped worker carries,
// and one import of that module would pull the core's chunk in behind it —
// flow control, compression, the frame queue — for two lines.
const noop = (): void => {}

// unrefTimer detaches a node timer from the event-loop lifetime where the
// runtime supports it (a pending start must not pin a node process the way it
// would not pin a browser tab); a no-op in browsers.
function unrefTimer(t: unknown): void {
  ;(t as { unref?: () => void }).unref?.()
}

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

// DefaultWasmExec is where the JS half of the Go runtime is looked for when
// this realm has no globalThis.Go yet — the path a page serves it from.
export const DefaultWasmExec = '/wasm_exec.js'

// GoLike is the part of wasm_exec.js's Go class this file touches. It is
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

// WasmApp is the compiled program, in every form that survives a trip to a
// worker: a URL to fetch (as a string or a URL), the module's bytes, or a
// Module compiled once and instantiated many times. A Response is deliberately
// not one — it cannot be structured-cloned, so it could never reach the realm
// that runs the instance; pass its URL, and the fetch (and the diagnosis of a
// broken build below) happens there.
export type WasmApp = string | URL | BufferSource | WebAssembly.Module

export interface InstanceOptions {
  // The global the instance publishes its port-taking function as; must match
  // the name the Go side serves under. Default DefaultEntryPoint.
  entryPoint?: string
  // A Go instance built by the caller — the way to pass argv or env, since
  // nothing here sets either. Default `new globalThis.Go()`, loading
  // wasm_exec.js first where this realm has none.
  go?: GoLike
  // How long to wait for the publish; <= 0 waits forever. Default
  // DefaultReadyTimeoutMs.
  readyTimeoutMs?: number
  // Where to fetch wasm_exec.js from when this realm has no globalThis.Go.
  // Default DefaultWasmExec.
  wasmExec?: string | URL
}

// The function the instance publishes: it takes one port and serves one peer
// on it (PROTOCOL.md §6.4). jsport.Gateway.Serve binds the port before
// returning, so a call opened on this very tick loses nothing.
type ServeFn = (port: MessagePort) => void

// Instance is one running Go program, seen from the realm that started it.
export interface Instance {
  // The global it published under; the name it is known by here.
  readonly entryPoint: string
  // Hands the instance one port to serve as its own peer (§6.4). Throws once
  // the instance has exited: a connection to a corpse would hang forever
  // rather than fail (§10.6 — no timers), so it has to be refused out loud.
  serve(port: MessagePort): void
  // Resolves — never rejects — with the cause when the instance is gone. This
  // is the §4.5 evidence nothing else can produce, and it settles only after
  // the corpse has been defused (makeInert), so whoever holds the ports may
  // post the goodbye the moment it fires.
  readonly exited: Promise<unknown>
}

// startInstance instantiates a Go wasm module and waits for it to publish its
// entry point. Rejects, without leaving a global behind, if the module cannot
// be fetched or instantiated, if the instance dies before publishing, or if it
// has not published after readyTimeoutMs.
export async function startInstance(app: WasmApp, opts: InstanceOptions = {}): Promise<Instance> {
  const name = opts.entryPoint ?? DefaultEntryPoint

  // Claimed before anything is fetched or run: the accessor has to be in
  // place before go.run for the publish to be catchable at all, and claiming
  // it first also means a second start on the same name is refused while this
  // one is still in the air rather than quietly stealing its publish.
  const entry = claimEntryPoint(name)
  try {
    const go = opts.go ?? (await newGo(String(opts.wasmExec ?? DefaultWasmExec)))
    const instance = await instantiate(app, go.importObject)
    const run = go.run(instance)
    const serve = await awaitPublish(entry.published, run, name, opts.readyTimeoutMs ?? DefaultReadyTimeoutMs)
    return live(name, go, serve, run)
  } finally {
    // On every path: the accessor was ours, it was never meant to survive the
    // start, and a second startInstance must find the name as it was.
    entry.release()
  }
}

// live wraps the started instance with the one thing the port it published
// cannot express — that it is gone. Every consumer of `exited` runs after
// makeInert, which is why the goodbye posted to the corpse is safe.
function live(name: string, go: GoLike, serve: ServeFn, run: Promise<void>): Instance {
  let dead: { cause: unknown } | undefined
  let announce!: (cause: unknown) => void
  const exited = new Promise<unknown>((res) => {
    announce = res
  })
  const died = (cause: unknown): void => {
    if (dead !== undefined) return
    dead = { cause }
    makeInert(go)
    announce(cause)
  }
  // A Go panic RESOLVES run() (wasm_exec exits with code 2), so both
  // settlements are death; only the wording differs. Each cause is a sentence
  // that names the instance, because it is read far from here — spelled into a
  // failed call's status by the adapter (see causeDetail in
  // ../transport/port), or carried to another realm as text (./protocol) where
  // "unreachable executed" alone would say nothing about what died. The trap
  // itself stays attached as the cause.
  void run.then(
    () => died(new Error('the wasm instance exited')),
    (e: unknown) => died(new Error(`the wasm instance failed: ${e instanceof Error ? e.message : String(e)}`, { cause: e })),
  )

  return {
    entryPoint: name,
    exited,
    serve(port: MessagePort): void {
      if (dead !== undefined) {
        throw new Error(`wasm: the instance behind globalThis.${name} has exited${dead.cause instanceof Error ? `: ${dead.cause.message}` : ''}`)
      }
      serve(port) // one port is one peer (§6.4)
    },
  }
}

// makeInert defuses the corpse before the goodbye is posted to it. An exited
// instance leaves its js.Funcs registered on the ports it was handed — os.Exit
// runs nothing, so nothing detaches them — and wasm_exec re-enters the dead
// runtime for every event that still arrives, throwing "Go program has already
// exited" out of an event handler. The two events a dying endpoint produces
// are exactly the two this teardown causes: the goodbye, and the channel
// closing under it. A page logs that and carries on; node treats an exception
// from an event handler as fatal and kills the process. Nothing can
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

// The entry points this file is currently waiting on. Two overlapping starts
// on one name cannot both catch a publish — whichever instance publishes first
// would be handed to whichever accessor is installed, which is a crossed wire,
// not an error anyone would find — so the second is refused, the way
// jsport.Serve refuses to overwrite an entry point that exists.
const awaiting = new Set<string>()

interface EntryPoint {
  // Resolves with the value the instance published.
  published: Promise<unknown>
  // Puts globalThis back the way it was found. Idempotent.
  release(): void
}

function claimEntryPoint(name: string): EntryPoint {
  if (awaiting.has(name)) {
    throw new Error(`wasm: another instance is already waiting for globalThis.${name}; give one of them its own entryPoint`)
  }
  const g = globalThis as unknown as Record<string, unknown>
  // Whatever was there — normally nothing — is restored at the end rather
  // than overwritten: the page may host something else under this name, and
  // one start is not a reason to take it permanently.
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
// the Go runtime
// ---------------------------------------------------------------------------

// newGo builds the instance the module will run in. globalThis.Go comes from
// the toolchain's wasm_exec.js, which this package does NOT vendor: it is
// version-coupled to the compiler that produced the module, so a copy here
// would pin the wrong one. Where the realm already has it — a page that loaded
// it as a classic <script>, a test that evaluated it — nothing is fetched.
async function newGo(wasmExec: string): Promise<GoLike> {
  let ctor = goCtor()
  if (ctor === undefined) {
    await loadWasmExec(wasmExec)
    ctor = goCtor()
    if (ctor === undefined) {
      throw new Error(`wasm: ${wasmExec} loaded but defined no globalThis.Go — is that the toolchain's wasm_exec.js?`)
    }
  }
  return new ctor()
}

function goCtor(): (new () => GoLike) | undefined {
  const ctor = (globalThis as unknown as { Go?: new () => GoLike }).Go
  return typeof ctor === 'function' ? ctor : undefined
}

// loadWasmExec fetches and evaluates the JS half of the Go runtime. It is a
// classic script and not a module — it assigns globalThis.Go from inside an
// IIFE — so it is evaluated rather than imported, which in a module worker
// (no importScripts) is the only way in at all. The one failure that matters
// is that it is not being served, and the error says exactly how to fix that:
// nothing else in the whole stack knows the file's name, let alone where it
// comes from.
async function loadWasmExec(url: string): Promise<void> {
  let res: Response
  try {
    res = await fetch(url)
  } catch (e) {
    throw new Error(missingWasmExec(url, String(e)), { cause: e })
  }
  if (!res.ok) throw new Error(missingWasmExec(url, `${res.status} ${res.statusText}`))
  new Function(await res.text())()
}

function missingWasmExec(url: string, why: string): string {
  return (
    `wasm: GET ${url} failed (${why}) — wasm_exec.js is the JS half of the Go runtime, and this package does not ship it: ` +
    "it is version-coupled to the compiler that built your module, so a vendored copy would pin the wrong one. Serve your toolchain's:\n" +
    '  cp "$(go env GOROOT)/lib/wasm/wasm_exec.js" ./public/\n' +
    'or load it yourself before starting — a realm that already has globalThis.Go fetches nothing.'
  )
}

// ---------------------------------------------------------------------------
// instantiation
// ---------------------------------------------------------------------------

async function instantiate(app: WasmApp, imports: WebAssembly.Imports): Promise<WebAssembly.Instance> {
  if (app instanceof WebAssembly.Module) return WebAssembly.instantiate(app, imports)
  if (typeof app === 'string' || app instanceof URL) return fromResponse(fetch(String(app)), String(app), imports)
  return (await WebAssembly.instantiate(app, imports)).instance
}

async function fromResponse(src: Promise<Response>, url: string, imports: WebAssembly.Imports): Promise<WebAssembly.Instance> {
  const res = await src
  if (!res.ok) {
    // The body is the point: a dev server that rebuilds on demand answers a
    // broken build with a 500 whose body is the compiler output, and it is
    // the only thing in the whole failure that names the line that stopped
    // compiling. instantiateStreaming would throw it away.
    const body = await res.text().catch(() => '')
    throw new Error(`wasm: GET ${url} failed: ${res.status} ${res.statusText}${body === '' ? '' : `\n${body}`}`)
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
