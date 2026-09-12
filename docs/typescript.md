# The TypeScript port

[`ts/`](../ts) is a second implementation of the same protocol — client **and**
server — written for browsers and Node. It is not a binding over the Go code
and shares no runtime with it; what the two share is
[PROTOCOL.md](./PROTOCOL.md) and a test suite that keeps them honest.

This page is the orientation for someone standing in the Go repo. The API
reference is [`ts/README.md`](../ts/README.md); this one explains what the port
is, how interoperability is proven, and where the two languages differ.

## What it is

- **Zero runtime dependencies.** `Frame`, `Envelope` and `Metadata` are
  hand-encoded protobuf; user payloads go through pluggable per-method codecs.
  The protobuf-es and Connect-ES bindings are optional peer dependencies.
- **Both endpoints**, with the full datagram machinery: seq windows and dedup,
  epoch/`peer_epoch` incarnation isolation, control retransmission, tombstones
  and the aged watermark, PING/probe liveness, the §15 caps — and, in reliable
  mode, flow control per stream and per peer (the connection window,
  §4.2.1).
- **Browser-safe.** `AbortSignal`, `setTimeout`, `crypto`, `TextEncoder`,
  `CompressionStream`. No Node built-ins outside the `node-udp` adapter.

```
ts/src/
  wire.ts     frame/envelope/metadata codec, flags, shape helpers
  conn.ts     Conn + ClientStream          server.ts  Server + streams
  seq.ts      tx seq + rx window           flow (in util.ts) stream + connection credit windows
  stats.ts    ProtocolStats observer + Counters (the §14 gap counter)
  transport/  webrtc · websocket · webtransport · port · node-udp · protobuf-es · connect
  wasm/       open() — a Go server compiled to js/wasm, started in a worker
```

## How interoperability is kept honest

Two mechanisms, doing different jobs.

**Golden byte vectors** pin the *encoding*. `ts/src/wire.test.ts` carries frames
as hex, and every vector is pinned byte for byte against what the Go
implementation marshals (`metadata_internal_test.go` holds the same metadata
vectors); both sides emit metadata entries in ascending key order (§11), so no
marshal option is needed for the bytes to agree — including the v1.1 fields
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
reliable mode only) and both teardown paths need. The flow-control cases go
past `W_conn` each way across three streams, so the run completes only if
both ends grant on `sid = 0`; the UDP suite runs the same case on the Go
fixture's reliable-annotated endpoint. Both skip themselves when `go` is
absent; CI installs Go so they always run.

One adapter has no cross-language run yet. The WebTransport datagram client
([`transport/webtransport`](../ts/src/transport/webtransport)) is
wire-compatible with the Go `transport/webtransport` gateway, but the suites
run in Node, which has no `WebTransport` — not the global the client dials
with, and no server either — so a Go↔TS run over that channel waits on a
runtime that has one (a browser driving the Go server, or a Node
implementation). Until then the adapter is verified against a mock session
pair, and the Go module's own suite is the bar for the channel; its README
says the same.

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

Browser, over WebTransport datagrams, against a Go server — the unreliable
channel with no signaling, one URL and nothing else:

