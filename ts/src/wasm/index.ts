// The front door for a Go server compiled to GOOS=js GOARCH=wasm: two lines,
// and the page has a drpc connection to it.
//
//   import { open } from '@lesomnus/grpc-dgram/wasm'
//
//   const sock = await open('/app.wasm')
//   const conn = sock.dial()            // ready to call
//
// The Go half is already that short — jsport.NewGateway, drpc.NewServer,
// register, gw.Serve — so this is the JS half of that pair and nothing else.
// The package ships the worker the instance runs in (./worker), so there is no
// worker script to write, no channel to make, no readiness to invent, and no
// teardown to remember; ./protocol is what the two halves say to each other.
//
// Why a worker by default: Go's scheduler shares whatever thread it runs on,
// so a handler that computes for 50 ms freezes the page for 50 ms if that
// thread is the main one. `{ worker: false }` runs the instance in this realm
// instead — node, tests, a page that does not want one — and everything else
// about the API is identical.
//
// The one thing that cannot be shipped is wasm_exec.js, the JS half of the Go
// runtime: it is version-coupled to the compiler that built the module, so a
// vendored copy would pin the wrong one. Serve your toolchain's, and say where
// with `wasmExec` if it is not at /wasm_exec.js:
//
//   cp "$(go env GOROOT)/lib/wasm/wasm_exec.js" ./public/
//
// What is left for the application is the part only it can decide: when to
// dial another connection, and when to close. One port is one peer (PROTOCOL.md
// §6.4), so a second dial() is a second independent peer of the same server —
// its own epoch, sid space, flow-control windows and per-peer caps, and a
// teardown that reaches only it.
//
// One instance may also serve more than one SERVER, which is a different axis
// entirely: a second drpc.Server, its own registry and its own interceptors,
// published under a name of its own (jsport.WithEntryPoint). The page reaches
// it by name, and everything else — the module, the runtime, the memory, the
// lifetime — is shared:
//
//   const sock = await open('/app.wasm')
//   const control = sock.dial()                          // drpcServe
//   const admin = sock.dial({ entryPoint: 'drpcAdmin' })  // the second gateway
//
// Only the FIRST of those names is what readiness means: open() resolves when
// the started entry point publishes, and a program that yields to the event
// loop between its two gateways can be reached before the second has run. So a
// dial to a name that is not there yet WAITS for it (bounded by
// readyTimeoutMs) instead of failing — which is what keeps the order the Go
// program happens to publish in from deciding whether the page works.

import { Conn, type ConnOptions } from '../conn'
import { PortTransport, type PortOptions } from '../transport/port'
import { DefaultEntryPoint, DefaultReadyTimeoutMs, DefaultWasmExec, startInstance, type GoLike, type Instance, type WasmApp } from './instance'
import { isWorkerMessage, type ServeMessage, type StartMessage } from './protocol'

export { DefaultEntryPoint, DefaultReadyTimeoutMs, DefaultWasmExec, type GoLike, type WasmApp } from './instance'

// WasmWorker is the part of a Worker open() touches. Structural, so a test
// double fits and so does a worker-like object from a runtime whose Worker is
// not the DOM's; a real Worker is assignable to it, `unknown` in the listener
// position being what makes that work (see PortLike in ../transport/port).
export interface WasmWorker {
  postMessage(message: unknown, transfer?: unknown[]): void
  addEventListener(type: string, fn: (ev: unknown) => void): void
  removeEventListener(type: string, fn: (ev: unknown) => void): void
  terminate(): void
}

