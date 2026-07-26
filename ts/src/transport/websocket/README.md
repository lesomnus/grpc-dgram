# `@lesomnus/grpc-dgram/transport/websocket`

dRPC over **WebSocket** — the TS twin of the Go `transport/gorilla` adapter,
and wire-compatible with it. **One binary message carries one marshaled
`Envelop`.** The channel is reliable and ordered, so the core auto-detects
reliable mode and runs with every protocol timer off (§10.6): plain gRPC
semantics over a WebSocket.

Uses the platform `WebSocket` — **no npm dependency, no node builtin**: the
browser, Deno, Node ≥22, and node's `ws` package all fit (`WebSocketLike` is
structural, so test mocks do too). The adapter sets `binaryType =
'arraybuffer'` itself.

## Client — `WebSocketTransport` / `dialWebSocket`

The `Conn` attaches the transport (`ConnAttacher`): the receive pump and the
keepalive start by themselves — nothing to manage — and the transport owns the
socket from then on. Sends are gated on the handshake, so calls made
immediately just queue.

```ts
import { Conn } from '@lesomnus/grpc-dgram'
import { dialWebSocket } from '@lesomnus/grpc-dgram/transport/websocket'

const conn = new Conn(dialWebSocket('wss://host/rpc')) // reliable mode auto-detected
await conn.invoke(Once, req)

conn.close() // one call closes the Conn, the transport, and the socket
```

Wrap a socket you built yourself with `new WebSocketTransport(ws)` — do it on
the same tick as `new WebSocket(url)`, since the stack drops messages that
arrive before a listener is registered.

## Server — `WebSocketGateway`

One `Server` serving many peers, one socket each; the peer key is a fresh
opaque counter per socket (addresses collide behind proxies, §6.4). `bind`
inside the connection handler so no early message is lost; `servePeer` blocks
until the socket dies, then deregisters the peer and calls
`server.disconnectPeer` — failing that peer's live calls.

```ts
import { Server } from '@lesomnus/grpc-dgram'
import { WebSocketGateway } from '@lesomnus/grpc-dgram/transport/websocket'

const gw = new WebSocketGateway()
const server = new Server(gw)
// server.register(...)

wss.on('connection', (ws) => {
  gw.bind(ws)
  void gw.servePeer(server, ws) // resolves with the death cause
})
```

## Options

| Option | Default | Meaning |
|---|---|---|
| `maxMessageSize` | `0` (unlimited) | bound on sends, for paths (a proxy, a browser) that cap message size; a reliable transport otherwise carries any size (§4.4) |
| `maxBufferedAmount` | 1 MiB | outbound high-water mark: sends park while `bufferedAmount` is at or above it |
| `sendStallTimeoutMs` | keepalive timeout (30 s) | how long one send may wait — for the socket to open, or at the mark — before the socket is declared dead |
| `keepaliveIntervalMs` / `keepaliveTimeoutMs` | 20 s / 30 s | ping cadence, and how long the peer may go without read progress (data or pong) before it is declared dead. Ignored where the runtime exposes no ping/pong |

## Caveats

- **Teardown is the whole point.** With protocol timers off, the *only*
  mechanism that unblocks live calls is the adapter detecting transport death
  and calling `conn.close()` / `server.disconnectPeer()` — the attached pump
  and `servePeer` do this on every exit path (§4.5).
- **Death must be detected out of band.** `close`/`error` events fire even
  while delivery is blocked in backpressure, and a send that stalls past its
  budget counts as death too — a peer that stops draining is not something to
  wait out (§4.2). Where the runtime exposes `ping`/`pong` (node's `ws`), the
  adapter also runs gorilla's liveness rule; **the browser `WebSocket` has no
  ping API**, so there the keepalive is off and death arrives as an event or a
  stalled send.
- **No rx backpressure in the browser.** A `WebSocket` cannot pause delivery,
  so inbound messages queue in the adapter while a slow reliable-mode consumer
  drains (Go's blocking read loop, which turns a full buffer into TCP
  backpressure, has no browser equivalent). Ordering and the §4.2 no-silent-drop
  contract still hold; memory is the cost.
- Text messages and undecodable payloads are ignored, and nothing is delivered
  after the socket dies; neither ever tears the connection down.
- **Use `wss://`** (or a trusted network): the protocol itself has no
  authentication or encryption — see `PROTOCOL.md` §15.
