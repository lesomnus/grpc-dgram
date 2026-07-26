# Transports

The core of gRPC-dgram never touches a socket. It emits and consumes
`Frame`s; an **adapter** turns those into whatever the channel carries, and
tells the core one thing about that channel: whether it loses, duplicates or
reorders messages. Everything else the protocol does follows from that answer
([unreliable-mode.md](./unreliable-mode.md),
[reliable-mode.md](./reliable-mode.md)).

This page is for two readers: someone choosing an adapter, and someone writing
one. The normative contract is [PROTOCOL.md](./PROTOCOL.md) §3–§4.

## What ships

| adapter | channel | mode | client | server |
|---|---|---|---|---|
| [`transport/udp`](../transport/udp) | UDP socket | unreliable | `udp.New(conn)` | `udp.NewGateway(pc)` + `Serve` |
| [`transport/gorilla`](../transport/gorilla) | WebSocket | reliable | `gorilla.New(wsc)` | `gorilla.NewGateway()` + `ServePeer` |
| [`transport/pion`](../transport/pion) | WebRTC DataChannel | derived per channel | `pion.New(dc)` | `pion.NewGateway()` + `Bind` + `ServePeer` |
| [`transport/jsport`](../transport/jsport) | JS message port (`js/wasm`) | reliable | `jsport.New(port)` | `jsport.NewGateway()` + `Serve` |
| [`ts/…/node-udp`](../ts/src/transport/node-udp) | UDP (Node) | unreliable | `new UdpTransport(...)`, `dialUdp` | `new UdpGateway(...)`, `listenUdp` |
| [`ts/…/websocket`](../ts/src/transport/websocket) | WebSocket | reliable | `new WebSocketTransport(ws)`, `dialWebSocket` | `new WebSocketGateway()` |
| [`ts/…/webrtc`](../ts/src/transport/webrtc) | WebRTC DataChannel | derived per channel | `new DataChannelTransport(dc)` | `new DataChannelGateway()` |
| [`ts/…/port`](../ts/src/transport/port) | JS message port | reliable | `new PortTransport(port)`, `startWasmServer` | `new PortGateway()` + `bind` + `servePeer` |

Two things under `ts/src/transport/` are **not** transports, despite living
there: [`protobuf-es`](../ts/src/transport/protobuf-es) derives method
descriptors from generated code, and [`connect`](../ts/src/transport/connect)
turns a `Conn` into a Connect-ES `Transport` so `createClient` works. They are
bindings; they sit next to the adapters because they are the same kind of
optional, dependency-carrying edge.

`transport/jsport` and `ts/…/port` are one channel seen from both sides. A
**message port** is anything with `postMessage` and a `message` event — either
end of a `MessageChannel`, a `Worker`, a worker's own global scope — and the
two adapters carry the WebSocket wire on it byte for byte, so a Go server
compiled to `js/wasm` and running *inside the page* is a peer like any other.
That is the deployment they exist for;
[`examples/browser-wasm`](../examples/browser-wasm) is it, running.

For that one pairing the wiring is two lines: `gw.Serve(ctx, srv)` publishes a
JS entry point — the publish being the readiness signal — and serves every port
handed to it, while `await startWasmServer('/app.wasm')` on the page waits for
that publish, makes the `MessageChannel` and returns the transport over its
end. One instance serves as many connections as it is given ports, each its own
peer (§6.4); `wasmServer().connect()` opens the next one. Any other shape — a
`Worker`, an iframe, two TS endpoints — is the manual path below, unchanged.

`transport/udp` and `transport/jsport` are part of the core Go module (stdlib
only — `jsport` needs nothing but `syscall/js`, and being `//go:build js &&
wasm` it is silently skipped by `go build ./...` on every other GOOS).
`gorilla` and `pion` are separate modules, so importing the core never pulls
their dependencies.

## How an adapter decides "reliable"

`TransportInfo` is a one-method interface — `Reliable() bool` — discovered by
type assertion at construction (§4.3). WebSocket and a message port answer
`true` unconditionally; UDP answers `false`. WebRTC is the interesting one,
because a DataChannel is only reliable if it was configured that way:

```go
// transport/pion/channel.go
func channelReliable(dc *webrtc.DataChannel) bool {
	return dc.Ordered() && dc.MaxRetransmits() == nil && dc.MaxPacketLifeTime() == nil
}
```

