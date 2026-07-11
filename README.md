# grpc-dgram

**gRPC programming model over unreliable datagram channels (UDP, WebRTC data
channels) — built for real-time sensor streams.**

`grpc-dgram` lets you keep your `.proto` files, your generated gRPC stubs, and
your handler code, and run them over a datagram transport instead of HTTP/2.
It is designed for one job well: **streaming frequently-produced messages where
a lost message is superseded by the next one** — sensor telemetry, game/robot
state, live tracking — with latency and graceful degradation prioritized over
perfect reliability.

It is **not** a general-purpose or reliable RPC framework, and it is **not**
wire-compatible with standard gRPC. See [What it is / isn't](#what-it-is--isnt).

```
generated stubs ──> drpc.Conn ──(FrameHandler)──> [Wrap1 | Coalescer] ──> adapter ──> channel
generated impls <── drpc.Server <──(per frame)──── adapter (unpacks Envelop) <──────── channel
```

- Package: `github.com/lesomnus/grpc-dgram` (import name `drpc`)
- Status: **core + protocol complete and characterized** (unary / server- /
  client- / bidi-streaming, metadata, interceptors, codecs, timeouts,
  liveness), **transport adapters shipped** (UDP, WebSocket, pion/webrtc).
  Wire protocol: [`PROTOCOL.md`](./PROTOCOL.md); milestones:
  [`ROADMAP.md`](./ROADMAP.md).

---

## Why

Standard gRPC needs HTTP/2, which needs a reliable ordered byte stream. A
sensor feed over UDP or an unreliable WebRTC data channel does not have one,
and does not want one: retransmitting a 20 ms-old reading just delays the
current one. `grpc-dgram` keeps the *gRPC programming model* — service
definitions, generated clients/servers, streaming, metadata, interceptors,
deadlines, status codes — and drops the parts that assume reliability, so the
same code runs over a lossy channel and degrades into an **ordered
subsequence** instead of stalling.

## Features

| | |
|---|---|
| Unary / Server- / Client- / Bidi-streaming | ✅ standard gRPC surface, generated stubs unchanged |
| Metadata (header / trailer) | ✅ on success and on error |
| Interceptors (unary + stream, client + server, chained) | ✅ |
| Codecs (`grpc.ForceCodecV2`) | ✅ (proto default; JSON etc. via call option) |
| Client & server deadlines | ✅ propagated on the wire, enforced both ends |
| Default timeout / liveness so lost frames never hang a call | ✅ core design (see [Guarantees](#guarantees)) |
| Reliable-mode (timers off over a reliable transport) | ✅ `WithReliable(true)` — the gRPC-over-WebSocket / reliable-datachannel path |
| Per-stream buffering & drop policy (`DropNewest` / `DropOldest`) | ✅ per method / per role |
| Resource caps (tombstones, live calls, reset maps) | ✅ bounded under a junk flood |
| Transport adapters: UDP, WebSocket, pion/webrtc | ✅ [`adapter/udp`](./adapter/udp), [`adapter/ws`](./adapter/ws), [`adapter/pion`](./adapter/pion) |
| Stats handler, browser JS/TS port | ⬜ planned |

## Install

```sh
go get github.com/lesomnus/grpc-dgram
```

Requires Go 1.25+ (`testing/synctest` is used by the test suite).

## Usage

`Conn` implements `grpc.ClientConnInterface` and `Server` implements
`grpc.ServiceRegistrar`, so generated code plugs straight in. An adapter
bridges the core to an actual channel — over UDP (the sensor path):

```go
// Server
pc, _ := net.ListenUDP("udp", laddr)
gw := udp.NewGateway(pc)                 // github.com/lesomnus/grpc-dgram/adapter/udp
srv := drpc.NewServer(gw)
pb.RegisterSensorServiceServer(srv, &myHandler{})
go gw.Serve(ctx, srv)

// Client: a drpc.Conn is a grpc.ClientConnInterface.
c, _ := net.Dial("udp", serverAddr)
tp := udp.New(c)
conn := drpc.NewConn(tp)
go tp.Serve(ctx, conn)
client := pb.NewSensorServiceClient(conn)

stream, _ := client.Readings(ctx, &pb.Subscribe{...})
for {
    r, err := stream.Recv()
    if err == io.EOF { break } // clean end of stream
    if err != nil { /* a gRPC status: DEADLINE_EXCEEDED, UNAVAILABLE, ... */ }
    use(r) // r may be an ordered *subsequence* of what was sent — see below
}
```

Three adapters ship. `adapter/udp` is part of the core module (stdlib only);
`adapter/ws` (gorilla/websocket) and `adapter/pion` (pion/webrtc) live in
their own Go modules so importing the core never pulls their dependencies.

| | transport | mode | wiring |
|---|---|---|---|
| [`adapter/udp`](./adapter/udp) | UDP socket | unreliable | `udp.New(conn)` / `udp.NewGateway(pc)` + `Serve` |
| [`adapter/ws`](./adapter/ws) | WebSocket | reliable | `ws.New(wsc)` + `ServeConn` / `ws.NewGateway()` + `ServePeer` |
| [`adapter/pion`](./adapter/pion) | WebRTC DataChannel | **derived from the channel config** | `pion.New(dc)` + `ServeConn` / `pion.NewGateway()` + `Bind`+`ServePeer` |

To wire a custom transport instead: the wire unit is one marshaled `Envelop`
(1..n `Frame`s) per transport message; implement `FrameHandler` (send) +
`TransportInfo`, feed received frames to `Conn.Handle`/`Server.Handle`, and
honor the teardown duty on connection-oriented channels. See
[`PROTOCOL.md`](./PROTOCOL.md) §3–§4 for the contract and any shipped adapter
as a reference.

### Reliable transports

Over a reliable, ordered transport, timers and retransmission are off,
delivery is the exact sequence, and any gap or duplicate is surfaced as
`INTERNAL` (a broken "reliable" transport). This is the path to **plain
gRPC-over-WebSocket / reliable-datachannel** semantics, and it is
auto-detected: `adapter/ws` always advertises reliable, `adapter/pion`
derives it from the data-channel configuration (ordered, no
retransmit/lifetime cap), and both ends of a pion channel derive the same
answer with zero options. `WithReliable` remains as the explicit override for
custom transports. With no protocol timers running, the adapter's death
detection (keepalive, `OnClose`) is what fails live calls — the shipped
adapters own that duty.

### Tuning (sensor streams)

```go
drpc.NewServer(tx,
    // Keep the freshest readings when a consumer lags: evict the oldest.
    drpc.WithMethodRxBuffer("/sensor.SensorService/Readings", 64, drpc.DropOldest),
    // Bound handler goroutines a single peer can spawn.
    drpc.WithLimits(drpc.Limits{MaxLiveCalls: 256}),
)

drpc.NewConn(tx,
    drpc.WithTiming(drpc.Timing{Call: 2*time.Second, Liveness: 10*time.Second}),
)
```

---

## Guarantees

Over an unreliable datagram channel, `grpc-dgram` keeps standard gRPC
code-generation and call semantics while degrading gracefully under loss,
reorder, and duplication. Every item below is pinned by an executable test
(`characterization_test.go`, `timeout_test.go`).

- **Ordered, de-duplicated delivery** — what the app receives is an ordered
  *subsequence* of what was sent: never reordered, never duplicated. Gaps are
  the only distortion; a one-step network reorder surfaces as a gap, not an
  out-of-order message.
- **Exactly one terminal per call** — every call ends with exactly one
  outcome: a value, a gRPC status, or `io.EOF`. End-of-stream is the terminal
  frame, never inferred from silence; a lost terminal is recovered by a probe
  or retransmit hitting the server's tombstone.
- **At-most-once execution per server incarnation** — a handler runs at most
  once even under duplicated or stale OPENs (sid dedup + tombstone replay +
  aged watermark). The *response* is delivered at-least-once; the *execution*
  is deduplicated.
- **Bounded termination** — no call hangs forever. A deadline-less unary fails
  within `T_call`; a broken stream within `T_live`; a lost control frame
  recovers within an RTI-backoff round; an idle stream recovers a lost
  terminal within `T_probe + RTI`. Every path has a stated ceiling.
- **Deadlines enforced on both ends** — the client deadline travels on the
  OPEN; the server enforces `DEADLINE_EXCEEDED` independently, never waiting
  for a frame. `WithMaxHandlerTimeout` clamps a client-asserted deadline.
- **Incarnation isolation** — calls are keyed by `(peer, epoch, sid)` on the
  server; each client stream locks to one server incarnation. A wrong-epoch
  frame — dead incarnation or stale straggler — cannot touch a live call.
- **Bounded state under garbage** — tombstones, live calls, and RESET/PING
  rate-limit maps are all capped; a junk flood costs bounded CPU and zero
  unbounded memory, and cannot keep a vanished peer's liveness alive.
- **Handlers never wedge** — a vanished client cannot pin a server handler:
  liveness expiry cancels it within `T_live`. `Stop`, `GracefulStop`, and
  `DisconnectPeer` drain handlers deterministically.

The standard gRPC surface (value+status on all four RPC types, header/trailer
on success and error, `Header()` blocking correctly, interceptor chaining,
metadata round-trip, `Unimplemented` for unknown methods) matches gRPC; see
the divergences below.

## Limitations & caveats

The honest list. Under the sensor use case most of these are *features*, not
bugs — a lost reading is superseded by the next.

- **Ordered subsequence, not the exact sequence.** Any dropped, reordered, or
  over-buffered data frame in unreliable mode is a silent gap with no error
  until the terminal (a skipped-count is exposed via stats, planned). *Need
  every message?* Use `WithReliable(true)` over a reliable adapter, or make the
  stream idempotent/superseding.
- **At-most-once is per server incarnation.** A server restart (new epoch)
  mid-call, or a tombstone evicted under cap pressure before the client stops
  retrying, can re-execute the handler once. Make handlers idempotent if a
  cross-restart duplicate is unacceptable; within one incarnation execution is
  strictly at-most-once.
- **No authentication — deploy encrypted.** The wire has no auth by design
  (`PROTOCOL.md` §15); `epoch` is a correctness device, not a security token.
  On **raw UDP**, anyone who can sniff a live `(epoch, sid, seq)` and inject
  datagrams can forge a RESET to kill a call or force a `DATA_LOSS`. Deploy
  over **DTLS / WSS / WebRTC** (all encrypted) and this is unreachable. Client
  streams reject foreign-epoch frames, so cross-incarnation poisoning is
  closed; same-epoch injection is the transport's job to prevent.
- **Status details are dropped.** `code` and `message` travel; a
  `status.WithDetails` payload does not (the frame carries only `code`+`desc`).
  Put needed detail in the message string or in trailer metadata (which does
  travel).
- **Best-effort, single-datagram messages.** No `WaitForReady`, no transparent
  retry. The message-size limit is **your transport's, not drpc's**: the core
  is size-agnostic and never fragments; an unreliable adapter rejects a
  message that doesn't fit its datagram at send (`ResourceExhausted`), while a
  reliable transport carries any size. Batched frames in one datagram
  fate-share. Set explicit deadlines; keep messages within a datagram
  (natural for sensor readings).
- **Not HTTP/2 gRPC.** No wire-compatibility with standard gRPC, proxies, or
  the HTTP/2 ecosystem. If you need interop with existing gRPC infrastructure,
  this is the wrong transport.

## What it is / isn't

**Is:** a transport/runtime for message-based RPC over datagram-style channels,
a compatibility layer around gRPC-generated interfaces (`grpc.ClientConnInterface`,
`grpc.ServiceRegistrar`, generated service/client code), tuned for real-time
streams that tolerate loss.

**Isn't:** HTTP/2-based gRPC; a general-purpose reliability layer over an
unreliable channel; wire-compatible with standard gRPC; a faithful
reproduction of every gRPC lifecycle detail (see divergences above).

## Development

```sh
go test -race ./...     # fast & deterministic — timing tests use testing/synctest
buf generate            # regenerate protobuf bindings after editing proto/
go test -run '^$' -fuzz FuzzServerHandle -fuzztime 20s .   # fuzz the frame entry points
```

- Wire protocol & design rationale: [`PROTOCOL.md`](./PROTOCOL.md)
- Milestones & status: [`ROADMAP.md`](./ROADMAP.md)
- Behavioral evidence for every guarantee/limitation above:
  [`characterization_test.go`](./characterization_test.go)
