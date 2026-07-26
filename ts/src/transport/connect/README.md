# `@lesomnus/grpc-dgram/transport/connect`

A **Connect-ES** `Transport` backed by a dRPC `Conn`: keep the standard
`createClient(Service, transport)` client ergonomics while the traffic runs
over dRPC (datagram RPC) to a dRPC server — Go or TS.

The bridge is thin because Connect's `Transport` receives protobuf-es method
descriptors, exactly what [`../protobuf-es`](../protobuf-es) turns into a dRPC
`MethodDesc`, and the `Conn` already implements all four RPC shapes.

## Peer dependencies

`@connectrpc/connect` and `@bufbuild/protobuf` are **optional peer
dependencies** — the core stays dependency-free; only this entry pulls them in.

```sh
npm i @connectrpc/connect @bufbuild/protobuf
```

## Usage

```ts
import { createClient } from '@connectrpc/connect'
import { createDrpcTransport } from '@lesomnus/grpc-dgram/transport/connect'
import { EchoService } from './echo_pb' // protoc-gen-es output

const client = createClient(EchoService, createDrpcTransport(conn))

await client.once({ message: 'hi' })                  // unary
for await (const m of client.many({ message: 'x' }))  // server streaming
const res = await client.count(reqIterable)           // client streaming
for await (const m of client.live(reqIterable)) {}    // bidi
```

`conn` is any dRPC `Conn` — over the WebRTC or Node UDP adapter, or an
in-memory pipe. The conformance suite drives a **real Go `drpc.Server` through
a Connect client** end to end.

## Impedance matching

- **Metadata** — Connect `Headers` ↔ dRPC `Metadata`. Multi-value entries
  round-trip as one comma-joined value (the same fidelity gRPC-over-HTTP has).
  dRPC metadata is arbitrary (§11), so a value HTTP headers cannot represent (a
  newline, a non-latin1 codepoint, a non-token key) is dropped rather than
  crashing the call — the message and status always surface.
- **Errors** — a dRPC `StatusError` maps to a `ConnectError` with the same code
  (the gRPC status codes are numerically identical), carrying header + trailer
  metadata.
- **Streaming** — Connect's `AsyncIterable` input is pumped into the stream's
  `send()`; the output is exposed as an `AsyncIterable`. Response headers are
  populated before Connect reads them (the transport peeks the first response
  frame), and the trailer after the message stream is exhausted.