export interface OpenOptions extends PortOptions {
  // false runs the instance in THIS realm instead of a worker — node, tests, a
  // page that does not want one. Or pass a Worker you made yourself, which is
  // then yours to terminate: only a worker open() made is one it ends. It has
  // to be a worker that runs ./worker (`import '@lesomnus/grpc-dgram/wasm/
  // worker'`, or serveIn(self)) — one that answers nothing leaves open()
  // waiting with nothing to end the wait, since readyTimeoutMs is a clock the
  // realm running the instance keeps. For a worker that is not a wasm instance
  // at all — one already running, with a server of its own — the seam is
  // dialWorker (../transport/port), not this: there is nothing to open.
  worker?: boolean | WasmWorker
  // Where the shipped worker module lives. It is resolved from this module's
  // own URL by default, which is right for a plain <script type="module"> and
  // for an import map; a bundler that rewrites workers will not follow a
  // computed URL, so hand it the one it produced:
  //
  //   import workerUrl from '@lesomnus/grpc-dgram/wasm/worker?worker&url'
  workerUrl?: string | URL
  // Where to fetch wasm_exec.js from, in whichever realm runs the instance.
  // Default DefaultWasmExec ('/wasm_exec.js'). Nothing is fetched where the
  // realm already has globalThis.Go — a page that loaded it as a classic
  // <script> has nothing to configure here.
  wasmExec?: string | URL
  // The global the instance publishes its port-taking function as; must match
  // the name the Go side serves under (jsport.WithEntryPoint). Default
  // DefaultEntryPoint.
  entryPoint?: string
  // How long to wait for that publish, measured from the moment the module is
  // instantiated; <= 0 waits forever. Default DefaultReadyTimeoutMs.
  readyTimeoutMs?: number
  // A Go instance built by the caller — the way to pass argv or env, since
  // nothing here sets either. It belongs to the realm that made it, so it goes
  // with `{ worker: false }`; open() refuses it otherwise rather than ignore
  // it.
  go?: GoLike
}

// DialOptions is a Conn's options plus the one thing that is this sock's
// business: which of the instance's servers to reach.
export interface DialOptions extends ConnOptions {
  // The entry point to dial, for a program that publishes more than one
  // (jsport.WithEntryPoint on the Go side). Omitted — the usual case — this
  // is the name open() started on, the one readiness was measured by.
  //
  // A name the instance has not published YET is waited for, not refused:
  // readiness means the FIRST entry point, and a program that yields to the
  // event loop between its two gateways (a fetch, a database opening) can
  // reach this line before the second one has run. The port queues the calls
  // opened on it meanwhile, so this stays synchronous and nothing is lost. A
  // name that never arrives ends the connection with a cause after
  // readyTimeoutMs — the same clock the start used — rather than hanging it.
  entryPoint?: string
}

// WasmSock is one running instance, seen from the page.
export interface WasmSock {
  // The worker, when there is one. Yours to look at; close() is what ends it,
  // and only if open() made it.
  readonly worker?: WasmWorker
  // Resolves — never rejects — with the cause when the instance is gone: the
  // §4.5 evidence no port can produce. close() resolves it too, since after
  // that this sock has stopped watching and a promise that could only hang
  // would be worse than one that answers.
  //
  // A dead instance does not take its worker with it — a worker outlives the
  // program that ran in it, and nothing here terminates one behind the
  // application's back — so this is what a page that wants the thread back
  // calls close() on.
  readonly exited: Promise<unknown>
  // dial opens one connection: a fresh channel, one end to the instance as its
  // own peer (§6.4), a Conn over the other. Call it again for another,
  // independent connection — or for another of the instance's servers, with
  // `entryPoint`.
  //
  // dial, because by now the peer exists — open() is what brought it into
  // existence, and this is the verb for reaching something that already does.
  // It hands back a Conn for the same reason every dial… in this library does:
  // a Conn is the endpoint you make calls on, and the plumbing under it is not
  // this caller's business (the transport path is ../transport/port).
  //
  // Synchronous on purpose, `entryPoint` included. A transferred MessagePort
  // queues everything posted into it until the far side binds it, so nothing
  // is lost and there is nothing to wait for — a call opened on this very tick
  // is delivered late, not dropped. Throws once the instance has exited or the
  // sock is closed: a connection to a corpse would hang forever rather than
  // fail (§10.6 — no timers), so it is refused out loud.
  //
  // What cannot be refused here is a dial to an entry point that turns out
  // never to appear: only the realm running the instance can see that, and it
  // sees it later. That connection fails with the cause instead, exactly as
  // one to an instance that dies does.
  dial(opts?: DialOptions): Conn
  // close ends every Conn dialled here — their live calls fail with a cause
  // rather than hang — and then terminates the worker, if open() made one.
  // Idempotent. It cannot stop the Go program itself when there is no worker:
  // wasm_exec has no kill switch, so an in-realm instance keeps running until
  // the page is reloaded or it exits on its own.
  close(): void
}

