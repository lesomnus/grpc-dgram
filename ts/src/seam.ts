// The transport seams (PROTOCOL.md §3, §4). The core emits and consumes
// individual frames; the wire unit is always one Envelop per transport
// message, which adapters marshal/unmarshal themselves (encodeEnvelop /
// decodeEnvelop in wire.ts).

import type { Conn } from './conn'
import type { Frame } from './wire'

// FrameContext carries what Go passes through context.Context:
// - peer: the transport-level identity of the remote end (PROTOCOL.md §6.4),
//   attached by adapters on the rx path and read back on the tx path.
//   Single-peer transports may omit it. Keys are compared by SameValueZero
//   (Map semantics): use primitives or stable object references.
// - reliable: the per-channel mode annotation of a mixed-mode gateway
//   (PROTOCOL.md §4.3); constant for a given peer.
// - signal: the rx bound for reliable-mode blocking delivery (§4.2), aborted
//   by adapter teardown. On the tx path, a bound for a blocking send.
export interface FrameContext {
  peer?: unknown
  reliable?: boolean
  signal?: AbortSignal
}

// FrameHandler is the seam both directions share. On the rx path adapters
// call Conn.handle / Server.handle once per frame of a received envelop, in
// order — awaiting each so reliable-mode backpressure propagates (§4.2). On
// the tx path the core calls the adapter; a returned promise lets a send
// block (backpressure), and a synchronous throw of MessageTooLargeError
// refuses an oversize message (§4.4).
//
// A rejection means "malformed input" or a fatal local condition; adapters
// MUST NOT tear down the channel on frame-level errors (§4.2).
export interface FrameHandler {
  handle(frame: Frame, ctx?: FrameContext): void | Promise<void>
}

// TransportInfo advertises transport capabilities; adapters implement it
// alongside handle, and Conn/Server discover it once at construction.
// Explicit options always override discovery (PROTOCOL.md §4.3). Reliability
// is the only capability the core needs — message size is deliberately the
// adapter's concern (§4.4).
export interface TransportInfo {
  reliable(): boolean
}

// ConnAttacher is discovered on the tx by the Conn constructor: the
// transport receives the Conn it serves and starts its own receive pump, so
// the client manages nothing (gRPC parity). Conn.close also calls a tx
// close() when present, so one close tears the whole endpoint down; the
// transport's close must be idempotent.
//
// Servers deliberately have no equivalent: registration must precede the
// first received frame (the registry freezes when serving starts, §13), so a
// server transport is started explicitly — servePeer — after register.
export interface ConnAttacher {
  attachConn(conn: Conn): void
}

export function hasTransportInfo(tx: FrameHandler): tx is FrameHandler & TransportInfo {
  return typeof (tx as Partial<TransportInfo>).reliable === 'function'
}

export function hasConnAttacher(tx: FrameHandler): tx is FrameHandler & ConnAttacher {
  return typeof (tx as Partial<ConnAttacher>).attachConn === 'function'
}

// unpack delivers each frame of a decoded envelop to h in order, awaiting
// each so backpressure propagates (PROTOCOL.md §4.1). Frame-level failures
// are swallowed: they never tear down the channel (§4.2).
export async function unpack(frames: readonly Frame[], h: FrameHandler, ctx?: FrameContext): Promise<void> {
  for (const f of frames) {
    try {
      await h.handle(f, ctx)
    } catch {
      // Frame-level errors are the adapter's to ignore (§4.2).
    }
  }
}
