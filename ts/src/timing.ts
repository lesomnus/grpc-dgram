// Protocol timers of unreliable mode (PROTOCOL.md §10.1). All values are
// milliseconds; an absent field selects its default. Protocol timers run only
// in unreliable mode and only while calls (or their tombstones) are live.

export interface Timing {
  // T_call: the default unary deadline injected when the caller sets none.
  // Client side only.
  callMs?: number
  // T_live: the peer-liveness window. The probe threshold and cadence
  // T_probe is livenessMs / 3.
  livenessMs?: number
  // RTI: the control-frame retransmission base interval; it doubles per
  // attempt up to the probe cadence.
  retransmitMs?: number
  // TTL_tomb: how long finished calls are remembered.
  tombstoneMs?: number
  // T_hold: the delayed-RESET grace for unknown-sid frames whose OPEN may
  // merely be late.
  holdMs?: number
}

export interface ResolvedTiming {
  callMs: number
  livenessMs: number
  retransmitMs: number
  tombstoneMs: number
  holdMs: number
  // T_probe: stream-probe idle threshold and cadence, and the retransmission
  // backoff cap.
  probeMs: number
  // The coarse sweep period (PROTOCOL.md Appendix C): fine enough that every
  // timer tolerates the jitter, bounded so idle cost stays negligible.
  tickMs: number
}

export function resolveTiming(t: Timing = {}): ResolvedTiming {
  const callMs = t.callMs ?? 5_000
  const livenessMs = t.livenessMs ?? 15_000
  const retransmitMs = t.retransmitMs ?? 1_000
  let tombstoneMs = t.tombstoneMs ?? 30_000
  if (tombstoneMs < 2 * livenessMs) {
    // TTL_tomb floor while liveness is enabled (PROTOCOL.md §9.2).
    tombstoneMs = 2 * livenessMs
  }
  const holdMs = t.holdMs ?? retransmitMs
  const tickMs = Math.max(1, Math.min(Math.min(retransmitMs, holdMs) / 2, 500))
  return { callMs, livenessMs, retransmitMs, tombstoneMs, holdMs, probeMs: livenessMs / 3, tickMs }
}

// mode aggregates the resolved transport profile.
export interface Mode {
  reliable: boolean
  timing: ResolvedTiming
}