// open starts the instance and returns once it can serve.
//
//   const sock = await open('/app.wasm')
//   const conn = sock.dial()
//
// open, not dial, because at this point there is no peer: a .wasm URL is a
// program, not an endpoint, and nothing exists to reach until this has fetched
// it, instantiated it and waited for it to say it can serve. open is the verb
// for bringing something into existence; dial is the verb for reaching
// something that exists, which is why what comes back is a Sock — not a Conn —
// and the connection is the sock.dial() after it. It is a Sock and not a
// Server for the other half of the same reason: this side is the client, and
// the thing it names is what it dials into.
//
// It rejects, having left nothing behind, if the module cannot be fetched or
// instantiated, if the instance dies on the way up, or if it has not published
// its entry point after readyTimeoutMs — with the reason, because a page that
// merely hangs blames the wrong half. A worker open() made is terminated on
// that path, which is also what ends the instance it had already started; one
// you passed is left alone, instance and all, because it is not ours to kill.
//
// The one wait nothing here bounds is a worker that answers nothing at all —
// readyTimeoutMs is counted by the realm running the instance, so a worker
// that never got that far cannot count it. A worker open() made and failed to
// load fires `error` instead, which is why that is wired; a worker of your own
// that does not run ./worker has neither, and open() waits on it forever.
export async function open(app: WasmApp, opts: OpenOptions = {}): Promise<WasmSock> {
  if (opts.worker === false) {
    const inst = await startInstance(app, opts)
    return new HereSock(inst, opts)
  }
  if (opts.go !== undefined) {
    throw new Error('wasm: opts.go builds the Go instance in this realm, and it cannot be posted to a worker — pass { worker: false } with it, or drop it')
  }
  const given = typeof opts.worker === 'object' && opts.worker !== null ? opts.worker : undefined
  const sock = new WorkerSock(given ?? spawn(opts.workerUrl), given === undefined, opts)
  try {
    await sock.start(app, opts)
  } catch (e) {
    // Whatever went wrong, the worker is not going to serve: end the one we
    // made (nothing else can — it holds an instance that may be running) and
    // release the listeners on one we did not.
    sock.close()
    throw e
  }
  return sock
}

// spawn loads the worker this package ships. The URL is computed from this
// module's own, which is where the worker lands in the published package —
// dist/wasm.mjs beside dist/wasm/worker.mjs — so a plain <script
// type="module"> or an import map needs no path of its own, and nothing has to
// be copied anywhere.
//
// A bundler is the case this cannot serve by itself: it rewrites workers by
// pattern-matching a literal `new Worker(new URL('./x', import.meta.url))`,
// which this is not (the constructor is looked up, because a realm without one
// must be told so rather than throw "Worker is not defined"). Hand it the URL
// it produced instead — that is what workerUrl is for.
function spawn(url: string | URL | undefined): WasmWorker {
  const ctor = (globalThis as unknown as { Worker?: new (url: string | URL, opts?: { type?: string }) => WasmWorker }).Worker
  if (typeof ctor !== 'function') {
    throw new Error('wasm: this realm has no Worker — pass { worker: false } to run the instance here (node, tests), or a worker you made yourself')
  }
  return new ctor(url ?? new URL('./wasm/worker.mjs', import.meta.url), { type: 'module' })
}

// Sock is what the two modes share: the connections dialled through it, the
// one death that fails all of them at once (§4.5), and an idempotent close.
// All that differs is where a connection goes and what, if anything, close()
// ends.
abstract class Sock implements WasmSock {
  abstract readonly worker?: WasmWorker
  readonly exited: Promise<unknown>

  protected readonly txs = new Set<PortTransport>()
  protected dead: { cause: unknown } | undefined
  private closed = false
  private announce!: (cause: unknown) => void

  constructor(protected readonly o: PortOptions) {
    this.exited = new Promise<unknown>((res) => {
      this.announce = res
    })
  }

  dial(opts: DialOptions = {}): Conn {
    if (this.dead !== undefined) {
      const what = this.closed ? 'this sock is closed' : 'the wasm instance has exited'
      throw new Error(`wasm: ${what}${this.dead.cause instanceof Error ? `: ${this.dead.cause.message}` : ''}`)
    }
    // Split rather than passed whole: entryPoint says which server this
    // channel goes to, which is settled before a Conn exists and is no part of
    // what a Conn is configured with.
    const { entryPoint, ...connOpts } = opts
    const tx = this.connect(entryPoint)
    this.txs.add(tx)
    return new Conn(tx, connOpts)
  }

