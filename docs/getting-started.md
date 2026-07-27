# Getting started

`grpc-dgram` runs the gRPC programming model — `.proto` files, generated
stubs, streaming, metadata, deadlines, status codes — over datagram channels
instead of HTTP/2. It exists for one workload: frequently-produced messages
where a lost message is superseded by the next one (sensor telemetry, robot
or game state, live tracking), which is why a stream under loss degrades into
an ordered *subsequence* rather than stalling to retransmit a reading that is
already stale. Point the same code at a reliable channel — a WebSocket, an
ordered WebRTC data channel — and you get plain gRPC semantics, exact
sequence and all, without changing a line, because the mode is derived from
the transport rather than configured.

This guide gets one service running end to end over UDP.
[PROTOCOL.md](./PROTOCOL.md) is the normative wire specification; it is cited
here by section (PROTOCOL.md §4.2) wherever the reason for something lives
there, so you can read the rule instead of taking this page's word for it.

## Install

```sh
go get github.com/lesomnus/grpc-dgram
```

Go 1.26+. The package's import name is `drpc`. `transport/udp` is part of the
core module and uses only the standard library, so everything below needs
that one `go get`. So is `transport/jsport`, the JS message-port adapter —
`syscall/js` is stdlib too, and its `//go:build js && wasm` files simply do not
exist on any other GOOS.

The other two adapters are separate Go modules, so importing the core never
pulls their dependencies:

```sh
go get github.com/lesomnus/grpc-dgram/transport/gorilla   # WebSocket
go get github.com/lesomnus/grpc-dgram/transport/pion      # WebRTC DataChannel
```

Nothing is tagged yet, and those two modules reach the core through a
`replace` directive of their own. A `replace` is ignored by anyone who
*depends* on a module, so today the WebSocket and WebRTC adapters resolve only
from inside a checkout of this repository — where each module's own `replace`
is enough, no workspace file required — or through a `replace` you add
yourself. The release list in [TODO.md](./TODO.md) tracks the fix.

## A service

Nothing in the `.proto` is drpc-specific — ordinary proto3, one unary method
and one server-streaming method, built with the stock `protoc-gen-go` and
`protoc-gen-go-grpc`:

```proto
syntax = "proto3";

package sensor;

option go_package = "example.com/thermo/sensorpb";

service SensorService {
  // Describe is a plain unary RPC: one request, one response.
  rpc Describe(DescribeRequest) returns (Device) {}
  // Readings streams samples until the caller's deadline expires. A sensor
  // feed has no natural end, so the subscription is a time budget.
  rpc Readings(Subscribe) returns (stream Reading) {}
}

message DescribeRequest {}

message Device {
  string model = 1;
  uint32 max_hz = 2;
}

message Subscribe {
  uint32 hz = 1;
}

message Reading {
  // Monotonic sample number, starting at 1: what arrives is an ordered
  // subsequence, and seq is how the application sees which samples it lost.
  uint64 seq = 1;
  double celsius = 2;
}
```

A `seq` field on the streamed message is worth the four bytes here. The
protocol will not reorder or duplicate what it delivers, so a dense `seq` on
the receiving side means nothing was lost and a hole means something was —
the application can measure its own gaps without any protocol support.

## The server

The implementation is what `protoc-gen-go-grpc` asks for and nothing more:

```go
type sensor struct {
	sensorpb.UnimplementedSensorServiceServer
}

func (s *sensor) Describe(ctx context.Context, _ *sensorpb.DescribeRequest) (*sensorpb.Device, error) {
	return &sensorpb.Device{Model: "TMP-117", MaxHz: 500}, nil
}

func (s *sensor) Readings(req *sensorpb.Subscribe, stream grpc.ServerStreamingServer[sensorpb.Reading]) error {
	hz := req.GetHz()
	if hz == 0 {
		hz = 100
	}
	tick := time.NewTicker(time.Second / time.Duration(hz))
	defer tick.Stop()

	ctx := stream.Context()
	for seq := uint64(1); ; seq++ {
		select {
		case <-ctx.Done():
			// The client's deadline rode the OPEN frame and this server
			// enforced it on its own clock, without waiting for a frame
			// (PROTOCOL.md §10.2). For an endless feed that is the normal
			// ending, not a failure.
			return status.FromContextError(ctx.Err()).Err()
		case <-tick.C:
		}
		err := stream.Send(&sensorpb.Reading{
			Seq:     seq,
			Celsius: 20 + 5*math.Sin(float64(seq)/40),
		})
		if err != nil {
			return err
		}
	}
}
```

