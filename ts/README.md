# @lesomnus/grpc-dgram

TypeScript port of [grpc-dgram](../): gRPC-style RPC over unreliable datagram
channels, implementing the **drpc wire protocol v1.0** (`../PROTOCOL.md`).
Wire-compatible with the Go implementation — the §5 golden byte vectors are
shared between the two test suites — so a TS client interoperates with a Go
server and vice versa.

- **Zero runtime dependencies.** The three wire messages (`Frame`, `Envelop`,
  `Metadata`) are hand-encoded; user payloads go through pluggable
  per-method marshallers (protobuf-es, JSON, anything that produces bytes).
  A **protobuf-es binding** (`@lesomnus/grpc-dgram/transport/protobuf-es`, optional peer
  dep) derives the whole method descriptor — path, streaming kind, codec —
  from a generated `protoc-gen-es` service, so RPC types are never re-declared
  by hand. The core itself stays dependency-free.
- **Both endpoints.** `Conn` (client) and `Server`, with the full unreliable-
  mode machinery: seq windows and dedup, epoch/`peer_epoch` incarnation
  isolation, control-frame retransmission, tombstones + aged watermark,
  PING/probe liveness, and the §15 resource caps. On a reliable transport all
  timers are off and sequencing is strict fail-loud (§10.6).
