# gRPC-dgram

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
generated stubs ──> drpc.Conn ──(FrameHandler)──> adapter (1 Envelop per message) ──> channel
generated impls <── drpc.Server <──(per frame)──── adapter (unpacks Envelop) <──────── channel
```

- Package: `github.com/lesomnus/grpc-dgram` (import name `drpc`); the wire
  protocol is **dRPC**, specified in [`docs/PROTOCOL.md`](./docs/PROTOCOL.md)
- Status: **core + protocol complete and characterized** (unary / server- /
  client- / bidi-streaming, metadata, interceptors, codecs, timeouts,
  liveness), **transport adapters shipped** (UDP, WebSocket, pion/webrtc), and
  a **TypeScript port** of the same wire protocol ([`ts/`](./ts) — browser and
  Node, verified against a real Go server).

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
| Codecs (`grpc.ForceCodecV2`, `grpc.CallContentSubtype`) | ✅ (proto default; JSON etc. via call option) |
| Message compression (`grpc.UseCompressor`) | ✅ per message, stateless (gzip and any registered compressor) |
| Rich status details (`status.WithDetails`) | ✅ travel on the terminal frame (dropped only if it would not fit the channel) |
| Binary metadata (`-bin` keys) | ✅ arbitrary bytes, gRPC's own validation rules |
| Per-call size limits, `OnFinish`, `Peer`, `PerRPCCredentials`, `GetServiceInfo` (reflection) | ✅ gRPC-parity option surface |
| `stats.Handler` + dRPC protocol counters (gaps, drops, RESETs, stalls) | ✅ [`stats.go`](./stats.go) |
| Client & server deadlines | ✅ propagated on the wire, enforced both ends |
| Default timeout / liveness so lost frames never hang a call | ✅ core design (see [Guarantees](#guarantees)) |
| Reliable transports: loss machinery off, violations fail loud (`INTERNAL` on a seq gap/duplicate) | ✅ auto-detected per transport / per peer — see [Reliable transports](#reliable-transports) |
| Per-stream flow control on reliable channels (no head-of-line blocking) | ✅ HTTP/2-shaped windows, counted in messages |
| Per-stream buffering & drop policy (`DropNewest` / `DropOldest`) | ✅ per method / per role |
| Resource caps (tombstones, live calls, reset maps) | ✅ bounded under a junk flood |
| Transport adapters: UDP, WebSocket, pion/webrtc | ✅ [`transport/udp`](./transport/udp), [`transport/gorilla`](./transport/gorilla), [`transport/pion`](./transport/pion) |
| Browser / Node TypeScript port (client + server, same wire) | ✅ [`ts/`](./ts) — WebRTC DataChannel, WebSocket, Node UDP, protobuf-es & Connect-ES bindings |
| Runnable examples | ✅ [`examples/`](./examples) — UDP sensor stream, WebSocket echo, browser↔Go WebRTC |
| `Envelop` batching (`Coalescer`) | ⬜ planned |

## Install

```sh
go get github.com/lesomnus/grpc-dgram
```

Requires Go 1.26+.

## Usage

`Conn` implements `grpc.ClientConnInterface` and `Server` implements
`grpc.ServiceRegistrar`, so generated code plugs straight in. An adapter
bridges the core to an actual channel — over UDP (the sensor path):

```go
// Server
pc, _ := net.ListenUDP("udp", laddr)
gw := udp.NewGateway(pc)                 // github.com/lesomnus/grpc-dgram/transport/udp
srv := drpc.NewServer(gw)
pb.RegisterSensorServiceServer(srv, &myHandler{})
go gw.Serve(ctx, srv)

// Client: a drpc.Conn is a grpc.ClientConnInterface. The transport attaches
// itself (drpc.ConnAttacher) — no goroutine to manage, and one Close tears
// down the conn, the transport, and the socket.
c, _ := net.Dial("udp", serverAddr)
conn := drpc.NewConn(udp.New(c))
defer conn.Close(nil)
client := pb.NewSensorServiceClient(conn)

