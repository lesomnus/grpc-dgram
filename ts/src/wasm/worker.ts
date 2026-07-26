// The worker this package ships, so that a page does not have to write one.
// It runs the Go instance off the main thread and does nothing else: it starts
// it, answers `ready`, hands every port it is given to the entry point, and
// says the last word for an instance that cannot say it itself.
//
// open() (./index) is what loads it — `new Worker(new URL('./wasm/worker.mjs',
// import.meta.url), { type: 'module' })`, or the URL a bundler rewrote — and
// ./protocol is what they say to each other. It carries none of the drpc core:
// the wire only ever crosses the ports it forwards, and both ends of every one
// of those belong to somebody else (the page's transport, and the Go server).
//
// Why a worker at all, when a wasm instance in the page would work: Go's
// scheduler shares whatever thread it runs on, so a handler that computes for
// 50 ms freezes the page for 50 ms if that thread is the main one. Off it, the
// UI stays live and the only thing crossing the boundary is bytes.
//
// Two ordering rules make it correct, and both are the reason this is a shipped
// file rather than a snippet in a README:
//
//   1. A `serve` may arrive before the instance exists. What is transferred is
//      a MessagePort, and a MessagePort QUEUES everything posted into it until
//      its owner starts it — which the Go adapter does when it binds — so a
//      call opened on the tick open() resolved is delivered late, not dropped.
//      (This is also why open() transfers a port instead of handing the Worker
//      object to a PortTransport: a worker's global scope is wired through
//      onmessage and drops what arrives before a handler is registered, which
//      is what jsport.Bind's doc says too.)
//   2. The `message` listener is registered SYNCHRONOUSLY, at module
//      evaluation, before any await. A module worker's top-level await yields
//      to the event loop, and a `start` or `serve` dispatched during that yield
//      would be lost with nothing to report it — the page would wait out
//      readyTimeoutMs and blame the module.
//
// serveIn is exported and the entry is one guarded call to it, because a
// shipped file nothing can test is how this breaks: node cannot run a DOM
// module worker, so the tests drive serveIn with a scope of their own.

import { startInstance, type Instance } from './instance'
import { isPageMessage } from './protocol'

// WorkerScope is the dedicated worker global scope, structurally: the two
// things serveIn touches. `unknown` in the listener position is what makes a
// real DedicatedWorkerGlobalScope assignable (see PortLike in
// ../transport/port for the same choice).
export interface WorkerScope {
  addEventListener(type: string, fn: (ev: unknown) => void): void
  postMessage(message: unknown): void
}

// The goodbye: a zero-frame envelop, which is exactly 0 bytes (PROTOCOL.md
// §4.1 carries 1..n frames, so the empty message is free to mean "this
// endpoint is going away"). Spelled out here rather than imported from
// ../transport/port so the shipped worker carries none of the core — it never
// speaks the wire, it only says this one word for an instance that no longer
// can.
const GOODBYE = new Uint8Array(0)

// serveIn wires one worker scope to one Go instance. It returns immediately;
// everything after that is driven by messages.
export function serveIn(scope: WorkerScope): void {
  // The ports handed to this instance, so the §4.5 goodbye can reach every one
  // of them when it dies. A port is dropped from the set only by that
  // teardown: this side has no way to learn that a peer went away — Go serves
  // the port and never says so — and posting a goodbye into a port whose peer
  // has already left is a no-op, not an error.
  const ports = new Set<MessagePort>()
  let instance: Promise<Instance> | undefined
  let gone = false

  // bury says the last word on every port at once. The goodbye goes out before
  // `exited` does, so a page that is watching neither the ports nor the
  // messages still tears down: with every protocol timer off (§10.6) an
  // unnotified peer waits forever.
  const bury = (): void => {
    gone = true
    for (const port of ports) farewell(port)
    ports.clear()
  }

  scope.addEventListener('message', (ev) => {
    const msg = (ev as { data?: unknown }).data
    if (!isPageMessage(msg)) return // not ours; somebody else's channel traffic
    if (msg.drpc === 'start') {
      // One worker is one instance: a second start has no entry point left to
      // publish under and no second lifetime to report, so it is ignored the
      // way the first would have refused it.
      if (instance !== undefined) return
      instance = startInstance(msg.app, {
        entryPoint: msg.entryPoint,
        readyTimeoutMs: msg.readyTimeoutMs,
        wasmExec: msg.wasmExec,
      })
      void instance.then(
        (inst) => {
          scope.postMessage({ drpc: 'ready' })
          void inst.exited.then((cause) => {
            // The instance is already inert by the time this runs (see
            // makeInert), so the goodbye posted into its ports cannot re-enter
            // the dead runtime.
            bury()
            scope.postMessage({ drpc: 'exited', message: reason(cause) })
          })
        },
        (e: unknown) => {
          // Nothing is running, so there is nothing to exit — but a port that
          // was already transferred has a live transport on the other end, and
          // only the goodbye ends it.
          bury()
          scope.postMessage({ drpc: 'error', message: reason(e) })
        },
      )
      return
    }
    // 'serve': the port is not in the message — a MessagePort cannot be cloned
    // — so it arrives on the event's transfer list.
    const port = (ev as { ports?: readonly MessagePort[] }).ports?.[0]
    if (port === undefined) return
    if (gone || instance === undefined) {
      // A dead instance, or a serve that overtook its start: say goodbye at
      // once rather than hold a port nothing will ever read. The peer's call
      // then fails instead of hanging, which on a channel with no timers is
      // the whole difference.
      farewell(port)
      return
    }
    ports.add(port)
    // Handed over when the instance is ready, which may be later than now —
    // the port queues what the page posts into it in the meantime (rule 1), so
    // a call opened on this very tick is delivered, not lost.
    void instance.then(
      (inst) => {
        try {
          inst.serve(port)
        } catch {
          // The instance died between the transfer and here, and serve()
          // refuses a corpse rather than hand it a port nobody would ever
          // read. bury() may not have had this one yet, so end it the same
          // way — and never let the throw out, where an unhandled rejection
          // would take a worker down over one connection.
          ports.delete(port)
          farewell(port)
        }
      },
      () => {
        // The start failure already went out as {drpc:'error'}, and bury()
        // already said goodbye on this port.
      },
    )
  })
}

// farewell posts the goodbye and releases the port. Best effort on both: a
// peer that has already gone refuses the message, and an already-closed port
// may throw — neither is anything this side can act on.
function farewell(port: MessagePort): void {
  try {
    port.postMessage(GOODBYE)
  } catch {
    // the peer is already gone; nothing to report and nobody to report it to
  }
  try {
    port.close()
  } catch {
    // an already-closed port may throw
  }
}

// reason spells a cause into the one sentence the page will see, since an
// Error itself survives structured clone as little more than its message.
// Every cause from ./instance already names the instance; anything else is
// carried as it stands.
function reason(cause: unknown): string {
  if (cause instanceof Error && cause.message !== '') return cause.message
  return String(cause)
}

// The entry. Guarded on the scope really being a dedicated worker's, because
// this module is also imported by its own tests and by nothing else that
// should get a listener: a worker global is its own `self`, has a
// postMessage, and has no document. In node `self` does not exist at all.
const g = globalThis as { self?: unknown; document?: unknown; postMessage?: unknown }
if (g.self === g && g.document === undefined && typeof g.postMessage === 'function') {
  // `self` is typed as a Window when compiling against the DOM lib (the
  // WebWorker lib cannot be added beside it), so the cast is this module's
  // declaration that it only ever runs as a dedicated worker.
  serveIn(g as unknown as WorkerScope)
}
