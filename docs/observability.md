# Observability

Two surfaces, answering different questions. Both are described normatively in
[PROTOCOL.md](./PROTOCOL.md) §14 and implemented in
[`stats.go`](../stats.go); their behavior is pinned by
[`stats_test.go`](../stats_test.go).

`WithStatsHandler` takes a `google.golang.org/grpc/stats.Handler` — the same
interface OpenTelemetry, opencensus and every hand-rolled gRPC metrics
middleware already implement. It reports **calls**: begin, headers, one event
per message, trailers, end, with the call's status on `End.Error`. Existing
instrumentation works unchanged because these are literally the same events.

`WithProtocolStats` takes a dRPC `ProtocolStats` observer. It reports what a
datagram channel does that gRPC has no vocabulary for: a gap in a stream, a
frame the receive buffer threw away, a RESET, a retransmission, a liveness
window expiring. None of that reaches a `stats.Handler`, because in gRPC's
model those events cannot happen — HTTP/2 has no lost messages to count.

| Question | Surface |
|---|---|
| How long do my RPCs take, per method, per code? | `stats.Handler` |
| How many bytes on the wire, compressed and not? | `stats.Handler` |
| How much of my stream is the application actually receiving? | `Counters.Skipped` |
| Is the loss the network's, or my consumer's? | `Skipped` vs `Dropped` |
| Is the peer still there? | `LivenessExpired`, `KeepaliveSent` |
| Why is this stream slow on a reliable channel? | `FlowStall` / `FlowResume` |
| Who is flooding me with junk? | `ResetSent`, `TombstoneReplay` (both name the peer) |

Both options may be given more than once, on either endpoint, and are
independent — installing one does not imply the other:

```go
counters := &drpc.Counters{}

conn := drpc.NewConn(udp.New(c),
    drpc.WithStatsHandler(&metrics{m: sink}), // gRPC's surface
    drpc.WithProtocolStats(counters),         // drpc's surface
)
```

```go
srv := drpc.NewServer(gw,
    drpc.WithStatsHandler(&metrics{m: sink}),
    drpc.WithProtocolStats(counters),
)
```

## The gRPC surface: `stats.Handler`

`TagRPC` runs once per call and its returned context is threaded through every
later event of that call — that is where per-call state lives. `TagConn` and
`HandleConn` run **only on a client**: `NewConn` emits a `ConnBegin` and
`Close` emits a `ConnEnd`, both with `Client: true`. A server has no
equivalent, because its peers come and go per received frame rather than per
connection; per-peer server visibility is `ProtocolStats`, where every event
carries `Peer`.

### Event order

The sequences below are exact, asserted element-by-element in `stats_test.go`.
A successful unary call:

```
client   Begin  OutHeader  OutPayload  InPayload  InTrailer  End
server   Begin  InHeader   InPayload   OutPayload OutTrailer End
```

A bidi call sending and receiving two messages:

```
client   Begin  OutHeader  [OutPayload InPayload] x2  InTrailer   End
server   Begin  InHeader   [InPayload OutPayload] x2  OutTrailer  End
```

A call the handler failed, here with `OUT_OF_RANGE` — no response was
produced, so no payload event is invented for one:

```
client   Begin  OutHeader  OutPayload  InTrailer  End
server   Begin  InHeader   InPayload   OutTrailer End
```

`Begin` and `OutHeader` are emitted at stream creation, before the OPEN frame
reaches the transport, so a call that dies in the adapter still has a `Begin`.
`End` is the call's last event on both ends, as gRPC guarantees: on a unary
call `Invoke` emits it *after* the response has been delivered to the caller,
not from the receive path — the response only becomes an `InPayload` when the
caller unmarshals the terminal frame, which happens after the call has already
ended internally.

Four things are not reported, and it is worth knowing which:

- **No `OutHeader` for the protocol's own creation ack.** A streaming call's
  server sends an `H` frame to acknowledge the OPEN (§8), but the handler did
  not flush a header, so nothing is reported. An explicit `grpc.SendHeader` /
  `SetHeader` flush *is* reported, between `InPayload` and `OutPayload`,
  exactly where grpc-go reports it.
- **No `InTrailer` when no terminal frame arrived.** A call that ends locally
  — deadline, RESET, liveness expiry, transport teardown — goes straight to
  `End` with the status on `End.Error`. There were no trailers on the wire to
  report.
