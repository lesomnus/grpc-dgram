# udp-sensor

The job `grpc-dgram` exists for: **a server-streaming sensor feed over UDP**,
subscribed to with an explicit deadline, degrading into an *ordered
subsequence* when datagrams go missing — and a report at the end of exactly
what went missing and why.

```sh
go run ./...
```

Server and client run in one process over a loopback UDP socket. Two processes
work too:

```sh
go run ./... -serve 127.0.0.1:9000       # terminal 1
go run ./... -connect 127.0.0.1:9000     # terminal 2
```

## What it demonstrates

- **Generated stubs, unchanged.** `sensorpb` is ordinary `protoc-gen-go` /
  `protoc-gen-go-grpc` output (`proto/sensor.proto`). The handler is a normal
  `grpc.ServerStreamingServer[Reading]`; the client is a normal generated
  client. Only the two constructor lines know about datagrams:
  `drpc.NewServer(gw)` and `drpc.NewConn(udp.New(c))`.
- **`WithMethodRxBuffer` + `DropOldest`** (`server.go`) — the feed's method gets
  a deep, freshest-wins receive buffer while every other method keeps the
  default (`PROTOCOL.md` §4.2). The client-side twin is `WithRxBuffer`
  (`client.go`), and that is where the policy bites in this demo: the consumer
  is deliberately slower than the feed, so its 4-frame buffer evicts the
  *oldest* reading to admit the newest.
- **An explicit deadline.** A sensor feed has no natural end, so the
  subscription is a time budget (`-for`, default 2s). It rides the OPEN frame,
  and both ends enforce it on their own clocks — the client's `Recv` ends with
  `DEADLINE_EXCEEDED`, the server's handler context is cancelled without
  waiting for a frame (§10.2).
- **Loss, made visible.** Loopback UDP loses nothing, so `lossy.go` throws away
  5% of outbound *data* frames (`-loss`) the way a real link would. Control
  frames pass through untouched.
- **A counter report** from `drpc.Counters`, the ready-made
  `drpc.ProtocolStats` implementation, next to the application's own accounting
  of the `Reading.seq` numbers it received.

## Reading the report

```
--- subscription report (2.031s) ---
  stream ended        : rpc error: code = DeadlineExceeded desc = context deadline exceeded
  readings produced   : 400 (seq 1..400)
  readings delivered  : 243 (60.8%)
  missing             : 157
    lost on the wire  : 16 (the §14 gap counter)
    evicted, DropOldest: 141 (rx buffer full while this consumer lagged)
  longest single gap  : 2 readings
  out of order        : 0, by construction — gaps are the only distortion

drpc.Counters (client):
  Skipped 16  Dropped 141  DataLoss 0  OffShape 0
  ...
```

Two different losses, and the difference is the point:

| | where | counted by |
|---|---|---|
| **lost on the wire** | the datagram never arrived | `Counters.Skipped` — the §14 gap counter, taken from the seq window |
| **evicted** | the datagram arrived, but the consumer was behind and `DropOldest` made room | `Counters.Dropped` — the rx buffer's own count |

Both are *gaps*, never reordering or duplication: what the application receives
is an ordered subsequence of what the handler sent. That is the whole contract
on an unreliable channel, and `Reading.seq` lets an application see it directly.

> The two counters are independent measurements that happen to agree: `Skipped`
> comes from the seq window (a frame that never arrived), `Dropped` from the rx
> buffer (a frame that arrived and was evicted). An eviction leaves no gap —
> the window already accepted it — which is why one counter cannot substitute
> for the other.

## Flags

| Flag | Default | |
|---|---|---|
| `-hz` | 200 | sample rate the sensor produces at |
| `-for` | 2s | the subscription's deadline |
| `-consume` | 8ms | time the client spends on each reading; slower than `1/hz` is what fills the rx buffer |
| `-rx-buffer` | 4 | client rx buffer, in frames |
| `-loss` | 0.05 | fraction of outbound data frames the server drops |
| `-serve` / `-connect` | — | run one half only |

Try `-loss 0` (all missing readings are evictions), `-consume 1ms` (no
evictions, only wire loss), or `-rx-buffer 64` (the buffer absorbs the lag).

## Files

| | |
|---|---|
| `main.go` | flags and the one/two-process wiring |
| `server.go` | the handler, `drpc.NewServer` options, `udp.NewGateway` + `Serve` |
| `client.go` | `drpc.NewConn` + `udp.New`, the deadline, the recv loop, the report |
| `lossy.go` | the loss injector: a `drpc.FrameHandler` in front of the gateway |
| `proto/sensor.proto`, `sensorpb/` | the service and its checked-in bindings |

Regenerate the bindings (needs [buf](https://buf.build); the generated files
are committed, so running the example does not):

```sh
buf generate examples/udp-sensor/proto --template examples/udp-sensor/buf.gen.yaml   # from the repo root
```
