# transport/udp

drpc over UDP datagrams: **one datagram carries one marshaled `Envelop`**, the
channel is unreliable (drpc's default mode, timers on), and nothing is ever
fragmented. This is the sensor-stream path.

Part of the core module — importing it pulls no third-party dependencies.

```go
import "github.com/lesomnus/grpc-dgram/transport/udp"
```

## Server

One listening socket serves many peers; each peer is keyed by its source
address.

```go
pc, err := net.ListenUDP("udp", &net.UDPAddr{Port: 7777})
if err != nil { ... }

gw := udp.NewGateway(pc)
srv := drpc.NewServer(gw)
pb.RegisterSensorServiceServer(srv, &myHandler{})

go gw.Serve(ctx, srv) // read pump; returns on ctx-done or socket close

// shutdown:
srv.GracefulStop() // or srv.Stop()
pc.Close()
```

## Client

A connected socket talks to one server. `drpc.NewConn` attaches the
transport (`drpc.ConnAttacher`): the receive pump starts by itself — no
goroutine to manage — and the transport owns the socket from then on.

```go
c, err := net.Dial("udp", "10.0.0.7:7777")
if err != nil { ... }

conn := drpc.NewConn(udp.New(c)) // unreliable mode auto-detected via TransportInfo
client := pb.NewSensorServiceClient(conn)

// shutdown — one call closes the conn, the transport, and the socket:
conn.Close(nil)
```

## Options

| Option | Default | Meaning |
|---|---|---|
| `WithMaxMessageSize(n)` | `DefaultMaxMessageSize` (1200 B) | largest marshaled `Envelop` this endpoint will **send**; receives accept any datagram |

## Caveats

- **One message = one datagram, never fragmented.** A marshaled envelop over
  the limit is refused at send and the owning call fails with
  `ResourceExhausted`. The 1200 B default stays under the typical 1500 B path
  MTU; raise it only if you control the path. Keep messages small — natural
  for sensor readings.
- **ICMP unreachable is treated as datagram loss.** A momentarily absent
  peer (e.g. a restarting server) surfaces as `ECONNREFUSED` on the connected
  socket; the adapter swallows it on both read and write so the core's
  retransmission and liveness machinery can ride the outage out. Calls still
  terminate within their deadlines if the peer never comes back.
- **No transport-death signal.** UDP is connectionless, so there is nothing
  to hook teardown on: vanished peers are handled by the core's timers
  (`T_call`, `T_live`), and shutting down is your move — on the client one
  `conn.Close(nil)` does it all; on the server, close the socket, then
  `Server.Stop`.
- **The wire is plaintext and spoofable.** Deploy over an encrypted channel
  (DTLS, WireGuard, ...) or on a trusted network — see `PROTOCOL.md` §15.
