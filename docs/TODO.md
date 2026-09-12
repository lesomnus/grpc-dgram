# TODO

What is left, and what has to be decided before it can start. Everything that
is *done* lives in the code, in [PROTOCOL.md](./PROTOCOL.md), or in the feature
docs next to this file — this list is only the open end.

## 1. `Envelope` batching — the seam is ours, the policy is the application's

The wire has always been "one marshaled `Envelope` of 1..n frames per transport
message" (§4.1), and the receive side has always delivered n of them
(`drpc.Unpack`). What this section deferred was a `Coalescer` — a batcher the
library itself would ship — behind four decisions, a benchmark that shows the
win, and a §4.1/§10.7 revision to match. The benchmark now exists
([Envelope batching: the measurement](./batching-measurement.md)), and it
resolves the section by answering a different question than the one it was
asked.

**What it found.** At a 22 B payload the transport is 4727 ns of the 5442 ns a
`SendMsg` costs — 86.9%, of which 3936 ns is the bare `write` syscall — and a
second frame in the same datagram is free: two batched cost 4717 ns where the
same two sent apart cost 9459 ns. Twenty-nine frames fit the 1200 B budget at
22 B, at 306 ns/frame: a 93.5% saving. And none of that matters at the rate
this library is for — at the 200 Hz one stream of `examples/udp-sensor` runs
at, that whole transport is about 0.1% of one core.

So there is no `MaxDelay` worth choosing on an application's behalf: a window
that collects anything at 200 Hz costs tens of milliseconds, added to every
bound in *both* directions (§10.7), to recover a tenth of a percent of a core.
But a batcher that waits for nobody — one that packs only frames already
together in the sender, as a retransmission tick's burst is (§10.3) — pays no
latency at all and keeps the whole saving. Whether a workload produces such
frames is the only thing that decides the question, and it is not something
this repository can know. Hence: **the library provides the seam, and the
batching policy is the application's to write.** PROTOCOL.md now says the same
normatively (§3, §4.1).

**The seam is complete and exported in both languages.** In Go, `frame.go` has both handler types
— `FrameHandler` (core-facing, one frame) and `EnvelopeHandler` (adapter-facing,
one envelope of 1..n frames), which every adapter satisfies with its exported
`Send`, and `Unpack` for the receive side. Every shipped adapter exports
an envelope-level send (`udp.Transport.Send(ctx, *drpc.Envelope)`, and the same
shape in the others). A batcher is a
`FrameHandler` that buffers frames and calls that `Send` with what it packed;
it needs no library change to write, and `transport/udp/batcher_test.go` is one
built entirely out of what is exported today.

Five duties bind whatever is installed there. None of them is a policy
choice, and every one of them fails silently — which is why they are written
normatively in §4.1, §4.4 and Appendix C rather than left to taste:

- **Embed the adapter; do not hold it in a field.** `ConnAttacher`,
  `io.Closer`, `TransportInfo` and `TransportPeer` are all found on the tx by
  type assertion (`conn.go`). Embedding promotes `AttachConn`, `Close`,
  `Reliable` and `Peer` through the batcher; a
  field-wrapper hides all four, and the worst of those failures is silent —
  without `AttachConn` the receive pump never starts and the endpoint receives
  nothing, with no error (§3 and Appendix C put the duty normatively).
- **One destination per envelope.** A datagram is addressed by the `ctx` of the
  call that flushes it, not by anything in its frames — `udp.Gateway.Send`
  reads the peer out of `ctx` (§6.4), and the server hands one tx a different
  per-peer ctx per peer. So a batcher above a **gateway** may pack together
  only frames whose ctx names the same peer (on pion, the same channel mode
  too, §4.3), and must flush on a ctx naming that peer. Mix two and peer B
  receives A's frames while A receives nothing: A's calls die of their
  deadlines, B drops a frame for an epoch it does not own, nothing errors. A
  client `Transport` over a connected socket has one destination and is
  exempt. The ctx a deferred flush holds on to must be a tx ctx — no tx path
  may depend on an rx ctx surviving (§6.4).
