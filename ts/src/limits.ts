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
// keep their defaults. On a Conn only maxPendingResets applies.
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
}

export interface ResolvedLimits {
  maxTombstones: number
  maxTombstoneBytes: number
  maxDeadPeers: number
  maxPendingResets: number
  maxLiveCalls: number
  maxRepliesPerRTI: number
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
  }
}