- **No events at all for a refused OPEN.** A draining server, the
  `MaxLiveCalls` cap (§15), or a duplicate OPEN hitting a tombstone (§9.2)
  never create a call, so there is no `Begin` and no `End`. Those show up only
  as `ResetSent` / `TombstoneReplay` on the protocol surface.
- **No `OutPayload` for a client-streaming response.** `SendAndClose`'s
  message rides the terminal frame (§8) and never passes the server's
  reporting path. The client still sees it as an `InPayload`, so the message
  is not invisible — but a server-side "bytes sent" metric under-counts
  client-streaming methods. This one is a divergence from grpc-go, not a
  design choice.

### `Length` vs `CompressedLength`

`InPayload` and `OutPayload` carry both, with the same meaning grpc-go gives
them: `Length` is the message as the codec produced it, `CompressedLength`
(and `WireLength`, set to the same value) is what the frame actually carried.
Without `grpc.UseCompressor` they are equal — the uncompressed-call assertion
in `TestStats_HandlerUnaryParity`. With a compressor they diverge, and the
ratio is the useful number: `CompressedLength` is what you are paying for, and
on an unreliable adapter it is what has to fit one datagram (§4.4).

One asymmetry to know about: a **unary response** is marshaled by the handler
path but compressed later, when the terminal frame is built (§8), so the
server's `OutPayload` for it reports the *uncompressed* size in both fields.
The client's `InPayload` for the same message reports the real wire size. Data
frames on a streaming call are compressed before reporting and are accurate on
both ends.

`InHeader.WireLength` is not a header size: it is the OPEN frame's payload
length, which is the request message for unary and server-streaming calls
(they piggyback it, §8) and `0` for the eager, bare client-streaming and bidi
OPENs.

### An OpenTelemetry-shaped handler

Nothing drpc-specific is needed. This is the standard shape — `TagRPC` creates
the per-call state, the later events find it on the context:

```go
// Recorder stands in for your metric instruments (an otel Float64Histogram,
// a prometheus.HistogramVec, ...); only the plumbing is shown.
type metrics struct{ m Recorder }

// callKey is the ctx key for per-call state. TagRPC is the only place a
// handler gets to create it; every later event carries that ctx back.
type callKey struct{}

type call struct {
    method   string
    begin    time.Time
    inBytes  atomic.Int64
    outBytes atomic.Int64
}

func (h *metrics) TagRPC(ctx context.Context, i *stats.RPCTagInfo) context.Context {
    return context.WithValue(ctx, callKey{}, &call{method: i.FullMethodName})
}

func (h *metrics) HandleRPC(ctx context.Context, s stats.RPCStats) {
    c, _ := ctx.Value(callKey{}).(*call)
    if c == nil {
        return
    }
    switch v := s.(type) {
    case *stats.Begin:
        c.begin = v.BeginTime
    case *stats.InPayload:
        c.inBytes.Add(int64(v.CompressedLength))
    case *stats.OutPayload:
        c.outBytes.Add(int64(v.CompressedLength))
    case *stats.End:
        code := status.Code(v.Error)
        h.m.Duration("rpc.duration", v.EndTime.Sub(c.begin), c.method, code)
        h.m.Bytes("rpc.sent", c.outBytes.Load(), c.method)
        h.m.Bytes("rpc.received", c.inBytes.Load(), c.method)
    }
}

func (h *metrics) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context {
    return ctx
}
func (h *metrics) HandleConn(context.Context, stats.ConnStats) {}
```

The `Client` field on every event says which side produced it, so one handler
can instrument both endpoints of a process (the examples run both in one).

## The dRPC surface: `ProtocolStats`

```go
type ProtocolStats interface {
    ProtocolEvent(ev ProtocolEvent)
}

type ProtocolEvent struct {
    Kind   ProtocolEventKind
    Peer   any    // the adapter's peer key; nil on a client
    Sid    uint32 // 0 for peer-scope events
    Method string // "" where the frame names no call
    Count  uint32 // magnitude where one exists
}
```

`Peer` is whatever key the adapter attached with `drpc.NewPeerContext`: a
`netip.AddrPort` for `transport/udp`, an opaque handle for the WebSocket,
WebRTC and message-port gateways. A `drpc.Conn` talks to exactly one peer, so client-side
events leave it nil. Peer-scope server events — keepalives, liveness expiry —
carry `Peer` with `Sid = 0` and no method; call-scope events carry all three.