- **WebRTC DataChannel adapter** (`@lesomnus/grpc-dgram/transport/webrtc`), the TS twin
  of the Go `transport/pion` adapter: the protocol mode is derived from the
  channel's own configuration — an ordered channel with no retransmit or
  lifetime cap runs reliable, anything else unreliable. Client `Transport`
  and a mixed-mode server `Gateway` (a reliable control channel and
  unreliable telemetry channels on one peer connection serve side by side,
  each peer in its channel's mode).
- **Node UDP adapter** (`@lesomnus/grpc-dgram/transport/node-udp`, Node only), the TS
  twin of the Go `transport/udp` adapter — `UdpTransport`/`UdpGateway` with
  `dialUdp`/`listenUdp` helpers. A TS client over this adapter interoperates
  with a Go `drpc.Server` on the wire; this is exercised by a cross-language
  conformance test (`test/conformance.test.ts`) that drives a real Go server.
- **Connect-ES transport** (`@lesomnus/grpc-dgram/transport/connect`, optional peer dep
  on `@connectrpc/connect`): use the standard `createClient(Service, transport)`
  ergonomics while the traffic runs over drpc. `createDrpcTransport(conn)`
  turns a drpc `Conn` into a Connect `Transport` — the conformance suite drives
  a **real Go `drpc.Server` through a Connect client** end to end.

## Usage

**With protobuf-es (recommended).** Point the binding at a generated service —
paths, streaming kinds, and codecs are all derived from the `.proto`, so
there is nothing to keep in sync and a TS client interoperates with a Go
server addressing the same methods:

```ts
import { fromService } from '@lesomnus/grpc-dgram/transport/protobuf-es'
import { create } from '@bufbuild/protobuf'
import { EchoService, EchoRequestSchema } from './echo_pb' // protoc-gen-es output

const Echo = fromService(EchoService) // { once, many, count, live }, fully typed

await conn.invoke(Echo.once, create(EchoRequestSchema, { text: 'hi' }))
server.register(Echo.once, (req) => create(EchoResponseSchema, { text: req.text }))
```

**With a Connect client.** If you already use Connect-ES, keep its client
ergonomics and swap only the transport:

```ts
import { createClient } from '@connectrpc/connect'
import { createDrpcTransport } from '@lesomnus/grpc-dgram/transport/connect'
import { EchoService } from './echo_pb'

const client = createClient(EchoService, createDrpcTransport(conn))
await client.once({ message: 'hi' })                 // unary
for await (const m of client.many({ message: 'x' })) // server streaming
```

**Without codegen.** Describe methods explicitly with any byte serializer
(the core is codec-agnostic — this is what the test suite uses):

```ts
import { unaryMethod, bidiMethod, type PayloadCodec } from '@lesomnus/grpc-dgram'

const json = <T>(): PayloadCodec<T> => ({
  marshal: (v) => new TextEncoder().encode(JSON.stringify(v)),
  unmarshal: (b) => JSON.parse(new TextDecoder().decode(b)),
})

const Once = unaryMethod<Req, Res>('/echo.Echo/Once', { request: json(), response: json() })
const Live = bidiMethod<Req, Res>('/echo.Echo/Live', { request: json(), response: json() })
```

Client over an `RTCDataChannel`:

```ts
import { Conn } from '@lesomnus/grpc-dgram'
import { DataChannelTransport } from '@lesomnus/grpc-dgram/transport/webrtc'

const dc = pc.createDataChannel('rpc') // ordered, no caps → reliable mode, no timers
const conn = new Conn(new DataChannelTransport(dc)) // pump attaches itself

const res = await conn.invoke(Once, { text: 'hi' })

const stream = conn.newStream(Live, {})
await stream.send({ text: 'x' })
for await (const msg of stream) console.log(msg)

conn.close() // one close tears everything down, channel included
```

Server behind a gateway (any number of peers, mixed reliability):

```ts
import { Server } from '@lesomnus/grpc-dgram'
import { DataChannelGateway } from '@lesomnus/grpc-dgram/transport/webrtc'

const gw = new DataChannelGateway()
const server = new Server(gw)
server.register(Once, (req) => ({ text: `echo:${req.text}` }))
server.register(Live, async (stream) => {
  for await (const msg of stream) await stream.send(msg)
})

pc.ondatachannel = ({ channel }) => {
  gw.bind(channel) // synchronously, so no early message is lost
  void gw.servePeer(server, channel) // §4.5 teardown on every exit
}
```

Handlers are plain functions per RPC type:

| Type | Signature |
|---|---|
| unary | `(req, ctx) => Res \| Promise<Res>` |
| server-streaming | `(req, stream, ctx) => Promise<void>` |
| client-streaming | `(stream, ctx) => Res \| Promise<Res>` — the return value is the response |
| bidi | `(stream, ctx) => Promise<void>` |

`ctx.signal` is an `AbortSignal` aborted when the call ends for any reason
(client abort, RESET, deadline, liveness expiry, `stop()`), with the
`StatusError` cause as its reason; `ctx.setHeader` / `ctx.sendHeader` /
`ctx.setTrailer` follow §11. Handler failures are `StatusError`s (anything
else maps to `UNKNOWN`).

## Translation notes (Go ↔ TS)

| Go | TS |
|---|---|
| `context.Context` cancellation/deadline | `CallOptions.signal` (`AbortSignal`) + `timeoutMs`; handler `ctx.signal` |
| `*status.Status` errors | `StatusError { code, desc }` |
| `metadata.MD` | `Metadata = Record<string, string[]>` |
| `grpc.ClientConnInterface` / generated stubs | `conn.invoke(desc, req)` / `conn.newStream(desc)` |
| `RegisterService` + codegen | `server.register(desc, handler)` per method |
| `TransportInfo` / `ConnAttacher` discovery | same, structural (`reliable()`, `attachConn()`) |
| `drpc.ErrMessageTooLarge` | `MessageTooLargeError` (adapters throw it or set it as `cause`) |
| `NewPeerContext` / `NewReliableContext` | `FrameContext { peer, reliable, signal }` argument to `handle` |
| mutexes + atomics | none needed: state transitions are synchronous between `await` points |

Deliberately not ported (yet): client/server interceptors, the stats surface
(planned in Go as well), and `Envelop` batching (`Coalescer` — planned M6 in
Go; every envelop carries one frame, as the shipped Go adapters do).

Receive-path note for browsers: an `RTCDataChannel` cannot pause delivery, so
reliable-mode backpressure (§4.2) bounds *ordering and loss*, not adapter
memory — inbound messages queue in the adapter while a slow consumer drains.
The Node/pion-style read-loop blocking does not exist in a browser.

## Tests

`pnpm test` — 90 tests mirroring the Go suites: the §5 golden wire vectors
byte-for-byte, e2e for all four RPC types, the §10 timeout system under
deterministic fake-timer loss (blackhole, lost terminals/acks/half-closes,
probes, liveness), the §6.5 restart walkthroughs, §15 caps and §4.2 drop
policies, and the DataChannel adapter against a mock channel pair (including
the reliable-datachannel echo, the project's final-goal demo shape).
