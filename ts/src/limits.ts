// Configurable delivery buffers (PROTOCOL.md §4.2) and resource caps (§15).

// DropPolicy selects what a full per-stream rx buffer discards in unreliable
// mode (PROTOCOL.md §4.2). Reliable mode never drops: delivery blocks instead.
export enum DropPolicy {
  // Discard the arriving frame (the default): the buffered prefix is
  // preserved.
  Newest,
  // Discard the oldest buffered frame to admit the newest — freshest-wins,
  // suited to state-sync / sensor streams.
  Oldest,
}

export interface RxBufferConfig {
  size?: number
  policy?: DropPolicy
}

export interface ResolvedRxConfig {
  size: number
  policy: DropPolicy
}

export function resolveRxConfig(c: RxBufferConfig = {}): ResolvedRxConfig {
  const size = c.size !== undefined && c.size > 0 ? c.size : 32
  return { size, policy: c.policy ?? DropPolicy.Newest }
}

// Limits bounds the endpoint's bookkeeping (PROTOCOL.md §15). Absent fields
// keep their defaults. On a Conn only maxPendingResets and maxPeerWindow
// apply.
export interface Limits {
  // Caps stored tombstone entries per peer incarnation. Past it the lowest
  // sid is evicted and the container's floor rises: evicted sids keep
  // key-only semantics (deduped, replay lost) at zero memory.
  maxTombstones?: number
  // Caps stored terminal-frame payload bytes per peer incarnation; oldest
  // stored terminals degrade to key-only past it.
  maxTombstoneBytes?: number
  // Caps retained finished peer incarnations per transport peer; oldest are
  // evicted (never one with live calls).
  maxDeadPeers?: number
  // Caps the RESET rate-limit / delayed-RESET / reply-budget maps.
  maxPendingResets?: number
  // Caps concurrently live calls per transport peer, counted across client
  // epochs; an OPEN past it is refused with RESOURCE_EXHAUSTED.
  maxLiveCalls?: number
  // Caps, per transport peer, the control replies the server volunteers
  // within one RTI — tombstone/creation-ack replays and RESETs — on top of
  // the per-object 1/RTI limits (anti-amplification).
  maxRepliesPerRTI?: number
  // Caps, in messages, what one transport peer may have buffered here across
  // all of its calls and client epochs — the connection flow-control window
  // (§4.2.1, reliable mode only); on a Conn it bounds the one peer the Conn
  // talks to. Values below W_CONN (1024) are raised to it: a sender assumes
  // W_CONN before any sid-0 grant, so a receiver holding less would be
  // overrun by a conforming sender — the same reason the rx buffer is
  // floored at W_INIT. Past it, the frame that overruns fails its own call
  // with INTERNAL, never the peer. Capped at 2^32 − 1, the wire's uint32
  // (§7): Infinity means that much, not "off".
  maxPeerWindow?: number
}

// DEFAULT_MAX_PEER_WINDOW is the default and the floor of maxPeerWindow: it
// equals W_CONN (util.ts), the sender's assumption — spelled out here rather
// than imported, since util.ts already imports this module.
const DEFAULT_MAX_PEER_WINDOW = 1024

// MAX_PEER_WINDOW is the cap of maxPeerWindow: `window` is a uint32 on the
// wire (§7), and the window is what the raise puts there (window − W_CONN,
// §4.2.1) and what the grant rule measures against. Go's field is an int
// cast to uint32 at the ledger, so it can never hold more; in JS a number
// can, and Infinity is the natural spelling of "unlimited" — unclamped it
// would encode as a WINDOW of 0 (dropped: a grant never enables) and never
// trip a batched grant, so the peer would spend its assumed W_CONN and park
// for good. Clamped, it is a window no peer can fill.
const MAX_PEER_WINDOW = 0xffff_ffff

export interface ResolvedLimits {
  maxTombstones: number
  maxTombstoneBytes: number
  maxDeadPeers: number
  maxPendingResets: number
  maxLiveCalls: number
  maxRepliesPerRTI: number
  maxPeerWindow: number
}

export function resolveLimits(l: Limits = {}): ResolvedLimits {
  const pos = (v: number | undefined, d: number) => (v !== undefined && v > 0 ? v : d)
  return {
    maxTombstones: pos(l.maxTombstones, 1024),
    maxTombstoneBytes: pos(l.maxTombstoneBytes, 1 << 20),
    maxDeadPeers: pos(l.maxDeadPeers, 4),
    maxPendingResets: pos(l.maxPendingResets, 1024),
    maxLiveCalls: pos(l.maxLiveCalls, 4096),
    maxRepliesPerRTI: pos(l.maxRepliesPerRTI, 64),
    // Floored, not just defaulted (the sender's assumption), capped at the
    // wire's uint32, and whole: a message count. Infinity clamps to the cap;
    // NaN, like any non-positive value, keeps the default.
    maxPeerWindow: Math.min(Math.max(Math.floor(pos(l.maxPeerWindow, DEFAULT_MAX_PEER_WINDOW)), DEFAULT_MAX_PEER_WINDOW), MAX_PEER_WINDOW),
  }
}