```ts
import { dialWebTransport } from '@lesomnus/grpc-dgram/transport/webtransport'

const conn = dialWebTransport('https://host:4433/rpc') // https only; serverCertificateHashes for a dev cert
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
| `drpc.EnvelopeHandler` (`Send(ctx, *Envelope)`) — the §4.1 batching seam | `EnvelopeSender` (`sendFrames(frames, ctx?)`), implemented by every exported adapter class; the transports that have one destination take no ctx |
| `NewPeerContext` / `NewReliableContext` | a `FrameContext { peer, reliable, signal }` argument |
| `WithLimits(Limits{MaxPeerWindow: n})` — the §4.2.1 connection window | `limits: { maxPeerWindow: n }` on `ConnOptions` / `ServerOptions`; same floor (`W_CONN` = 1024), same default, same `sid = 0` grants on the wire |
| `EventPeerFlowStall` / `EventPeerFlowResume` (with the stream pair) | `'peer-flow-stall'` / `'peer-flow-resume'` — see [observability.md](./observability.md#typescript) |
| mutexes and atomics | none: state transitions are synchronous between `await` points |

The metadata row is the one to remember. Go keeps the raw octets of a `-bin`
value inside a `string`, which is grpc-go's own convention; a JS string cannot,
so the TS API uses base64 and converts at the codec boundary. Both put the same
bytes on the wire — the conformance test above is what proves it.

## Interceptors

Both ends take interceptor chains as arrays on their options —
`unaryInterceptors` / `streamInterceptors` on `ConnOptions` and
`ServerOptions` — in a TS-native shape: a function over the call and a
`next`, with no single-vs-chain split.

```ts
const conn = new Conn(adapter, {
  unaryInterceptors: [
    async (req, call, next) => {
      call.opts = { ...call.opts, metadata: { ...call.opts.metadata, authorization: ['bearer …'] } }
      return next(req, call)
    },
  ],
})

const server = new Server(adapter, {
  streamInterceptors: [
    async (stream, ctx, next) => {
      console.log(ctx.method, ctx.desc.clientStreams, ctx.desc.serverStreams)
      return next(stream, ctx)
    },
  ],
})
```

What carries over from Go: element 0 runs outermost and the last element is
handed the real invoker or handler (grpc-go's chain order — the reverse of
Connect-ES, which applies the last interceptor first); the stream is created
by the innermost invoker, after the chain, so metadata an interceptor adds
still rides the OPEN (§8, §11) and is validated there; and on a unary call in
unreliable mode `call.opts.timeoutMs` already holds T_call when the chain sees
it, as Go sets the ctx deadline before its chain runs (§10.2), and the budget
is absolute across the chain as a ctx deadline is: a retrying interceptor's
later attempts carry the remainder, an exhausted one fails
`DEADLINE_EXCEEDED` before reaching the wire, and an interceptor that sets a
different `timeoutMs` starts a new budget from that point. An interceptor may
skip `next` (a cache), call it again (a retry — every call is a fresh stream,
and `onHeader` / `onTrailer` fire once per attempt), or wrap what it returns.

What differs from grpc-go's signatures: `next` is async on the server and a
unary interceptor resolves to the response — the type rejects a void arrow,
so awaiting `next` without returning is a compile error rather than an empty
message; a server stream interceptor gets one `next` for the three streaming
shapes and reads `ctx.desc` to tell them apart; and for a client-streaming
call the value the chain resolves to is the response (resolving to nothing
fails the call with `INTERNAL`). Under the Connect binding a Conn's own chain
runs inside Connect's: the drpc transport sits at the centre of that onion.

## What the port does not have

The **`stats.Handler` bridge** (grpc-go's type; the drpc half,
`ProtocolStats`/`Counters`, is ported — see
[observability.md](./observability.md#typescript)). That is a gap, not a
divergence: the wire is identical either way.

Neither language ships a **batcher**, and neither will — the policy is the
workload's ([TODO.md](./TODO.md) §1). Both ship the seam to write one
against: in Go, embed the adapter and override `Handle`; here, `extends` it
and override `handle`, calling `sendFrames` with what you packed. TS is the
easier of the two, because the core discovers `reliable()`, `attachConn()`
and `close()` structurally and a subclass inherits all three — where Go's
field-wrapper would hide them. Two traps are TS's own. `private` members are
part of the type, so a subclass may not redeclare a name a base class holds
privately (`max`, and `send` under `WebTransportDatagramTransport`) — it is
rejected, and the subclass stops being assignable to the base. And of §4.1's
concurrency duty only the buffer half is free here: a flush on a reliable
adapter can park (waiting for the socket to open, or for `bufferedAmount` to
fall), so a second flush issued meanwhile overtakes it and reliable mode has
no retransmission to repair the reorder. Keep one flush in flight.

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