stream, _ := client.Readings(ctx, &pb.Subscribe{...})
for {
    r, err := stream.Recv()
    if err == io.EOF { break } // clean end of stream
    if err != nil { /* a gRPC status: DEADLINE_EXCEEDED, UNAVAILABLE, ... */ }
    use(r) // r may be an ordered *subsequence* of what was sent — see below
}
```

Three adapters ship. `transport/udp` is part of the core module (stdlib only);
`transport/gorilla` (gorilla/websocket) and `transport/pion` (pion/webrtc) live in
their own Go modules so importing the core never pulls their dependencies.

| | transport | mode | client | server |
|---|---|---|---|---|
| [`transport/udp`](./transport/udp) | UDP socket | unreliable | `udp.New(conn)` | `udp.NewGateway(pc)` + `Serve` |
| [`transport/gorilla`](./transport/gorilla) | WebSocket | reliable | `gorilla.New(wsc)` | `gorilla.NewGateway()` + `ServePeer` |
| [`transport/pion`](./transport/pion) | WebRTC DataChannel | **derived from the channel config** | `pion.New(dc)` | `pion.NewGateway()` + `Bind`+`ServePeer` |

Clients are gRPC-shaped: `drpc.NewConn(tp)` attaches the transport and its
receive machinery starts by itself; `conn.Close(nil)` (or the transport's
`Close`) tears everything down. Servers are gRPC-shaped too, in the other
direction: registration must precede the first received frame, so the
server transport starts explicitly after `RegisterService` — the
`Serve`/`ServePeer` calls above, the same shape as `grpc.Server.Serve(lis)`.

To wire a custom transport instead: the wire unit is one marshaled `Envelop`
(1..n `Frame`s) per transport message; implement `FrameHandler` (send) +
`TransportInfo` (+ `ConnAttacher` and `io.Closer` for the self-managing
client shape), feed received frames to `Conn.Handle`/`Server.Handle`, and
honor the teardown duty on connection-oriented channels. See
[`docs/PROTOCOL.md`](./docs/PROTOCOL.md) §3–§4 for the contract and any shipped
transport as a reference.

### Reliable transports

Over a reliable, ordered transport, timers and retransmission are off,
delivery is the exact sequence, and any gap or duplicate is surfaced as
`INTERNAL` (a broken "reliable" transport). A consumer that falls behind
stops its *own* producer instead of losing messages: each stream carries a
credit window (advertised on the OPEN and on the creation ack, refreshed as
the application consumes), so the sender parks and **other calls on the same
channel keep flowing** — the head-of-line blocking a single blocking receiver
would otherwise impose, and the reason gRPC has per-stream HTTP/2 windows.
This is the path to
**plain gRPC-over-WebSocket / reliable-datachannel** semantics, and it is
auto-detected with zero options: `transport/gorilla` always advertises
reliable, and `transport/pion` derives it from each data channel's
configuration (ordered, no retransmit/lifetime cap). On the server the mode
is **per peer**: a gateway annotates each channel's reliability
(`drpc.NewReliableContext`), so one `drpc.Server` serves a reliable control
channel and unreliable telemetry channels side by side — each in its own
mode. `WithReliable` remains as the explicit override for custom
transports. With no protocol timers running on a reliable channel, the
transport's death detection (keepalive, `OnClose`, send stall) is what
fails live calls — the shipped transports own that duty.

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
(`characterization_test.go`, `timeout_test.go`, `restart_test.go`,
`shutdown_test.go`).

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
on success and error, `Header()` blocking correctly — including a `SendHeader`
flush that returns before the response — interceptor chaining, metadata
round-trip including `-bin` keys, status details, per-call size limits,
`OnFinish`/`Peer`/`PerRPCCredentials`/`CallContentSubtype`, `GetServiceInfo`
for reflection, `stats.Handler`, and `Unimplemented` for unknown methods)
matches gRPC; the divergences are in
[Limitations & caveats](#limitations--caveats).

## Limitations & caveats

The honest list. Under the sensor use case most of these are *features*, not
bugs — a lost reading is superseded by the next.

- **Ordered subsequence, not the exact sequence.** Any dropped, reordered, or
  over-buffered data frame in unreliable mode is a silent gap with no error
  until the terminal — the skipped-message count is reported through the stats
  surface ([`docs/observability.md`](./docs/observability.md)). *Need
  every message?* Use `WithReliable(true)` over a reliable adapter, or make the
  stream idempotent/superseding.
- **At-most-once is per server incarnation.** A server restart (new epoch)
  mid-call can re-execute the handler once — dedup state died with the old
  instance. Make handlers idempotent if a cross-restart duplicate is
  unacceptable; within one incarnation execution is strictly at-most-once,
  and dedup survives even tombstone cap pressure (`PROTOCOL.md` §9.2).
- **No authentication — deploy encrypted.** The wire has no auth by design
  (`PROTOCOL.md` §15); `epoch` is a correctness device, not a security token.
  On **raw UDP**, anyone who can sniff a live `(epoch, sid, seq)` and inject
  datagrams can forge a RESET to kill a call or force a `DATA_LOSS`. Deploy
  over **DTLS / WSS / WebRTC** (all encrypted) and this is unreachable. Client
  streams reject foreign-epoch frames, so cross-incarnation poisoning is
  closed; same-epoch injection is the transport's job to prevent.
- **Status details are a passenger.** `code`, `message` and
  `status.WithDetails` payloads all travel — but details are the first thing
  dropped if the terminal frame would not fit the channel, because a lost
  terminal is what strands a call. Keep them small.
- **Best-effort, single-datagram messages.** No `WaitForReady`, no transparent
  retry, no load balancing — those need a connectivity model a datagram
  channel does not have. The transport's message-size limit is **your
  adapter's, not dRPC's**: the core is size-agnostic and never fragments; an
  unreliable adapter rejects a message that doesn't fit its datagram at send
  (`ResourceExhausted`), while a reliable transport carries any size. The
  per-call `MaxCallRecvMsgSize`/`MaxCallSendMsgSize` guards are a separate
  axis — they measure one message, the adapter measures the whole frame. Set
  explicit deadlines; keep messages within a datagram (natural for sensor
  readings).
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
reproduction of every gRPC lifecycle detail (see
[Limitations & caveats](#limitations--caveats)).

## Development

```sh
go test -race ./...     # core + transport/udp — fast & deterministic (testing/synctest)
# transport/gorilla and transport/pion are separate modules (their own go.mod),
# so ./... does not reach them; CI iterates over every go.mod the same way:
for d in transport/gorilla transport/pion; do (cd $d && go test -race ./...); done
buf generate            # regenerate protobuf bindings after editing proto/
go test -run '^$' -fuzz FuzzServerHandle -fuzztime 20s .   # fuzz the frame entry points
(cd ts && pnpm install && pnpm test)   # TypeScript port, incl. the Go↔TS conformance test
```

- Documentation: [`docs/`](./docs) — getting started, the transports, the two
  modes, gRPC compatibility, observability, and the TypeScript port
- Wire protocol & design rationale: [`docs/PROTOCOL.md`](./docs/PROTOCOL.md)
- Runnable examples: [`examples/`](./examples) — a UDP sensor stream with the
  gap/drop counters printed, a reliable WebSocket echo with graceful shutdown,
  and the browser↔Go WebRTC DataChannel demo
- TypeScript port (client + server, same wire): [`ts/`](./ts)
- Behavioral evidence for every guarantee/limitation above:
  [`characterization_test.go`](./characterization_test.go),
  [`timeout_test.go`](./timeout_test.go),
  [`restart_test.go`](./restart_test.go),
  [`shutdown_test.go`](./shutdown_test.go)