  close(): void {
    if (this.closed) return
    this.closed = true
    // The local teardown FIRST, and only then whatever ends the instance: a
    // Conn closed here fails its live calls with a cause, where a worker
    // terminated out from under them would leave them hanging forever (§10.6 —
    // nothing times a call out). The goodbye each close() posts may not
    // survive the terminate that follows, which costs nothing: the peer it
    // would have reached is about to stop existing.
    this.bury(new Error('the wasm sock was closed'))
    this.stop()
  }

  // connect opens one more connection to the instance, through the entry point
  // named — the one it was started on by default.
  protected abstract connect(entryPoint: string | undefined): PortTransport
  // stop ends the instance if this sock is what owns it.
  protected abstract stop(): void

  // channelTo is what every connection here is made of, and all the two modes
  // differ by is `handover` — the one line that gives the far end to whatever
  // runs the instance. A MessageChannel is ONE channel with two entangled,
  // symmetric ends: what is posted into one arrives at the other, and the only
  // thing telling them apart is which one you give away. Making it here is why
  // port1 and port2 never appear in application code.
  //
  // It is the same shape dialWorker (../transport/port) has, kept here rather
  // than borrowed from there because what this sock has to hold is the
  // transport: a dying instance fails every connection at once (bury), and the
  // transport is what carries the cause that says why.
  protected channelTo(handover: (port: MessagePort) => void | Promise<void>): PortTransport {
    const ch = new MessageChannel()
    let tx: PortTransport | undefined
    try {
      // Constructed BEFORE the far end is handed over, so nothing the instance
      // posts on its first tick arrives unlistened.
      tx = new PortTransport(ch.port1, this.o)
      const handed = handover(ch.port2)
      // A handover that is not settled yet — an entry point this instance has
      // not published — reports its failure later than this call can throw.
      // THIS connection ends then, and only this one: the instance is alive
      // and every other connection to it is unaffected. Nothing else ever
      // would end it, since the channel runs with no timers (§10.6).
      if (handed !== undefined) {
        const t = tx
        void handed.catch((e: unknown) => {
          this.txs.delete(t)
          t.close(e)
        })
      }
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

  // bury fails every connection at once and announces the cause. Every
  // connection, not just the first: they share the process that stopped
  // existing.
  protected bury(cause: unknown): void {
    if (this.dead !== undefined) return
    this.dead = { cause }
    for (const tx of this.txs) tx.close(cause)
    this.txs.clear()
    this.announce(cause)
  }
}

// ---------------------------------------------------------------------------
// the instance in a worker
// ---------------------------------------------------------------------------

class WorkerSock extends Sock {
  private readonly readiness: Promise<void>
  private ready!: () => void
  private failed!: (e: unknown) => void
  private readonly detach: (() => void)[] = []
  // The instance said it can serve. It divides the two things a Worker's
  // `error` event can mean; see the listener below.
  private serving = false

  constructor(
    readonly worker: WasmWorker,
    private readonly own: boolean,
    o: PortOptions,
  ) {
    super(o)
    this.readiness = new Promise<void>((res, rej) => {
      this.ready = res
      this.failed = rej
    })
    // Registered before anything is posted, for the mirror of the rule
    // ./worker follows: a Worker's implicit port is started at construction,
    // so a message that arrives before a listener exists is dropped rather
    // than queued — and the answer to `start` is the only thing that ever
    // arrives here.
    this.listen('message', (ev) => this.onMessage(ev))
    // A worker that could not be loaded at all — a wrong workerUrl, a module
    // that threw on evaluation — fires this and nothing else. Without it a bad
    // path would look exactly like a module that is slow to publish, and only
    // readyTimeoutMs (which the worker cannot count, because it never ran)
    // would ever end the wait.
    //
    // Until `ready`, and no longer. A worker SURVIVES an uncaught exception,
    // and one raised after the instance is serving cannot be ours — every path
    // in ./worker catches its own — so it belongs to whatever else of the
    // application's is sharing this worker, and somebody else's bug may not
    // tear these connections down any more than somebody else's message may
    // (§4.2). The instance's own death arrives as `exited`, with a goodbye on
    // every port; that is the signal, and this is only ever the start's.
    this.listen('error', (ev) => {
      if (this.serving) return
      const detail = (ev as { message?: unknown }).message
      this.die(new Error(`wasm: the worker itself failed${typeof detail === 'string' && detail !== '' ? `: ${detail}` : ''} — is workerUrl the module this package ships?`))
    })
    // A message this realm could not deserialize is dropped like any other
    // input the adapter cannot read (§4.2); it is never a teardown.
    this.listen('messageerror', () => {})
  }

  // start posts the one message that runs the instance and resolves when the
  // worker says it can serve. Every field is resolved here rather than
  // defaulted again on the far side, so the two halves cannot disagree about
  // what a default is across a version skew.
  start(app: WasmApp, opts: OpenOptions): Promise<void> {
    const start: StartMessage = {
      drpc: 'start',
      app,
      wasmExec: String(opts.wasmExec ?? DefaultWasmExec),
      entryPoint: opts.entryPoint ?? DefaultEntryPoint,
      readyTimeoutMs: opts.readyTimeoutMs ?? DefaultReadyTimeoutMs,
    }
    this.worker.postMessage(start)
    return this.readiness
  }

  protected connect(entryPoint: string | undefined): PortTransport {
    // The port is transferred, never the worker itself: a MessagePort queues
    // what is posted into it until the far side binds it (see ./worker, rule
    // 1), and it is what makes a second, independent peer possible at all.
    //
    // The field is omitted rather than filled in with the started name: which
    // name that is belongs to the realm holding the instance, and a worker
    // reading the two from one message could disagree with the page about it.
    const serve: ServeMessage = entryPoint === undefined ? { drpc: 'serve' } : { drpc: 'serve', entryPoint }
    // Nothing to await here even when the entry point is one the instance has
    // yet to publish: the worker holds the port until it can serve it, and
    // ends it with a goodbye if it never can — which reaches this connection
    // as a teardown through the transport, like any other death (§4.5).
    return this.channelTo((port) => this.worker.postMessage(serve, [port]))
  }

  protected stop(): void {
    // Only a worker open() made. One the application passed may be hosting
    // something else of its own, and terminate() would take that with it.
    if (this.own) this.worker.terminate()
    for (const off of this.detach.splice(0)) off()
  }

  private listen(type: string, fn: (ev: unknown) => void): void {
    this.worker.addEventListener(type, fn)
    this.detach.push(() => this.worker.removeEventListener(type, fn))
  }

  private onMessage(ev: unknown): void {
    const msg = (ev as { data?: unknown }).data
    if (!isWorkerMessage(msg)) return // somebody else's traffic on this worker
    switch (msg.drpc) {
      case 'ready':
        this.serving = true
        this.ready()
        break
      case 'error':
        // A start that will never succeed. Nothing is running on the far side,
        // so there is nothing to bury — open() rejects and ends the worker.
        this.failed(new Error(`wasm: ${msg.message}`))
        break
      case 'exited':
        this.die(new Error(msg.message))
        break
    }
  }

  // die is the §4.5 report: the instance is gone, so every connection to it
  // fails with the cause. Before readiness it is also the answer to start —
  // rejecting a promise that has already settled is a no-op, so both cases go
  // through here.
  private die(cause: unknown): void {
    this.failed(cause)
    this.bury(cause)
    for (const off of this.detach.splice(0)) off()
  }
}

// ---------------------------------------------------------------------------
// the instance in this realm
// ---------------------------------------------------------------------------

class HereSock extends Sock {
  readonly worker = undefined

  constructor(
    private readonly inst: Instance,
    o: PortOptions,
  ) {
    super(o)
    // go.run()'s promise is the only evidence a page in this position ever
    // gets that the server died — there is no socket to close and an exited
    // instance posts no goodbye — and it settles for a Go panic exactly as it
    // does for a clean exit. Wiring it to the connections is what makes a live
    // call fail instead of hang (§10.6).
    void inst.exited.then((cause) => this.bury(cause))
  }

  protected connect(entryPoint: string | undefined): PortTransport {
    // No transfer list: the instance runs in this realm, so handing it the
    // other end is a plain call — which is also what makes it the one place
    // that throws when the instance has already exited. An entry point it has
    // not published yet is the one part that cannot answer synchronously, and
    // channelTo ends this connection with the reason when it does.
    return this.channelTo((port) => this.inst.serve(port, entryPoint))
  }

  protected stop(): void {
    // Nothing to stop: no host can end a Go program once go.run has started it
    // (wasm_exec has no kill switch, and os.Exit is the instance's own
    // decision). The connections are already closed, which is all this side
    // owns.
  }
}
