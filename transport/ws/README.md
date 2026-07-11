# transport/ws

drpc over WebSocket ([gorilla/websocket](https://github.com/gorilla/websocket)):
**one binary message carries one marshaled `Envelop`**. The transport is
reliable and ordered, so the core auto-detects reliable mode and runs with
every protocol timer off — plain gRPC semantics over a WebSocket.

Its own Go module: importing the core never pulls gorilla.

```go
import "github.com/lesomnus/grpc-dgram/transport/ws"
```

## Server

One `Gateway` serves many WebSocket connections, one peer each.

```go
gw := ws.NewGateway()
srv := drpc.NewServer(gw)
pb.RegisterEchoServiceServer(srv, &myHandler{})

up := websocket.Upgrader{}
http.HandleFunc("/rpc", func(w http.ResponseWriter, r *http.Request) {
    c, err := up.Upgrade(w, r, nil)
    if err != nil {
        return
    }
    // Blocks until the connection dies, then deregisters the peer and
    // calls srv.DisconnectPeer — failing that peer's live calls.
    gw.ServePeer(r.Context(), srv, c)
})
```

## Client

`drpc.NewConn` attaches the transport (`drpc.ConnAttacher`): the read loop
and keepalive start by themselves — no goroutine to manage — and the
transport owns the WebSocket from then on.

```go
c, _, err := websocket.DefaultDialer.DialContext(ctx, "wss://host/rpc", nil)
if err != nil { ... }

conn := drpc.NewConn(ws.New(c)) // reliable mode auto-detected via TransportInfo
client := pb.NewEchoServiceClient(conn)

// shutdown — one call closes the conn, the transport, and the socket:
conn.Close(nil)
```

## Options

| Option | Default | Meaning |
|---|---|---|
| `WithKeepalive(interval, timeout)` | 20 s / 30 s | ping cadence, and how long the peer may go without read progress (data or pong) before the connection is declared dead |
| `WithMaxMessageSize(n)` | 0 (unlimited) | bound on sends, for paths (a proxy, a browser) that cap message size; a reliable transport otherwise carries any size |

## Caveats

- **Teardown is the whole point.** With protocol timers off, the *only*
  mechanism that unblocks live calls is the adapter detecting transport death
  and calling `Conn.Close` / `DisconnectPeer` — the attached client pump and
  `ServePeer` do this on every exit path.
- **Keepalive doubles as the write bound.** Data writes carry a deadline
  equal to the keepalive timeout: a peer that stops draining would otherwise
  block a send forever with no timer to save it. A stalled write is treated
  as transport death.
- Received non-binary messages and unparseable envelops are ignored; they
  never tear the connection down.
- **Use `wss://`** (or a trusted network): the protocol itself has no
  authentication or encryption — see `PROTOCOL.md` §15.
