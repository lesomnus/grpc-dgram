# websocket-echo

The same gRPC code as the sensor demo, over a **reliable** transport: a
WebSocket. The core detects that from the adapter, turns every protocol timer
and retransmission off, and delivers the exact sequence a handler sent — plain
gRPC semantics with no datagram caveats. The second half of the demo is
shutdown: `GracefulStop` drains an in-flight stream and refuses what comes
after.

```sh
go run ./...
```

Server and client run in one process. Two processes work too:

```sh
go run ./... -serve 127.0.0.1:9010                 # terminal 1
go run ./... -connect ws://127.0.0.1:9010/rpc      # terminal 2
```

Expected output:

```
server listening on ws://127.0.0.1:37959/rpc
Echo("hello") -> "hello"

Count(count=20, interval=50ms):
  tick 1
  ...

[400ms in] GracefulStop: refusing new calls, waiting for the live handler
  tick 20
  20 responses, sequence 1..20 exactly — no gaps, no reordering
[server] GracefulStop returned: every handler finished

calling Echo again, on a server that has already stopped:
  refused, as expected: rpc error: code = Unavailable desc = call reset by peer
```

## What it demonstrates

- **Reliable mode, auto-detected.** `gorilla.NewGateway()` and `gorilla.New(ws)`
  advertise `Reliable() == true`; `drpc.NewServer` / `drpc.NewConn` read that
  once at construction and run with no timers, no retransmission, and a strict
  sequence window (`PROTOCOL.md` §4.3, §10.6). There is no option to set.
- **Exact sequence, checked.** `client.go` asserts that `Count` delivers every
  response, in order, `1..N`. On this transport a gap is not a silent
  subsequence: the core fails the call with `INTERNAL`, because a "reliable"
  transport that lost a frame is broken.
- **Who unblocks a call when there are no timers.** The adapter. `ServePeer`
  blocks until the socket dies and then calls `srv.DisconnectPeer`; the client
  pump calls `conn.Close`. With protocol timers off that teardown is the *only*
  thing that fails live calls (§4.5) — which is why the gorilla adapter also
  runs a ping/pong keepalive and puts a write deadline on every send.
- **Graceful shutdown.** `GracefulStop` refuses new calls and waits for
  in-flight handlers: the `Count` stream started before the stop still delivers
  all 20 responses and ends with a clean `io.EOF`, while the `Echo` attempted
  afterwards is refused immediately with a RESET (§9.4) instead of hanging.
- **Order of teardown.** RPC layer first (`GracefulStop`), HTTP second. An
  upgraded WebSocket is a hijacked connection, so `http.Server.Shutdown`
  neither waits for it nor closes it — the drpc gateway owns it from the
  `Upgrade` on.

## Files

| | |
|---|---|
| `main.go` | flags, the demo's narrative, the mid-stream shutdown |
| `server.go` | the handlers, `gorilla.NewGateway` + `ServePeer` behind `net/http` |
| `client.go` | `drpc.NewConn` + `gorilla.New`, unary and streaming calls |
| `proto/wsecho.proto`, `echopb/` | the service and its checked-in bindings |

Regenerate the bindings (needs [buf](https://buf.build); the generated files
are committed, so running the example does not):

```sh
buf generate examples/websocket-echo/proto --template examples/websocket-echo/buf.gen.yaml   # from the repo root
```

## Notes

- `ws://` is plaintext and the protocol has no authentication of its own
  (`PROTOCOL.md` §15). Use `wss://` in anything real; nothing else changes.
- The keepalive defaults (20 s ping, 30 s timeout) are what detect a dead peer
  here; `gorilla.WithKeepalive` tunes them.
