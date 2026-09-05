// The observability surface (PROTOCOL.md §14): ProtocolStats, the observer
// for the datagram-specific events gRPC has no concept of — the
// skipped-message counter §14 promises, rx drops, RESETs, retransmissions,
// probes, liveness expiry, tombstone replays and flow-control stalls — and
// Counters, the ready-made observer that just counts them. This is the TS
// twin of stats.go's ProtocolStats half; the other half there, the
// google.golang.org/grpc/stats.Handler bridge, is a grpc-go type with no
// counterpart here and is deliberately not mirrored.
//
// Why it exists on this side at all: a gap is not an error. §14 promises an
// ordered subsequence and §6.3 accepts any forward step within W_fwd, so a
// lost frame produces a shorter stream and nothing else — and the receiver
// on the lossy path this library exists for is a browser, on the far end of
// a server-streaming call, where every gap happens. Without this surface a
// page cannot tell 244 of 400 readings from 400 of 400.

// ProtocolEventKind names a drpc protocol event. The strings are the ones
// Go's ProtocolEventKind.String() fixes, so a log line reads the same from
// either end.
export type ProtocolEventKind =
  // A seq gap was observed on a stream. count is the number of messages the
  // gap skipped — the counter PROTOCOL.md §14 promises.
  | 'skipped'
  // A frame was dropped by the rx buffer's drop policy (§4.2). count is 1.
  | 'dropped'
  // A frame was dropped because its shape is illegal for the call's RPC
  // type (§8), or trailer metadata failed validation and was discarded (§11).
  // count is 1.
  | 'off-shape'
  // A RESET was volunteered / acted on (§9.3).
  | 'reset-sent'
  | 'reset-received'
  // A control frame was retransmitted (§10.3).
  | 'retransmit'
  // A stream probe was sent (§10.5).
  | 'probe-sent'
  // A peer keepalive PING was sent (§10.4).
  | 'keepalive-sent'
  // A peer's liveness window elapsed (§10.4).
  | 'liveness-expired'
  // A stored terminal was replayed (§9.2).
  | 'tombstone-replay'
  // A window overrun failed a call loudly (§6.3).
  | 'data-loss'
  // A sender parked waiting for flow-control credit, and got some (§4.2.1,
  // reliable mode).
  | 'flow-stall'
  | 'flow-resume'

// ProtocolEvent describes one protocol event. sid is 0 and method '' for
// peer-scope events; peer is set on every server-side event (the transport
// peer the event concerns, PROTOCOL.md §6.4) and absent on the client, whose
// one channel is one peer.
export interface ProtocolEvent {
  readonly kind: ProtocolEventKind
  readonly peer?: unknown
  readonly sid: number
  readonly method: string
  // The event's magnitude where one exists: messages skipped ('skipped'),
  // frames dropped in this event ('dropped' and 'off-shape', always 1). 0
  // otherwise.
  readonly count: number
}

// ProtocolStats observes drpc protocol events. It must not block: these fire
// on the receive path and the sweep, synchronously, and an endpoint's whole
// delivery waits behind a slow observer. Do arithmetic and return. A throw
// is contained (see emit) and costs that observer the event, nothing else.
// Go's ProtocolStatsFunc collapses into this type.
export type ProtocolStats = (ev: ProtocolEvent) => void

// CounterSnapshot is a point-in-time read of Counters.
export interface CounterSnapshot {
  skipped: number // messages lost to gaps (§14)
  dropped: number // frames dropped by the rx drop policy (§4.2)
  offShape: number // frames dropped as illegal for the call shape (§8)
  resetSent: number
  resetReceived: number
  retransmit: number // control frames retransmitted (§10.3)
  probeSent: number
  keepaliveSent: number
  livenessExpired: number
  tombstoneReplay: number
  dataLoss: number // calls failed by window overrun (§6.3)
  flowStall: number // sends parked on flow-control credit
  flowResume: number // parked sends that got credit and continued
}

// Counters is a ready-made ProtocolStats that just counts, for applications
// that want the §14 gap counter without writing an observer. `observe` is
// the function to install — an arrow, so it can be handed over unbound:
//
//   const counters = new Counters()
//   const conn = new Conn(tx, { protocolStats: counters.observe })
//   …
//   counters.snapshot().skipped // readings the wire ate
export class Counters {
  private readonly n: CounterSnapshot = {
    skipped: 0,
    dropped: 0,
    offShape: 0,
    resetSent: 0,
    resetReceived: 0,
    retransmit: 0,
    probeSent: 0,
    keepaliveSent: 0,
    livenessExpired: 0,
    tombstoneReplay: 0,
    dataLoss: 0,
    flowStall: 0,
    flowResume: 0,
  }

  readonly observe: ProtocolStats = (ev) => {
    // The three magnitudes add their count; everything else is one event,
    // one increment — as Go's Counters does.
    const n = Math.max(ev.count, 1)
    switch (ev.kind) {
      case 'skipped':
        this.n.skipped += n
        break
      case 'dropped':
        this.n.dropped += n
        break
      case 'off-shape':
        this.n.offShape += n
        break
      case 'reset-sent':
        this.n.resetSent++
        break
      case 'reset-received':
        this.n.resetReceived++
        break
      case 'retransmit':
        this.n.retransmit++
        break
      case 'probe-sent':
        this.n.probeSent++
        break
      case 'keepalive-sent':
        this.n.keepaliveSent++
        break
      case 'liveness-expired':
        this.n.livenessExpired++
        break
      case 'tombstone-replay':
        this.n.tombstoneReplay++
        break
      case 'data-loss':
        this.n.dataLoss++
        break
      case 'flow-stall':
        this.n.flowStall++
        break
      case 'flow-resume':
        this.n.flowResume++
        break
    }
  }

  snapshot(): CounterSnapshot {
    return { ...this.n }
  }
}

// statsSink resolves an endpoint's protocolStats option into the list an
// emitter fans out to: nothing, one, or several — Go accepts the option more
// than once, and an array is the TS spelling of that.
export function statsSink(opt: ProtocolStats | readonly ProtocolStats[] | undefined): readonly ProtocolStats[] {
  if (opt === undefined) return []
  // Copied: the set is resolved at construction, as Go resolves each
  // WithProtocolStats, not read back from an array the application may go on
  // mutating.
  return typeof opt === 'function' ? [opt] : [...opt]
}

// emit fans one event out. Peer-scope callers pass sid 0 / method ''; the
// stream-scope emitters fill them in first.
//
// An observer's failure is its own. Every emitter sits between a state step
// and the action it accounts for — the window advanced and the frame not yet
// delivered, the clock stamped and the PING not yet sent, ps.dead set and the
// cancels not yet run — and the sweep runs on a bare setInterval. A throw
// escaping from here would leave that step half-taken: a call whose liveness
// expired but was never failed, a frame the window has moved past but nobody
// received, a send on a healthy call failing UNKNOWN. Go documents "must not
// block" and lets a panic crash; here the contract is the same and the
// consequence of breaking it is contained instead, because the endpoint's
// correctness must not be one thrown exception away from a metrics hook.
export function emit(sink: readonly ProtocolStats[], ev: ProtocolEvent): void {
  for (const h of sink) {
    try {
      h(ev)
    } catch {
      // The observer's own problem, by contract; the protocol's step proceeds.
    }
  }
}