`drpc.Counters` is the ready-made sink for applications that want the numbers
without writing a handler. The zero value is usable and `Snapshot()` reads it
atomically. `drpc.ProtocolStatsFunc` adapts a plain function.

### Every event

| Event | Cause | A rising rate means |
|---|---|---|
| `Skipped` | The seq window accepted a frame more than one step ahead: the frames in between are gone (§6.3). `Count` = how many. | Wire loss. This is the **only** way loss becomes visible at all — a gap raises no error, and the application sees an ordered subsequence (§14). |
| `Dropped` | The stream's rx buffer was full and the drop policy discarded a frame (§4.2). Unreliable mode only. | **Your consumer is behind, not the network.** The datagram arrived. Raise the buffer, speed up the consumer, or accept `DropOldest` freshest-wins semantics. |
| `OffShape` | The client received a server data frame on a call shape that has none (§8); on the server, `SetTrailer` was handed metadata that failed validation (§11). | A peer that is not speaking the protocol, or a handler writing invalid trailer keys. Never expected in steady state. |
| `ResetSent` | This endpoint disowned a call: a frame for an unknown sid, an OPEN a draining server refused (§9.3, §9.4). | State desync — usually a peer restart, or on raw UDP an injection (§15). The server event names the peer, which is what makes a RESET storm attributable. |
| `ResetReceived` | The peer disowned one of our calls; the call fails `UNAVAILABLE`. | The peer forgot us: it restarted, or our OPEN never landed and it is answering our data frames. |
| `Retransmit` | A control frame — an OPEN or an abort still owing an acknowledgement — was re-sent after RTI backoff (§10.3). | Control-frame loss. A lost *terminal* also shows up here, because it leaves the OPEN's retransmission obligation uncleared. |
| `ProbeSent` | A stream idle past `T_probe` (= `T_live`/3) was probed to recover a possibly-lost terminal (§10.5). | Mostly benign: long-lived idle streams cost one probe per `T_probe` each. Rising together with `Retransmit` means real loss. |
| `KeepaliveSent` | The peer link was idle past `T_probe`, so a `sid = 0` PING went out (§10.4). | Idle connections. Only interesting as the denominator for `LivenessExpired`. |
| `LivenessExpired` | No validated frame from the peer for `T_live`; every live call fails `UNAVAILABLE` and server handlers are cancelled (§10.4). | The peer vanished — process death, NAT rebinding, a black-holed path. Any non-zero value is a real outage for the calls that were live. |
| `TombstoneReplay` | A duplicate OPEN or straggler drew a stored terminal back out (§9.2). | The peer is not receiving our terminals, so it keeps retrying — or one peer is poking finished calls. The event names the peer and the sid. |
| `DataLoss` | `K_loud` (3) mutually consistent beyond-window frames arrived with no accepted frame between them: a loss burst wider than `W_fwd` (4096) frames. The call fails `DATA_LOSS` (§6.3). | A path that is dropping most of what you send — classically a PMTU black hole where small frames pass and large ones die. This is the one loss that is loud. |
| `FlowStall` | A send parked with no flow-control credit left (§4.2.1). Reliable mode only. | The receiving application is not draining its stream. Other calls on the channel keep flowing — that is the point — but this one is stopped. |
| `FlowResume` | A parked send got credit and continued. | Paired with `FlowStall`. **`FlowStall` without a matching `FlowResume` is a sender still parked right now**; after `T_stall` (default 30 s) the send fails `UNAVAILABLE`. |

Mode matters when reading these. `Skipped`, `Dropped` and `DataLoss` are
**unreliable mode only** — a reliable channel that loses or reorders a frame
fails the call with `INTERNAL` instead of counting anything (§10.6), which is
the correct behavior for a transport that promised not to. `FlowStall` and
`FlowResume` are **reliable mode only**, since that is the mode flow control
exists in. `Retransmit`, `ProbeSent`, `KeepaliveSent` and `LivenessExpired`
require the protocol timers, which reliable mode turns off; there, transport
death detection is the adapter's job.

One honest caveat on `Skipped`: it counts skipped **frames**, not messages.
Every frame of a direction consumes a seq — data frames, the header ack, the
terminal — so a lost `H` frame contributes 1 to `Skipped` even though no
application message was lost. On a server-streaming feed nearly every frame is
a message, so the count is the message loss in practice, which is how the
sensor example below uses it.

