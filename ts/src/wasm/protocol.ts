// What open() (./index) and the worker this package ships (./worker) say to
// each other. It is internal — no application ever posts one of these — but it
// is a protocol between two halves of one library that are loaded separately:
// the page imports the entry, the browser fetches the worker module from a URL
// (which a bundler may have rewritten, and which a stale dist may answer). So
// it is written down once, here, with the reasoning, instead of twice as
// literals in two files.
//
// Page → worker: `start` first, then one `serve` per dial().
// Worker → page: `ready`, or `error`, and `exited` at the end.
//
// Every one of them is a plain object with a `drpc` tag and NEVER a
// Uint8Array, so none can be confused with a frame: the drpc wire only ever
// crosses the transferred MessagePorts — one message per marshaled Envelop
// (PROTOCOL.md §4.1) — and never the worker's own channel. That is what lets a
// worker started here also be a worker of the application's own, talking about
// something else on the same channel: anything untagged is not ours (see
// isPageMessage), and what we post is tagged so the application can tell the
// same way.

import type { WasmApp } from './instance'

// StartMessage runs the instance. Exactly one is honoured per worker: a worker
// is one instance, and a second start would need a second entry point, a
// second runtime and a second lifetime to report — which is what a second
// worker is.
//
// Every field is resolved by the page rather than defaulted again here, so the
// two halves cannot disagree about what a default is across a version skew.
export interface StartMessage {
  drpc: 'start'
  app: WasmApp
  wasmExec: string
  entryPoint: string
  readyTimeoutMs: number
}

// ServeMessage hands the instance one more port to serve as its own peer
// (PROTOCOL.md §6.4). The port is NOT in this object — a MessagePort cannot be
// cloned, only transferred — so it rides the transfer list and arrives as
// `ev.ports[0]`. It is exactly the default message dialWorker
// (../transport/port) posts, which is what makes that seam and this worker a
// working pair; a worker of your own can ignore the message entirely and read
// the port off the event.
export interface ServeMessage {
  drpc: 'serve'
}

// ReadyMessage says the instance published its entry point and can serve. It
// is the only success: open() resolves on it, and until it arrives the page
// knows nothing — go.run()'s promise settles at EXIT, and the page cannot see
// it from where it stands anyway.
export interface ReadyMessage {
  drpc: 'ready'
}

// ErrorMessage says the instance will never be ready — a module that would not
// fetch, a broken build, a runtime that is not being served, a main that died
// on the way up. It carries the message rather than the Error because an Error
// survives structured clone only as far as its own fields do, and what open()
// needs is the sentence: it is the only diagnosis the page will ever get.
export interface ErrorMessage {
  drpc: 'error'
  message: string
}

// ExitedMessage says a running instance is gone (§4.5) — the evidence a port
// cannot produce, since a MessagePort whose peer stopped existing looks
// exactly like one whose peer is merely quiet. The worker posts the goodbye on
// every port it holds first, then this; the page fails every Conn it dialled
// with the message as the cause, which with every protocol timer off (§10.6)
// is the only thing that ever ends a call on this channel.
export interface ExitedMessage {
  drpc: 'exited'
  message: string
}

export type PageMessage = StartMessage | ServeMessage
export type WorkerMessage = ReadyMessage | ErrorMessage | ExitedMessage

// isPageMessage / isWorkerMessage are the guards each half applies to
// everything that arrives. Anything untagged belongs to somebody else sharing
// the channel and is dropped in silence — the same treatment §4.2 gives input
// the adapter cannot read, and for the same reason: a message we do not
// understand must not be able to tear anything down.
export function isPageMessage(v: unknown): v is PageMessage {
  const tag = tagOf(v)
  return tag === 'start' || tag === 'serve'
}

export function isWorkerMessage(v: unknown): v is WorkerMessage {
  const tag = tagOf(v)
  return tag === 'ready' || tag === 'error' || tag === 'exited'
}

function tagOf(v: unknown): unknown {
  return typeof v === 'object' && v !== null ? (v as { drpc?: unknown }).drpc : undefined
}
