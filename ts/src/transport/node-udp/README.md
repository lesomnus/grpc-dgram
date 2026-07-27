# `@lesomnus/grpc-dgram/transport/node-udp`

dRPC over **UDP datagrams on Node.js** — the TS twin of the Go `transport/udp`
adapter, and wire-compatible with it. One datagram carries one marshaled
`Envelop`; the channel is unreliable (dRPC's default mode); nothing is ever
fragmented — a message over the size limit is refused at send with
`MessageTooLargeError`, which the core maps to `RESOURCE_EXHAUSTED` on the
owning call (§4.4).

**Node only** (it imports `node:dgram`); the browser uses the WebRTC adapter
instead. No npm dependency beyond the Node builtin.

## Client — `dialUdp` / `UdpTransport`

```ts
import { dialUdp } from '@lesomnus/grpc-dgram/transport/node-udp'

const conn = await dialUdp(port, '127.0.0.1')
await conn.invoke(Once, req)
conn.close() // closes the transport and the socket too
```

`dialUdp` opens a connected socket and hands back a **`Conn`** — the endpoint
you make calls on, the way `net.Dial` hands back a `net.Conn`. Its options are
one bag: `maxMessageSize` is the adapter's, everything `ConnOptions` declares
is the core's, and the two share no key.

Bring your own socket — or reach for the transport itself — with the explicit
pair:

```ts
import { Conn } from '@lesomnus/grpc-dgram'
import { UdpTransport } from '@lesomnus/grpc-dgram/transport/node-udp'

const conn = new Conn(new UdpTransport(socket, { maxMessageSize: 1200 }), connOpts)
```

## Server — `listenUdp` / `UdpGateway`

`listen`, not `dial`: a server endpoint reaches nobody. One socket serves every
peer that writes to it, keyed by source `address:port` (§6.4), so what comes
back is the gateway rather than a connection. `serve` is a separate call
because the registry freezes when serving starts (§13); it runs until
`close()`.

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