## Reading the counters: the sensor report

[`examples/udp-sensor`](../examples/udp-sensor) is the worked example: it
streams readings over UDP with 5% of outbound data frames deliberately
discarded, a consumer slower than the feed, and a 4-frame client buffer set to
`DropOldest` — so both kinds of loss happen at once. Because each `Reading`
carries its own `seq`, the application knows how many readings the handler
produced, and the counters say which kind of loss ate the difference:

```go
// printReport accounts for the readings that never arrived. produced comes
// from the application's own Reading.seq numbers; the counters say which
// kind of loss it was.
func printReport(produced, received uint64, c drpc.CounterSnapshot) {
    missing := produced - received
    // Every missing reading was either lost on the wire — which the seq
    // window saw as a gap and counted — or evicted from the rx buffer by
    // DropOldest, which is invisible to the window because the frame did
    // arrive.
    lost := min(c.Skipped, missing)
    evicted := missing - lost

    fmt.Printf("delivered  : %d of %d\n", received, produced)
    fmt.Printf("  wire loss: %d (Skipped, the §14 gap counter)\n", lost)
    fmt.Printf("  evicted  : %d (DropOldest; Dropped reports %d)\n", evicted, c.Dropped)
    fmt.Printf("recovery   : Retransmit %d  Probe %d  Keepalive %d\n",
        c.Retransmit, c.ProbeSent, c.KeepaliveSent)
    fmt.Printf("trouble    : DataLoss %d  LivenessExpired %d  Reset %d\n",
        c.DataLoss, c.LivenessExpired, c.ResetReceived)
}
```

One run of it, at the defaults:

```
readings produced   : 400 (seq 1..400)
readings delivered  : 244 (61.0%)
missing             : 156
  lost on the wire  : 23
  evicted, DropOldest: 133

drpc.Counters (client):
  Skipped 23  Dropped 133  DataLoss 0  OffShape 0
```

The example derives the eviction count arithmetically, from
`missing - Skipped`, because it wants to prove the two losses add up; you do
not have to. `Dropped` reports the same 133 directly.

The `Skipped` / `Dropped` split is the operationally important one, and the
two have opposite remedies. `Skipped` is the link: the datagram never arrived,
and no buffer size will bring it back — either accept the subsequence, or move
to a reliable transport. `Dropped` is you: the datagram *did* arrive and the
buffer was full, so raising `WithRxBuffer` / `WithMethodRxBuffer`, or making
the consumer faster, actually fixes it. Here, 133 of the 156 missing readings
were the consumer's own lag, not the 5% loss injector — a report that showed
only "missing: 156" would have sent you to look at the network.

Nothing else in the system can tell you this. The frames counted by `Dropped`
were accepted by the seq window, so they leave no gap; the frames counted by
`Skipped` never reached the buffer. Neither raises an error at the application
— that is the ordered-subsequence contract of §14, and these counters are the
whole of its visibility.

## Observers must not block

Both surfaces are called synchronously from the paths that must not stop: the
receive path (`Conn.Handle` / `Server.Handle`, which an adapter drives from
its read loop) and the timer sweep. A slow observer stalls the endpoint's
entire receive loop — every call on the channel, not just the one that emitted
the event — and on a reliable adapter that backpressure propagates all the way
into TCP. Do arithmetic and return. Anything that can block, allocate
unboundedly, take a contended lock, or do I/O belongs on the other side of a
buffered channel:

```go
events := make(chan drpc.ProtocolEvent, 1024)
go func() {
    for ev := range events {
        emit(ev) // export, log, whatever is slow
    }
}()

observer := drpc.ProtocolStatsFunc(func(ev drpc.ProtocolEvent) {
    select {
    case events <- ev:
    default:
        // The drain is behind. Lose the event, never the receive loop.
    }
})
```

`drpc.Counters` is safe by construction — it is a set of atomic adds. If all
you need is a counter per kind, route the event straight into your metrics
library's counter, using the kind's string form as a label:

```go
observer := drpc.ProtocolStatsFunc(func(ev drpc.ProtocolEvent) {
    n := uint64(max(ev.Count, 1))
    count("drpc.protocol.events", n, ev.Kind.String(), ev.Method)
})
```

