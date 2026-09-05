# TODO

What is left, and what has to be decided before it can start. Everything that
is *done* lives in the code, in [PROTOCOL.md](./PROTOCOL.md), or in the feature
docs next to this file — this list is only the open end.

## 1. `Envelop` batching (`Coalescer`) — deferred on purpose

The wire has always been "one marshaled `Envelop` of 1..n frames per transport
message" (§4.1), and the spec already carries the normative duties a batching
middleware would have to honor: re-expose the wrapped adapter's `TransportInfo`
(§3), fail an oversize frame synchronously in `Handle` because an async
`MaxDelay` flush has no owning call to fail (§4.4), and add up to `MaxDelay` per
direction to every bound in §10.7. What does not exist is the design.

Four decisions come first. They are not implementation details; each one
changes what the feature is.

**What may share an envelop.** Frames in one envelop share its fate (§4.1): if
it is lost, all of them are. Batching frames of *different calls* therefore
couples calls that the protocol otherwise keeps independent — one datagram loss
becomes a gap in three streams. The narrow version (batch only within a call, or
only control frames: retransmission ticks, probes, keepalives, which §10.3
already emits in bursts) is safe and much less useful; the wide version needs an
argument for why coupled loss is acceptable.

**The latency budget.** `MaxDelay` is added to every termination bound, in both
directions. The workload this library targets is sensor streams where a reading
loses value as it ages, so a batching window that helps throughput is directly
subtracted from the thing the library exists to protect. Before writing code:
measure. A benchmark that shows syscall or header overhead dominating at a real
message rate is the entry condition.

**Interaction with flow control (new in v1.1).** Credit is accounted in
*messages* (§4.2.1); a batch is a *transport message*. Open questions: if a
sender runs out of credit mid-batch, does it flush the partial batch or park
holding it? May a `WINDOW` grant ride the same batch as data — and if it does,
can a credit update end up waiting behind the very frames it would release?

**Interaction with compression (new in v1.1).** Per-frame compression (§12.1) or
per-batch? Per-batch compresses better across small similar messages but makes
the COMPRESSED marker a property of the envelop, which the frame-level flag
cannot express today.

Entry conditions: (1) a benchmark that shows the win, (2) the four decisions
above, (3) a §4.1/§10.7 spec revision to match.

## 2. Release preparation

- **Adapter `replace` directives.** `transport/pion/go.mod` and
  `transport/gorilla/go.mod` (and the example modules) carry
  `replace github.com/lesomnus/grpc-dgram => ../..`. A `replace` is ignored by
  anyone who *depends* on the published module, so as long as they are there and
  the core is untagged, those adapters cannot be consumed from outside this
  repo. Tagging a release means: tag the core, drop the replace, require the
  real version, and re-tag the adapters.
- **TypeScript packaging** — done (`@lesomnus/grpc-dgram` 0.0.1, Apache-2.0,
  `publishConfig.access: public`). Still open: the versioning relationship to
  the Go modules (they share a wire version, not a release cadence).
- **Wire freeze.** PROTOCOL.md is v1.1 and still pre-release, which is what
  makes breaking wire changes cheap. A release fixes that; anything the wire
  should carry natively (see below) is cheaper to add before it.

## 3. TypeScript parity, if and when it is wanted

The port deliberately stops short of the Go feature set in three places
(`ts/STATUS.md` has the reasoning):

- client/server **interceptors**;
- the **`stats.Handler` bridge** — `ProtocolStats`/`Counters` are ported
  (`ts/src/stats.ts`, so a browser client reports the §14 gap counter), but the
  grpc-go `stats.Handler` type has no TS counterpart and is not mirrored;
- **`Envelop` batching**, which follows item 1 in both languages.

## 4. Smaller, unowned

- **Connection-level flow control.** §4.2.1 bounds a *stream*; the aggregate a
  peer can pin is still `MaxLiveCalls × window`. A per-peer window (HTTP/2 has
  one) would make the memory bound meaningful, and `Limits` is where it belongs.
- **A `Peer()` for the pion adapter that names the ICE candidate pair** instead
  of the DataChannel label, so `peer.FromContext` reports a routable address.
- **Reserved wire space.** §5 lists what is reserved for future work: the `ack`
  field that would let a long-lived half-closed stream stop retransmitting its
  CLOSE (§10.3), and any further status plumbing. Field 18 is the next free
  number.