The wiring is the only part that knows about datagrams:

```go
func serve(ctx context.Context, addr string) error {
	laddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return err
	}
	pc, err := net.ListenUDP("udp", laddr)
	if err != nil {
		return err
	}
	defer pc.Close()

	gw := udp.NewGateway(pc)
	srv := drpc.NewServer(gw,
		// The feed gets a deep, freshest-wins buffer; Describe and every
		// other method keep the default 32/DropNewest (PROTOCOL.md §4.2).
		drpc.WithMethodRxBuffer(sensorpb.SensorService_Readings_FullMethodName, 64, drpc.DropOldest),
		// Bound the handler goroutines one peer can spawn (PROTOCOL.md §15).
		drpc.WithLimits(drpc.Limits{MaxLiveCalls: 64}),
	)
	sensorpb.RegisterSensorServiceServer(srv, &sensor{})

	// Only now: the registry freezes when serving starts, so registration
	// must precede the first received frame (PROTOCOL.md §13, §4.3).
	err = gw.Serve(ctx, srv)
	srv.GracefulStop()
	return err
}
```

`drpc.Server` implements `grpc.ServiceRegistrar`, so
`RegisterSensorServiceServer` is generated code called unchanged. One
listening socket serves many peers; each is keyed by its source address.

The ordering matters, and it is why the server transport is started
explicitly instead of attaching itself the way the client's does. The method
registry becomes immutable the moment `Handle` first runs (§13). An OPEN that
arrives before its service is registered does not queue and does not wait —
it resolves against an empty registry and draws `UNIMPLEMENTED`, which the
client sees as a failed call, not a retryable one. Starting the read pump
after `RegisterService` closes that window, and it is the same shape as
`grpc.Server.Serve(lis)`.

`gw.Serve` blocks until `ctx` is done or the socket is closed. `GracefulStop`
then drains in-flight handlers before returning, so anything a handler
accounted for is final once it does; `Stop` cancels them instead.

## The client

```go
func call(ctx context.Context, addr string) error {
	c, err := net.Dial("udp", addr)
	if err != nil {
		return err
	}

	counters := &drpc.Counters{}

	// NewConn attaches the transport (drpc.ConnAttacher): its receive pump
	// starts by itself, and one Close tears down the conn, the transport,
	// and the socket (PROTOCOL.md §4.3).
	conn := drpc.NewConn(udp.New(c),
		drpc.WithRxBuffer(16, drpc.DropOldest),
		drpc.WithProtocolStats(counters),
	)
	defer conn.Close(nil)

	client := sensorpb.NewSensorServiceClient(conn)

	dev, err := client.Describe(ctx, &sensorpb.DescribeRequest{})
	if err != nil {
		return err
	}
	fmt.Printf("%s, up to %d Hz\n", dev.GetModel(), dev.GetMaxHz())

	// A feed has no natural end, so the subscription is a time budget. The
	// deadline rides the OPEN frame and both ends enforce it independently.
	sctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	stream, err := client.Readings(sctx, &sensorpb.Subscribe{Hz: 200})
	if err != nil {
		return err
	}

	var got, last uint64
	for {
		r, err := stream.Recv()
		if errors.Is(err, io.EOF) || status.Code(err) == codes.DeadlineExceeded {
			break // the two normal endings for this call
		}
		if err != nil {
			return err
		}
		got++
		last = r.GetSeq()
	}

	s := counters.Snapshot()
	fmt.Printf("%d of %d readings; %d lost on the wire, %d dropped by the buffer\n",
		got, last, s.Skipped, s.Dropped)
	return nil
}
```