- **Serialise the buffer; `Handle` is called concurrently.** Every call
  goroutine transmits, and so do the retransmission/keepalive sweep
  (`unreliable.go`) and the `WINDOW` grant paths (`conn.go`, `server.go`). The
  shipped adapters are stateless below `Handle` and never had to care; a
  batcher is the first thing at this seam with mutable state, and without a
  lock it races on its own buffer on the second concurrent call. On a
  *reliable* adapter it must also keep envelopes in the order it packed them
  (§4.3 promises no reordering, and reliable mode has no retransmission to
  repair one): holding the lock across the flush does that, at the price the
  core refuses to pay itself (it sends outside every lock so a blocking
  adapter cannot wedge `Handle`), and one flushing goroutine does it without
  that price.
- **Own a byte budget no larger than the adapter's, and reject an oversize
  frame synchronously** from the `Handle` that carried it in (§4.4). Once a
  frame is buffered its call has moved on, and a later flush's error is worse
  than lost: it comes back out of whatever *other* call's `Handle` triggered
  the flush, and the core reads a synchronous `ErrMessageTooLarge` as "this
  frame never reached the wire" — it reclaims that innocent call's `seq` and
  refunds its credit while the frame that really overran is dropped in
  silence, and the reclaimed `seq` goes back out on a second frame. Equal
  budgets remove the case: what the batcher accepts, the adapter takes.
- **Count the delay, and never sit on a grant.** Whatever the batcher holds a
  frame for is added to every termination bound of §10.7, in both directions —
  and a `WINDOW` grant rides the same seam as data, so a batcher with no delay
  budget at all (flush every k frames, say) can hold the grant that would
  unpark its peer while the peer, parked, sends nothing that would complete
  the batch. Flush uncredited control frames at once; otherwise the bound is
  not `MaxDelay` but `T_stall`, where the peer's call fails `UNAVAILABLE`
  (§4.2.1).

The four decisions this section said had to come first resolve like this — two
belong to the application, one dissolves, and one stays ours:

**What may share an envelope** — the application's. Frames in one envelope share
its fate (§4.1): batching frames of *different calls* couples calls the
protocol otherwise keeps independent, and one datagram loss becomes a gap in
three streams. The narrow version (within one call, or only control frames:
the retransmission ticks §10.3 already emits in bursts, probes, keepalives) is
safe and much less useful. Which trade is acceptable is a property of the
workload, not of the protocol. Across *peers* there is no trade to make: one
datagram carries one address, so that boundary is a duty above, not a choice.

**The latency budget** — the application's, and the measurement prices it: at
200 Hz, `k = 2` costs 5 ms and saves half the transport, `k = 8` saves 84% for
35 ms, `k = 29` saves 93.5% for 140 ms; at 1000 msg/s per stream the same three
cost 1 ms, 7 ms and 28 ms. How much age a reading can take is the operator's
number.

**Interaction with flow control (new in v1.1)** — *dissolves*. Credit is
accounted in messages (§4.2.1) and the core takes it on the way *into* the tx
path, before the frame it will send exists (`stream.go`: `acquire2` runs ahead
of the frame build, and refunds it if the call ends before the wire). Every
frame that reaches the `FrameHandler` seam is therefore already paid for, so a
batcher below that seam can never run out of credit mid-batch and has no credit
of its own to return. What is left is the return trip, and it is a liveness
question rather than an accounting one: a `WINDOW` grant goes out through the
same seam, so a batcher that holds it holds the thing that would make its peer
send again. With a delay budget the cost is that budget, on the §10.7 bounds;
without one — a count-only flush, and §4.1 allows it — the peer stays parked
until `T_stall` and its call fails `UNAVAILABLE`. Hence the duty above: flush
uncredited control frames immediately.

**Interaction with compression (new in v1.1)** — *stays ours*, if we ever want
it. Per-frame compression belongs to the core and `COMPRESSED` is a frame flag
(§12.1), so a batcher below the core only ever packs frames whose compression
is already decided, and must pack them unchanged. Per-batch compression would
need an envelope-level flag to carry the marker — a wire change, and one that is
cheap only before the freeze (item 2).

