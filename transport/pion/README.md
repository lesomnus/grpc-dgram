# transport/pion

drpc over WebRTC DataChannels ([pion/webrtc](https://github.com/pion/webrtc)):
**one channel message carries one marshaled `Envelop`**, and the protocol
mode is derived from the channel's own configuration:

| channel config | mode |
|---|---|
| ordered, no retransmit/lifetime cap (the default) | **reliable** — timers off, plain gRPC semantics |
| anything else (`Ordered: false`, `MaxRetransmits`, `MaxPacketLifeTime`) | **unreliable** — full timer machinery, sensor semantics |

Same adapter, mode decided by the channel; both ends derive the same answer
because DCEP propagates the parameters. Signaling and `PeerConnection` setup
stay with your application — the adapter takes an already-negotiated
`*webrtc.DataChannel`.

Its own Go module: importing the core never pulls pion.

```go
import "github.com/lesomnus/grpc-dgram/transport/pion"
```

## Client (the side that creates the channel)

```go
dc, err := pc.CreateDataChannel("rpc", nil) // nil config = reliable
if err != nil { ... }

conn := drpc.NewConn(pion.New(dc)) // mode auto-detected via TransportInfo
client := pb.NewEchoServiceClient(conn)

// shutdown — one call closes the conn, the transport, and the channel:
conn.Close(nil)
```

`drpc.NewConn` attaches the transport (`drpc.ConnAttacher`): the drain pump
starts by itself — no goroutine to manage — and the transport owns the
DataChannel from then on.

If your client side *receives* the channel instead of creating it, call
`pion.New` synchronously inside `OnDataChannel`, for the same reason `Bind`
must be (below).

For an unreliable channel (lossy sensor path):

```go
ordered := false
retx := uint16(0)
dc, err := pc.CreateDataChannel("sensor", &webrtc.DataChannelInit{
    Ordered:        &ordered,
    MaxRetransmits: &retx,
})
```

## Server (the side that receives channels)

```go
gw := pion.NewGateway()
srv := drpc.NewServer(gw)
pb.RegisterEchoServiceServer(srv, &myHandler{})

pc.OnDataChannel(func(dc *webrtc.DataChannel) {
    gw.Bind(dc)                   // MUST run synchronously in this callback
    go gw.ServePeer(ctx, srv, dc) // MUST NOT block this callback
})
```

`Bind` must be synchronous inside `OnDataChannel`: pion holds the channel's
read loop until that callback returns and drops messages that arrive with no
handler registered. `ServePeer` blocks until the channel dies (then calls
`srv.DisconnectPeer`), so it must run on its own goroutine.

### Mixed channels

Channels of differing reliability mix freely under one `Gateway` and one
`drpc.Server` — a reliable control channel plus unreliable telemetry
channels on the same `PeerConnection` is the natural wiring. `ServePeer`
annotates each peer with its channel's reliability
(`drpc.NewReliableContext`), and the server runs every peer in its own
mode: strict sequencing with no timers on the reliable channel, the full
timer machinery on the unreliable ones. No mode options anywhere.

## Options

| Option | Default | Meaning |
|---|---|---|
| `WithMaxMessageSize(n)` | 1200 B unreliable / 16 KiB reliable | largest marshaled `Envelop` this endpoint will send; 0 removes the limit |
| `WithMaxBufferedAmount(n)` | 1 MiB | outbound high-water mark: sends block while `dc.BufferedAmount()` is at or above it (pion queues without limit); 0 never blocks |
| `WithSendStallTimeout(d)` | 30 s | total budget for one send — the channel-open wait and the buffered-amount wait — before the channel is declared dead; 0 waits on ctx alone |

## Caveats

- **Watch the `PeerConnection` too.** A severed peer (network gone, browser
  killed) may never surface `OnClose` on the channel — the SCTP shutdown
  needs a live transport to travel over. Hook the connection state and close
  the channel — or call `Conn.Close` / `DisconnectPeer` — yourself:

  ```go
  pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
      if s == webrtc.PeerConnectionStateFailed {
          conn.Close(errors.New("peer connection failed"))
      }
  })
  ```

- **The 16 KiB reliable default is the browser-interop ceiling.** Pion-to-pion
  can go higher; raise it only when no browser is involved. On an unreliable
  channel keep messages inside one SCTP packet (the 1200 B default) — a
  fragmented partially-reliable message is lost whenever any fragment is lost.
- A message over the size limit fails the owning call with
  `ResourceExhausted`; the channel stays up.
- Attach promptly after `New` — i.e. call `drpc.NewConn` right away (and
  `ServePeer` every bound channel): inbound messages buffer from
  construction, and once the bound (32 messages) fills, pion's read loop for
  that channel blocks until the pump drains it.
- DataChannels are DTLS-encrypted by WebRTC itself — no extra transport
  security needed, but there is still no *authentication* of frames beyond
  the channel (see `PROTOCOL.md` §15).