`drpc.Conn` implements `grpc.ClientConnInterface`, so
`NewSensorServiceClient(conn)` is again generated code unchanged.

The client is the mirror image of the server on lifecycle. `NewConn`
type-asserts the transport for `drpc.ConnAttacher` and hands it the endpoint,
so the receive pump starts on its own — there is no `Serve` to call and no
goroutine to own (§4.3). Teardown collapses the same way: `Conn.Close` fails
every live call with `UNAVAILABLE` and then closes a transport that
implements `io.Closer`, so the single `defer conn.Close(nil)` takes the conn,
the UDP transport, and the socket with it. It is idempotent, which is what
lets an adapter's own death path call back into it.

Both calls end on a deadline, but only one of them is yours. A unary call
whose context has no deadline gets `now + T_call` injected by the client
(5 s by default, §10.1) before the OPEN goes out, because on a datagram
channel a silent peer is indistinguishable from a lost response and something
has to bound the wait. Streaming calls get no implicit deadline — long-lived
streams are the point — so `Readings` has to say `context.WithTimeout`
itself. Either way the remaining budget travels on the OPEN frame and the
server enforces it on its own clock without waiting for a frame (§10.2),
which is why the recv loop above treats `DEADLINE_EXCEEDED` as a normal
ending next to `io.EOF`.

## Running it

Wire the two halves into a `main` (loopback, one process) and:

```
$ go run ./...
TMP-117, up to 500 Hz
399 of 399 readings; 0 lost on the wire, 0 dropped by the buffer
```

Loopback UDP does not drop datagrams, so this run is lossless and `seq` is
dense — worth knowing before a local test convinces you the loss handling is
untested. The shipped `examples/udp-sensor` throws away 5% of outbound data
frames on purpose for exactly this reason.

Over a real link the two counters separate, and the difference is
actionable:

| Counter | What happened | Where it comes from |
|---|---|---|
| `Skipped` | the datagram never arrived | the per-stream seq window, the §14 gap counter |
| `Dropped` | it arrived, but this consumer was behind and the rx buffer's drop policy discarded it | §4.2, per stream |

`Skipped` says the network is lossy; `Dropped` says your consumer is slower
than the producer, which is a buffer or a design problem, not a link problem.
Both are gaps, and gaps are the only distortion: what the application
receives is an ordered subsequence of what was sent — never reordered, never
duplicated (§14). `DropOldest` chooses which end of a full buffer to lose,
and freshest-wins is the right choice for a sensor feed precisely because the
newest reading supersedes the one it evicts.

## What changes on a reliable channel

Nothing in your code. The service, the handler, the generated stubs, the
recv loop, and every option above are unchanged; the constructor arguments
name a different adapter:

```go
// Server
gw := gorilla.NewGateway()
srv := drpc.NewServer(gw)
sensorpb.RegisterSensorServiceServer(srv, &sensor{})

up := websocket.Upgrader{}
http.HandleFunc("/rpc", func(w http.ResponseWriter, r *http.Request) {
	c, err := up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	// Blocks until the connection dies, then deregisters the peer and
	// fails its live calls (PROTOCOL.md §4.5).
	_ = gw.ServePeer(r.Context(), srv, c)
})

// Client
c, _, err := websocket.DefaultDialer.DialContext(ctx, url, nil)
if err != nil {
	return err
}
conn := drpc.NewConn(gorilla.New(c))
client := sensorpb.NewSensorServiceClient(conn)
```

There is no mode flag in either snippet because the mode is not a setting.
`NewConn`/`NewServer` type-assert the transport for `drpc.TransportInfo`
once, at construction (§4.3): the UDP adapter's `Reliable()` returns false,
the gorilla adapter's returns true, and the pion adapter derives it from each
data channel's own configuration (ordered, no retransmit or lifetime cap).
The reason it is discovered rather than configured is §10.6's
mode-agreement rule — both ends of a channel must land in the same mode, and
a channel knows its own reliability while a config file only knows what
someone typed.

