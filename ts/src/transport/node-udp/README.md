# `@lesomnus/grpc-dgram/transport/node-udp`

drpc over **UDP datagrams on Node.js** — the TS twin of the Go `transport/udp`
adapter, and wire-compatible with it. One datagram carries one marshaled
`Envelop`; the channel is unreliable (drpc's default mode); nothing is ever
fragmented — a message over the size limit is refused at send with
`MessageTooLargeError`, which the core maps to `RESOURCE_EXHAUSTED` on the
owning call (§4.4).

**Node only** (it imports `node:dgram`); the browser uses the WebRTC adapter
instead. No npm dependency beyond the Node builtin.

## Client — `UdpTransport` / `dialUdp`

```ts
import { Conn } from '@lesomnus/grpc-dgram'
import { dialUdp } from '@lesomnus/grpc-dgram/transport/node-udp'

const conn = new Conn(await dialUdp(port, '127.0.0.1'))
await conn.invoke(Once, req)
conn.close() // closes the socket too
```

`dialUdp` opens a connected socket and returns a `UdpTransport`; wrap an
existing `dgram.Socket` with `new UdpTransport(socket)` if you manage it
yourself.

## Server — `UdpGateway` / `listenUdp`

The source `address:port` is the peer key. `serve` runs until `close()`.

```ts
import { Server } from '@lesomnus/grpc-dgram'
import { listenUdp } from '@lesomnus/grpc-dgram/transport/node-udp'

const { gateway, port } = await listenUdp(0, '127.0.0.1')
const server = new Server(gateway)
// server.register(...)
void gateway.serve(server)
```

## Notes

- **Connectionless.** There is no transport-death signal, so vanished peers are
  handled by the core's own liveness machinery (unreliable mode); tearing an
  endpoint down is the application's move.
- **ICMP unreachable = loss.** On a connected socket an ICMP unreachable
  (`ECONNREFUSED`/`EHOSTUNREACH`/`ENETUNREACH`) surfaces as an `'error'` event
  but the socket stays usable; the adapter treats it as datagram loss and rides
  it out (a restarting server is survived), exactly as the Go adapter does.
- **Plaintext.** Deploy over an encrypted channel (DTLS) or a trusted network
  (§15). `{ maxMessageSize }` (default 1200 B) bounds sends only.
- **Interop.** A TS client here talks to a Go `drpc.Server` over
  `transport/udp` on the wire — exercised by the cross-language conformance
  test.
