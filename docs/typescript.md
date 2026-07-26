# The TypeScript port

[`ts/`](../ts) is a second implementation of the same protocol — client **and**
server — written for browsers and Node. It is not a binding over the Go code
and shares no runtime with it; what the two share is
[PROTOCOL.md](./PROTOCOL.md) and a test suite that keeps them honest.

This page is the orientation for someone standing in the Go repo. The API
reference is [`ts/README.md`](../ts/README.md); this one explains what the port
is, how interoperability is proven, and where the two languages differ.

## What it is

- **Zero runtime dependencies.** `Frame`, `Envelop` and `Metadata` are
  hand-encoded protobuf; user payloads go through pluggable per-method codecs.
  The protobuf-es and Connect-ES bindings are optional peer dependencies.
- **Both endpoints**, with the full datagram machinery: seq windows and dedup,
  epoch/`peer_epoch` incarnation isolation, control retransmission, tombstones
  and the aged watermark, PING/probe liveness, the §15 caps — and, in reliable
  mode, per-stream flow control.
- **Browser-safe.** `AbortSignal`, `setTimeout`, `crypto`, `TextEncoder`,
  `CompressionStream`. No Node built-ins outside the `node-udp` adapter.

```
ts/src/
  wire.ts     frame/envelop/metadata codec, flags, shape helpers
  conn.ts     Conn + ClientStream          server.ts  Server + streams
  seq.ts      tx seq + rx window           flow (in util.ts) credit windows
  transport/  webrtc · websocket · node-udp · protobuf-es · connect
```

## How interoperability is kept honest

Two mechanisms, doing different jobs.

**Golden byte vectors** pin the *encoding*. `ts/src/wire.test.ts` carries frames
as hex, and every vector was generated from the Go implementation with
`proto.MarshalOptions{Deterministic: true}` — including the v1.1 fields
(`window`, `compressor`, `details`) and the awkward metadata cases: a `-bin`
value containing `00 01 ff 80 7f`, a present-but-empty `Metadata`, a key with
no values, a key with one empty value. If either side's encoder drifts, the
byte comparison fails.

**The conformance suite** pins the *behavior*. `ts/test/conformance.test.ts`
builds and runs the real Go server in `conformance/udpserver` and drives it
over UDP from a TS client — all four RPC shapes, metadata, a Go-encoded
`Timestamp`, non-OK statuses with details, unknown methods, edge payloads. It
skips itself when `go` is absent; CI installs Go so it always runs.

The distinction matters for one case in particular. Binary metadata is the only
place where the two languages' *idiomatic representations* differ, so a
mirror-shaped test ("send it, get it back") would pass even if both sides were
wrong together. The conformance test therefore asserts the exact octets a Go
server sent, decoded from the base64 the TS API hands back.

## Using it

Browser, over a WebRTC DataChannel — the shape
[`examples/browser-webrtc`](../examples/browser-webrtc) runs:

```ts
import { Conn } from '@lesomnus/grpc-dgram'
import { DataChannelTransport } from '@lesomnus/grpc-dgram/transport/webrtc'

const dc = pc.createDataChannel('rpc')  // ordered, no caps → reliable mode
const conn = new Conn(new DataChannelTransport(dc))  // the pump attaches itself

const res = await conn.invoke(Echo.once, { message: 'hi' })

const stream = conn.newStream(Echo.live, {})
await stream.send({ message: 'x' })
for await (const msg of stream) console.log(msg)

conn.close()  // one close tears down conn, transport and channel
```

Node, over UDP, against a Go server:

```ts
import { Conn } from '@lesomnus/grpc-dgram'
import { dialUdp } from '@lesomnus/grpc-dgram/transport/node-udp'

const conn = new Conn(await dialUdp(7777, '127.0.0.1'))
```

A server is the same shape in reverse — `new Server(gateway)`,
`server.register(desc, handler)` per method, then `gateway.servePeer(...)`
after registration, for the reason §13 gives: the registry freezes when serving
starts.

Method descriptors come from generated code if you have it
(`fromService(EchoService)` in the protobuf-es binding derives path, streaming
kind and codec from the `.proto`), or are declared explicitly with any byte
serializer. If you already use Connect-ES, `createDrpcTransport(conn)` keeps
`createClient(Service, transport)` and swaps only what is underneath.

## Go ↔ TS

| Go | TypeScript |
|---|---|
| `context.Context` cancel/deadline | `CallOptions.signal` + `timeoutMs`; handler `ctx.signal` |
| `*status.Status` | `StatusError { code, desc }`, details via `statusDetails(err)` |
| `metadata.MD` | `Metadata = Record<string, string[]>` |
| `metadata.MD` with a `-bin` key | **base64** in the TS API, raw octets on the wire |
| generated stubs / `RegisterService` | `conn.invoke(desc, req)` / `server.register(desc, handler)` |
| `TransportInfo` / `ConnAttacher` | the same seams, structural (`reliable()`, `attachConn()`) |
| `drpc.ErrMessageTooLarge` | `MessageTooLargeError` |
| `NewPeerContext` / `NewReliableContext` | a `FrameContext { peer, reliable, signal }` argument |
| mutexes and atomics | none: state transitions are synchronous between `await` points |

The metadata row is the one to remember. Go keeps the raw octets of a `-bin`
value inside a `string`, which is grpc-go's own convention; a JS string cannot,
so the TS API uses base64 and converts at the codec boundary. Both put the same
bytes on the wire — the conformance test above is what proves it.

## What the port deliberately does not have

Client/server **interceptors**, the **observability surfaces** (Go's
`stats.Handler` bridge and `ProtocolStats`), and **`Envelop` batching** — the
last one is unbuilt in Go too ([TODO.md](./TODO.md)). These are gaps, not
divergences: the wire is identical either way.

One genuine environmental difference: a browser `RTCDataChannel` cannot pause
delivery, so inbound messages queue in the adapter while a slow consumer
drains. Since v1.1 the protocol paces the *sender* instead
([reliable-mode.md](./reliable-mode.md)), so this no longer costs ordering or
stalls other calls — it is only adapter memory.

## Building and testing

```sh
cd ts
pnpm install
pnpm test    # vitest — unit, e2e, and the Go↔TS conformance suite
pnpm check   # tsc --noEmit, strict
pnpm build   # tsdown → dist/
```

Unit and per-adapter tests are co-located with their source
(`src/wire.test.ts`, `src/transport/webrtc/index.test.ts`, …); cross-cutting
suites live in `ts/test/`. `ts/STATUS.md` is the handoff document: what is
done, what was found in audit, and what remains.
