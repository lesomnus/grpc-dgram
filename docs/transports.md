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
| [`ts/…/node-udp`](../ts/src/transport/node-udp) | UDP (Node) | unreliable | `new UdpTransport(...)`, `dialUdp` | `new UdpGateway(...)`, `listenUdp` |
| [`ts/…/websocket`](../ts/src/transport/websocket) | WebSocket | reliable | `new WebSocketTransport(ws)`, `dialWebSocket` | `new WebSocketGateway()` |
| [`ts/…/webrtc`](../ts/src/transport/webrtc) | WebRTC DataChannel | derived per channel | `new DataChannelTransport(dc)` | `new DataChannelGateway()` |

Two things under `ts/src/transport/` are **not** transports, despite living
there: [`protobuf-es`](../ts/src/transport/protobuf-es) derives method
descriptors from generated code, and [`connect`](../ts/src/transport/connect)
turns a `Conn` into a Connect-ES `Transport` so `createClient` works. They are
bindings; they sit next to the adapters because they are the same kind of
optional, dependency-carrying edge.

`transport/udp` is part of the core Go module (stdlib only). `gorilla` and
`pion` are separate modules, so importing the core never pulls their
dependencies.

## How an adapter decides "reliable"

`TransportInfo` is a one-method interface — `Reliable() bool` — discovered by
type assertion at construction (§4.3). WebSocket answers `true`
unconditionally; UDP answers `false`. WebRTC is the interesting one, because a
DataChannel is only reliable if it was configured that way:

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
WebRTC 1200 B unreliable / 16 KiB reliable, WebSocket unlimited; per-call caps
are gRPC's own 4 MiB receive and effectively unlimited send.

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
| udp | nothing — UDP is connectionless, so the core's timers (`T_call`, `T_live`) are the bound, and shutdown is the application's move |

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
per-channel mode and back-pressure.
