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
  stats.ts    ProtocolStats observer + Counters (the §14 gap counter)
  transport/  webrtc · websocket · port · node-udp · protobuf-es · connect
  wasm/       open() — a Go server compiled to js/wasm, started in a worker
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

**The conformance suites** pin the *behavior*, on two channels.
`ts/test/conformance.test.ts` builds and runs the real Go server in
`conformance/udpserver` and drives it over UDP from a TS client — all four RPC
shapes, metadata, a Go-encoded `Timestamp`, non-OK statuses with details,
unknown methods, edge payloads. `ts/test/wasm.test.ts` builds
`conformance/wasmserver` for `GOOS=js GOARCH=wasm`, loads it into the test
process and drives it over a `MessageChannel`
([`transport/port`](../ts/src/transport/port) ↔
[`transport/jsport`](../transport/jsport)): a genuinely reliable channel
between the two implementations, which is what the flow-control cases (§4.2.1,
reliable mode only) and both teardown paths need. Both skip themselves when
`go` is absent; CI installs Go so they always run.

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
import { dialUdp } from '@lesomnus/grpc-dgram/transport/node-udp'

const conn = await dialUdp(7777, '127.0.0.1')
```

The browser, with no server anywhere — the Go service compiled to `js/wasm`
and started by the page ([`examples/browser-wasm`](../examples/browser-wasm)):

```ts
import { open } from '@lesomnus/grpc-dgram/wasm'

const sock = await open('/app.wasm')  // a worker, an instance, a readiness handshake
const conn = sock.dial()              // and again for a second, independent peer
```

Three ways in, and the verb says which: `new XTransport(ch)` wraps a channel you
already hold, `dial…(target)` makes the channel and hands back a `Conn` the way
`net.Dial` hands back a `net.Conn`, and `open(app)` brings the peer into
existence first because a `.wasm` file is not something you can reach yet.
[Transports](./transports.md#the-four-ways-in) has the fourth — the serving
side — and the reasoning.

A server is the same shape in reverse — `new Server(gateway)`,
`server.register(desc, handler)` per method, then serving, always *after*
registration, for the reason §13 gives: the registry freezes when serving
starts. Which serving call depends on what the gateway owns: `serve(server)`
where it holds the whole endpoint (the UDP socket), `bind` + `servePeer` where
channels arrive one at a time (a WebSocket, a DataChannel, a port).

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

Client/server **interceptors**, the **`stats.Handler` bridge** (grpc-go's
type; the drpc half, `ProtocolStats`/`Counters`, is ported — see
[observability.md](./observability.md#typescript)), and **`Envelop`
batching** — the last one is unbuilt in Go too ([TODO.md](./TODO.md)). These
are gaps, not divergences: the wire is identical either way.

One genuine environmental difference: a browser `RTCDataChannel` cannot pause
delivery, so inbound messages queue in the adapter while a slow consumer
drains. Since v1.1 the protocol paces the *sender* instead
([reliable-mode.md](./reliable-mode.md)), so this no longer costs ordering or
stalls other calls — it is only adapter memory.

## Building and testing

```sh
cd ts
pnpm install
pnpm test    # vitest — unit, e2e, and both Go↔TS conformance suites
pnpm check   # tsc --noEmit, strict
pnpm build   # tsdown → dist/
```

Unit and per-adapter tests are co-located with their source
(`src/wire.test.ts`, `src/transport/webrtc/index.test.ts`, …); cross-cutting
suites live in `ts/test/`. `ts/STATUS.md` is the handoff document: what is
done, what was found in audit, and what remains.