`ProtocolEventKind.String()` yields the stable lower-case names used in the
table above (`skipped`, `dropped`, `off-shape`, `reset-sent`,
`reset-received`, `retransmit`, `probe-sent`, `keepalive-sent`,
`liveness-expired`, `tombstone-replay`, `data-loss`, `flow-stall`,
`flow-resume`). Note `Count`: `Counters` adds it for `Skipped`, `Dropped` and
`OffShape` and adds 1 for everything else, so a raw event handler that ignores
`Count` will under-report gaps.

## TypeScript

The TS port carries the dRPC half of this page and not the gRPC half:
`ProtocolStats` and `Counters` are in `@lesomnus/grpc-dgram` (`ts/src/stats.ts`),
installed with `protocolStats` on `ConnOptions` or `ServerOptions`; there is no
`stats.Handler` bridge, because that is a grpc-go type.

```ts
import { Conn, Counters } from '@lesomnus/grpc-dgram'

const counters = new Counters()
const conn = new Conn(transport, { protocolStats: counters.observe })

const stream = conn.newStream(Readings, {}) // a server-streaming MethodDesc
await stream.send(req)
for await (const reading of stream) { /* ... */ }
counters.snapshot().skipped // readings the wire ate (§14)
```

```ts
type ProtocolStats = (ev: ProtocolEvent) => void   // Go's ProtocolStatsFunc collapses into the type

interface ProtocolEvent {
  kind: ProtocolEventKind   // 'skipped' | 'dropped' | 'off-shape' | 'reset-sent' | … — the strings Go's String() fixes
  peer?: unknown            // FrameContext.peer; absent on a client
  sid: number               // 0 for peer-scope events
  method: string            // '' where the frame names no call
  count: number             // magnitude where one exists, else 0
}
```

(Every field is `readonly`: an observer sees a record, not a handle.)

Everything in [Every event](#every-event) holds verbatim: the same kinds, emitted
from the same decision points, with the same fields — `peer` on every
server-side event, `sid` and `method` on every call-scope one, `count` only for
`skipped`, `dropped` and `off-shape`. `protocolStats` accepts one observer or an
array, which is the TS spelling of "`WithProtocolStats` may be given more than
once". `Counters.observe` is an arrow property, so it can be handed over
unbound, and `snapshot()` returns a copy.

One input is TS-only: a `-bin` trailer value that is not base64 is dropped by
`setTrailer` and reported as `off-shape`, next to the invalid-key case Go
reports too. Go has no such input — its `-bin` values are raw bytes, and any
Go string is valid octets — while a JS string holding octets must be base64
(§11), so the check exists only here.

An observer that throws is contained: `emit` swallows it and the protocol's
step proceeds. The contract is Go's — do arithmetic and return — but the
consequence of breaking it is not a half-taken step (a liveness window that
expired and failed no call, a frame the window moved past and nobody received),
because the endpoint's correctness must not be one exception away from a
metrics hook.

The receiver that most needs this is the one this port runs in: on the lossy
path — a WebRTC data channel into a page — the browser is the receiving end of
a server-streaming call, so every gap happens there, and the Go server across
from it observes nothing about them. This is what lets a page tell 244 of 400
readings from 400 of 400.

## Limits

- **No tracing spans.** dRPC emits no spans of its own; a `stats.Handler` that
  builds them from `Begin`/`End` (as `otelgrpc` does) works, but there is no
  trace context propagated on the wire beyond the metadata you put there
  yourself (§11).
- **The TypeScript port has `ProtocolStats` but not `stats.Handler`.** The
  datagram-specific events — the §14 gap counter first among them — are
  emitted from the same decision points on both ends, so a browser client
  reports its own gaps (see [TypeScript](#typescript) above). The
  `stats.Handler` bridge is grpc-go's type and has no TS counterpart; a
  Connect-ES client has Connect's own instrumentation story.
- **Server off-shape frame drops are not all reported.** Data frames arriving
  on a server call shape that has none, and payload on an eager OPEN, are
  dropped into a per-stream counter without emitting `OffShape`; only the
  client side reports that case.
- **Counters are per endpoint, not per call.** `Counters` aggregates
  everything a `Conn` or `Server` sees. For per-call or per-peer breakdowns,
  implement `ProtocolStats` and key on `ev.Sid`, `ev.Method` and `ev.Peer`.