What the derived mode changes is entirely inside the core:

| | unreliable | reliable |
|---|---|---|
| stream delivery | ordered subsequence, gaps allowed | the exact sequence |
| a gap or a duplicate | invisible, counted (§14) | fails the call with `INTERNAL` — a "reliable" transport that lost a frame is broken (§10.6) |
| protocol timers | on: `T_call`, `T_live`, `RTI`, probes, tombstones | off |
| a slow consumer | the rx buffer drops by policy | a per-stream credit window parks the *sender*; other calls on the channel keep flowing (§4.2.1) |
| what fails a live call when the peer vanishes | the core's own timers | only the adapter noticing (§4.5) |

That last row is the one to internalize. With the protocol timers off, an
adapter that fails to detect transport death leaves calls hanging forever —
nothing else can unblock them. The shipped adapters carry that duty
(keepalive timeout, `OnClose`, a stalled write, all routed into `Conn.Close`
/ `Server.DisconnectPeer`); a custom one must too.

`WithReliable(true)` overrides the derivation. It is for a custom transport
whose adapter does not implement `TransportInfo`, not a tuning knob — setting
it against the channel's actual behavior is how you get a `DATA_LOSS` or a
stalled call. One server can also serve both modes at once: a gateway with
channels of differing reliability annotates each peer's receive context with
`drpc.NewReliableContext`, and the server runs each peer in its channel's
mode.

## The shipped examples

Each is its own Go module with its own README, small enough to read in one
sitting, and its `.proto` bindings are committed — running them needs neither
`buf` nor `protoc`.

```sh
cd examples/udp-sensor     && go run ./...
cd examples/websocket-echo && go run ./...
cd examples/browser-webrtc && go run .     # after: cd ts && pnpm install && pnpm build
cd examples/browser-wasm   && go run .     # same prerequisite
```

- [`udp-sensor`](../examples/udp-sensor) — this guide's workload for real: an
  injected 5% data-frame loss, `WithMethodRxBuffer` + `DropOldest`, and a
  report that separates wire loss from buffer eviction. Its `lossy.go` shows
  how middleware sits in front of an adapter without masking `Reliable()`.
- [`websocket-echo`](../examples/websocket-echo) — the reliable path: timers
  off, every response in order, `GracefulStop` draining a live stream.
- [`browser-webrtc`](../examples/browser-webrtc) — a browser page on the
  TypeScript port calling a Go service over a WebRTC data channel.
- [`browser-wasm`](../examples/browser-wasm) — an ordinary gRPC service
  compiled to `js/wasm` and served *to the page it answers*, so reloading the
  browser restarts (and rebuilds) the server.

## Where to go next

- [PROTOCOL.md](./PROTOCOL.md) — the normative wire specification. Worth
  reading directly when you need the exact rule: §4 for the transport
  contract if you are writing an adapter, §10 for the timers and termination
  bounds, §14 for the delivery contract per RPC type, §15 for what the
  protocol does *not* protect you from.
- The adapter READMEs — [`transport/udp`](../transport/udp),
  [`transport/gorilla`](../transport/gorilla),
  [`transport/pion`](../transport/pion),
  [`transport/jsport`](../transport/jsport) — each documents its own options,
  message-size ceiling, and death-detection behavior. Those are per-adapter
  properties; the core has no opinion on them (§4.4).
  [Transports](./transports.md) compares them, and covers the TypeScript
  adapters that pair with them.
- [`ts/README.md`](../ts/README.md) — the TypeScript port (browser and Node,
  same wire, verified against a real Go server); [`ts/STATUS.md`](../ts/STATUS.md)
  for where it deliberately stops short of the Go feature set.
- The characterization suites — `characterization_test.go`, `timeout_test.go`,
  `restart_test.go`, `shutdown_test.go` — are the executable version of every
  guarantee in the README, and the tiebreaker when prose and code disagree.
- [TODO.md](./TODO.md) — what is deliberately not built yet, and the decision
  each item is waiting on.
- The other documents in this directory go deeper on individual features;
  this one is only the way in.