Both ends evaluate the same DCEP-negotiated configuration, so they reach the
same answer without exchanging anything — which is what §10.6's "both ends of a
channel must agree" needs. Get that wrong and the failure is concrete, not
graceful: see [reliable-mode.md](./reliable-mode.md#both-ends-of-a-channel-must-agree).

For a transport the core cannot ask, `drpc.WithReliable(true|false)` sets it
explicitly. Apply it at **both** ends.

### One server, mixed channels

Reliability is a property of a channel, and one WebRTC peer connection can
carry a reliable control channel next to an unreliable telemetry channel. A
gateway therefore annotates each frame's receive context instead of answering
for the whole endpoint:

```go
ctx = drpc.NewPeerContext(ctx, key)          // who sent it (§6.4)
ctx = drpc.NewReliableContext(ctx, reliable) // which mode it runs in (§4.3)
```

The server then runs **each peer in its channel's mode** — strict sequencing
and no timers for the reliable one, the full retransmission machinery for the
other — with one `drpc.Server` and one registry. The annotation must be
constant for a peer; it is captured once, when that peer's state is created.

## Message size: two different ceilings

They are often confused, and they measure different things.

| | what it measures | who owns it | what happens past it |
|---|---|---|---|
| adapter ceiling | the whole marshaled `Envelop` — frame header, method string, metadata, framing | the adapter (§4.4) | the send is refused synchronously; the core maps it to `ResourceExhausted` on that call |
| `MaxCallSendMsgSize` / `MaxCallRecvMsgSize` | one message, after compression on the way out and after decompression on the way in | the application (gRPC parity) | the call fails `ResourceExhausted` |

Neither implies the other. A 1200-byte send cap still overflows a 1200-byte
datagram budget once the frame around it is counted. Defaults: UDP 1200 B,
WebRTC 1200 B unreliable / 16 KiB reliable, WebSocket and message port
unlimited — neither channel imposes a ceiling of its own, so the adapter
invents none; per-call caps are gRPC's own 4 MiB receive and effectively
unlimited send.

The core never fragments and never asks for an MTU. On an unreliable channel
that is deliberate: reassembly over a lossy link would rebuild the reliability
layer this protocol exists to avoid.

## Writing your own

Four seams, in the order you will meet them.

**1. Send — `FrameHandler`.** The core hands you frames; you put them on the
wire. The wire unit is one marshaled `Envelop` per transport message, so the
usual shape is `drpc.Wrap1` plus your own `EnvelopHandler`, or a single type
that does both:

```go
func (t *Transport) Handle(ctx context.Context, f *drpc.Frame) error {
	e := &drpc.Envelop{}
	e.SetFrames([]*drpc.Frame{f})
	return t.Send(ctx, e)
}
```

Refuse an oversize message **synchronously**, wrapping `drpc.ErrMessageTooLarge`
— that sentinel is how the core knows to fail the owning call with
`ResourceExhausted` instead of treating the error as fatal.

Note that `Wrap1` returns a bare `FrameHandler` and re-exposes nothing: a
transport wrapped in it loses `TransportInfo` discovery. Implement both on one
type, or annotate per peer.

**2. Receive — `Conn.Handle` / `Server.Handle`.** Unmarshal one `Envelop`, then
deliver its frames **in order** (`drpc.Unpack` does this), with the peer
attached to the context. What you may do next depends on the mode:

- unreliable: `Handle` must not block. A full stream buffer drops a frame by
  policy, exactly as if the network had lost it (§4.2).
- reliable: `Handle` may block, and adapters *should* call it synchronously
  from the read loop so the stall propagates into TCP/SCTP flow control — but
  once per-stream flow control is active it will not block, because a
  conforming peer cannot overrun its window (§4.2.1).

A non-nil return means "malformed input", not "tear down the channel". Dropped,
stray and duplicate frames are normal here and return `nil`.

**3. Lifecycle — `ConnAttacher`, `io.Closer`, `TransportPeer`.** A client
transport that implements `AttachConn(*drpc.Conn)` receives its endpoint at
construction and starts its own receive machinery, so the application manages
no goroutine; `Conn.Close` then closes a transport that is an `io.Closer`, and
one `Close` tears down conn, transport and socket. Servers deliberately have no
equivalent — registration must precede the first received frame, so serving
starts explicitly after `RegisterService`, the shape of `grpc.Server.Serve`.

`TransportPeer` (`Peer() *peer.Peer`) is optional and worth implementing: it is
what makes `grpc.Peer(&p)` and `peer.FromContext` report a real address. A
gateway with opaque per-connection keys attaches a `*peer.Peer` to the receive
context instead, next to `NewPeerContext`.

**4. Teardown — the one duty you cannot skip.** A connection-oriented adapter
MUST detect transport death and call `Conn.Close(err)` or
`Server.DisconnectPeer(peer, err)`, which fails live calls with `UNAVAILABLE`
and cancels handler contexts (§4.5). In reliable mode there are no protocol
timers, so this is the *only* thing that ever unblocks a call whose peer
vanished.

The subtle part: detection must not depend on the read loop making progress. If
delivery is synchronous, the read loop is exactly what blocks under
back-pressure, so a death signal that lives only there — a read error, a read
deadline — can never fire when it is needed. The shipped adapters use an
out-of-band signal:

| adapter | what notices death |
|---|---|
| gorilla | keepalive ping write failure, plus a write deadline on every send |
| pion | `OnClose`/`OnError` callbacks and a send-stall timeout at the buffered-amount mark |
| ts websocket | `onclose`/`onerror` plus a keepalive |
| jsport, ts port | nothing the channel reports — the peer's goodbye, or an explicit `Close`/`close(cause)` from the host (below) |
| udp | nothing — UDP is connectionless, so the core's timers (`T_call`, `T_live`) are the bound, and shutdown is the application's move |

### When there is nothing to detect

A message port is the case that row cannot answer. Both endpoints live in one
process: there is no socket to break, no error event that has to arrive, and
nothing for a keepalive to measure — two things sharing an event loop cannot be
partitioned, so an unanswered ping would only report how busy the peer is.
Death is therefore not detected here. It is **said out loud**, by two
mechanisms that between them are the whole of the §4.5 duty on this channel.

The first is on the wire: **a 0-byte message is the goodbye**. It is a
marshaled `Envelop` with no frames — something §4.1 (1..n) means the wire never
otherwise carries — so `Close` posts one before it drops the port, and the peer
that reads it tears down. The trigger is the *byte length*, never "the envelop
decoded to no frames" — protobuf keeps fields it does not know, so a later wire
version, or another library's traffic on the same port, decodes to no frames as
well, and reading one of those as a goodbye would kill a healthy channel over
input §4.2 says to drop.

The second is the host's, because the port cannot report what only the host
knows: a wasm instance that exited or panicked, a terminated worker, a page
unloading. Both adapters take an explicit close for it — `Transport.Close` /
`Gateway.Close` in Go, `close(cause)` in TypeScript. The TS close carries the
host's cause into the failed calls; Go's `Close` is an `io.Closer` and takes
none, so a Go host with something to say calls the core's own teardown API
directly — `conn.Close(err)`, `srv.DisconnectPeer(peer, err)` — which is what
the adapter's `Close` would otherwise trigger with no cause. For the wasm page
`startWasmServer` wires that close to `go.run()`'s own promise already, which
is what keeps a panicking in-page server from hanging its UI
([`examples/browser-wasm`](../examples/browser-wasm)); a host on the manual
path owes the same wiring itself. An endpoint that vanishes without
either signal leaves its peer's calls to their deadlines: with timers off,
nothing else ever fails them.

Backpressure gets the browser DataChannel's answer. `postMessage` applies none,
so received messages queue in the adapter — bounded not by the channel but by
the protocol, since a conforming peer in reliable mode cannot put more in
flight than the per-stream windows it was granted (§4.2.1). Never drop one to
make room: a gap in reliable mode is a protocol error, not a lost datagram.

## Checklist

An adapter is done when it: preserves message boundaries; carries one
`Envelop` per message; answers `Reliable()` truthfully (or annotates per peer);
refuses oversize sends synchronously with `ErrMessageTooLarge`; delivers frames
in order with the peer attached; never tears down the channel on a frame-level
error; and calls the core's teardown API from a signal that is independent of
the read loop.

The shipped adapters are the reference — [`transport/udp`](../transport/udp) is
the smallest, [`transport/gorilla`](../transport/gorilla) shows the
keepalive/teardown pattern, [`transport/pion`](../transport/pion) shows
per-channel mode and back-pressure, and
[`transport/jsport`](../transport/jsport) shows teardown on a channel that
cannot report its own death. Each of the TypeScript adapters
([`ts/src/transport/`](../ts/src/transport)) is the same contract in the other
language, with a README next to it.