**TypeScript has the same seam**, in the shape the language makes natural.
Discovery there is structural — `hasConnAttacher` is a `typeof
tx.attachConn === 'function'` check, and the same for `reliable()` and
`close()` — so a subclass inherits every one of them through the prototype
and there is no Go-style wrapper trap: `class Batcher extends UdpTransport`
overriding `handle` alone keeps all three. What it calls is `sendFrames`,
the envelope-level entry every exported adapter class now carries
(`UdpTransport`, `UdpGateway`, `WebSocketTransport`, `WebSocketGateway`,
`DataChannelTransport`, `DataChannelGateway`, `PortTransport`,
`PortGateway`, `WebTransportDatagramTransport`); `handle` is a one-line
delegation to it, so an override reaches exactly the code the core would
have. It is named `sendFrames` rather than `send` because the objects these
adapters sit on already have a byte-level `send(data)`.
`ts/src/transport/node-udp/batcher.test.ts` is the twin of the Go one, and
adds the per-peer case the connected-socket Go test cannot show.

Two traps particular to TypeScript, both surfaced by writing that test.
`private` members are part of the type: a subclass that declares its own
field named like a base `private` one (`max`, and `send` under
`WebTransportDatagramTransport`) is rejected and, worse, becomes
unassignable to the base type. And of §4.1's concurrency duty only the first
half is free: the buffer needs no lock while it is touched inside one
synchronous turn, but the ordering half still binds. On a reliable adapter a
flush can park — `Socket.send` and `Channel.send` wait for the socket to open
and while `bufferedAmount` is at the high-water mark — and a second flush
issued meanwhile goes straight out past it, which reliable mode has no
retransmission to repair. A batcher there must keep one flush in flight,
chaining each on the last.

## 2. Release preparation

- **Adapter `replace` directives.** `transport/pion/go.mod`,
  `transport/gorilla/go.mod` and `transport/webtransport/go.mod` (and the
  example modules) carry
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

The port stops short of the Go side in two places — one deliberate, one a gap
(`ts/STATUS.md` has the reasoning):

- the **`stats.Handler` bridge** — `ProtocolStats`/`Counters` are ported
  (`ts/src/stats.ts`, so a browser client reports the §14 gap counter), but the
  grpc-go `stats.Handler` type has no TS counterpart and is not mirrored;

That is the whole of it. The **connection window** (`WINDOW sid=0`, §4.2.1)
that this section once listed as sequenced rather than deliberate is in the
port: `W_CONN`, `FlowSender.confirm`, `acquireBoth`, `PeerFlowRx`,
`maxPeerWindow` and the two `peer-flow-*` event kinds mirror `flow.go`, and
both cross-language suites move more than `W_conn` messages each way across
three streams with the `sid = 0` grants asserted in both directions — so the
Direction A failure Appendix A entry 11 describes no longer has a peer in
this repo to happen against, and issue #4 is complete on both sides.

## 4. Smaller, unowned

- **WebTransport, step two: a reliable channel over a session stream.**
  `transport/webtransport` and its TS twin (issue #6, step one) use only the
  session's datagrams — unreliable, `Reliable() == false`, in both languages.
  The follow-up the issue names is a reliable channel over one of the same
  session's streams, on the pion precedent: one session carrying a reliable
  control channel next to the datagram telemetry, each peer annotated with its
  channel's mode (`NewReliableContext`), and the mode derived from the
  channel rather than configured. Two things wait on a runtime rather than on
  design: a TS *server* (Node has no `WebTransport`, client or server), and
  the browser-driven Go↔TS conformance run for this channel — the Go module's
  own suite is the bar today.
- **A `Peer()` for the pion adapter that names the ICE candidate pair** instead
  of the DataChannel label, so `peer.FromContext` reports a routable address.
- **Reserved wire space.** §5 lists what is held for future work: the `ack`
  field that would let a long-lived half-closed stream stop retransmitting its
  CLOSE (§10.3), and any further status plumbing. `ack` rides every server
  frame, so it takes field 6 — the last one-byte tag, kept unassigned for it.
  Cold fields (OPEN/H/T only) start at 18. `ack` is genuinely additive: a
  receiver that never sends one just leaves the sender retransmitting, which
  is today's behaviour, so it can land after a release at no cost.
