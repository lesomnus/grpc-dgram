# TS port — status & handoff

Read this before continuing the port.

## TL;DR

The TypeScript port of dRPC **v1.1** (`../docs/PROTOCOL.md`) is **functionally
complete, green, and adversarially audited**. Cross-language interop with the
Go server is verified at runtime over UDP — including the v1.1 surface, where
a Go/TS split would be silent: binary metadata, status details and the
flow-control advertisements are asserted against exact bytes, not mirrored
shapes. Nothing is outstanding: the last gap, an
envelope-level send for an application's own batcher to call, closed with
`sendFrames` on every exported adapter class (`../docs/TODO.md` §1 — the
batching policy is the workload's, and neither language ships a batcher).

**v1.1 round (2026-07-25)** — mirrored from the Go core: metadata values are
bytes on the wire (`-bin` keys hold base64 in the TS API), `Frame.window` /
`WINDOW` per-stream flow control, `Frame.compressor` / `COMPRESSED`
per-message compression via `CompressionStream`, `Frame.details` status
details, the shape/modifier flag split (§7.1), per-call size caps, and the
unary `sendHeader` flush. New adapter: **WebSocket** (`transport/websocket`),
the twin of Go's `transport/gorilla`. Since **2026-07-26** a second one:
**message port** (`transport/port`), the twin of Go's new `transport/jsport` —
the first channel whose two ends can be in one process, which is what lets a Go
server compiled to `js/wasm` serve the page it runs in
(`../examples/browser-wasm`). It brings the port's own teardown protocol (an
empty message is the goodbye) and a second cross-language proof,
`test/wasm.test.ts`, this one on a channel that is *actually* reliable.

**Connection-window round (2026-09-11; advertised since 2026-09-12)** — the
mirror of Go's `flow.go` for the per-peer **connection window** (§4.2.1, PR
#12's Go core and spec; issue #4 part 2, issue #20 for the advertisement): a
reliable-mode data frame needs one credit from its stream window *and* one
from the peer's. The window is **advertised** as `Frame.connWindow` — a `Conn`
on every OPEN, a `Server` on every H and T — and a sender adopts the first
advertisement it hears from a peer incarnation (`FlowSender.observe`, once;
absent = off, a later one ignored). Only the client assumes anything: `W_CONN`
= 1024 until the server's first H or T; a server's sender is created from the
OPEN that creates its container, admitted or rejected. Credit rides `WINDOW`
on `sid = 0` and never enables, the receiver's ledger (`PeerFlowRx`) is per
transport peer with pending held per client incarnation and a stash for
senders the container cap evicted, and one `T_stall` covers both windows
(`acquireBoth`). `Limits.maxPeerWindow` (floored at `W_CONN`, capped at the
wire's uint32) and the `peer-flow-stall` / `peer-flow-resume` event pair came
with it. The advertisement replaced an earlier draft that inferred the window
— a settle from the per-stream advertisement plus a one-off "raise" grant —
which parked a streaming sender for good against a peer that paces streams
but never grants on `sid = 0` (Appendix A, entry 11). Where the Go code and
the design memo differed, the Go code won; the few places TS cannot take Go's
shape are stated in the code (the pre-check before taking credit instead of
take-then-refund, since there is no yield between them; `tryAcquireBoth` as
the synchronous fast path). Both cross-language suites move more than
`W_conn` messages each way across three streams, so a TS endpoint and a Go
endpoint pace each other on sid 0 exactly as two Go endpoints do.

**WebTransport round (2026-09-11)** — a third adapter twin, **WebTransport
datagrams** (`transport/webtransport`, the client half of Go's new
`transport/webtransport`; issue #6, step one): the browser's unreliable
channel with no signaling. `dialWebTransport(url)` → `Conn`, or
`new WebTransportDatagramTransport(wt)` over a structural `WebTransportLike`;
the ceiling follows `datagrams.maxDatagramSize` at each send (1200 B when
unreported), the §4.5 teardown comes from `closed`/`ready`, and
`requireUnreliable` defaults to true. Client only — Node has no
`WebTransport`, so there is no TS server and no cross-language run for this
channel yet; the adapter is tested against a mock session pair. The reliable
per-stream channel on the same session is the issue's second step.

## Done

Client + server core and the WebRTC DataChannel adapter, wire-compatible with
the Go reference. The zero-dep core lives in `ts/src/`; each third-party /
platform adapter is its **own directory** under `ts/src/transport/` — an
`index.ts` plus a `README.md` — exported as `@lesomnus/grpc-dgram/transport/*`,
mirroring Go's `transport/{udp,pion,gorilla,webtransport,jsport}/` layout (dir
+ README each).

| File | Role | Go twin |
|---|---|---|
| `wire.ts` | zero-dep protobuf codec for `Frame`/`Envelope`/`Metadata` | `*.pb.go`, `frame.go` |
| `seq.ts` | tx seq + rx window (dedup, beyond-window fail-loud, strict mode) | `seq.go` |
| `timing.ts` / `limits.ts` | timer + resource-cap resolution (`maxPeerWindow`, the advertised connection window: floored at `W_CONN`, capped at the wire's uint32) | `timing.go`, `limits.go` |
| `status.ts` / `metadata.ts` | `StatusError`/`Code`, `Metadata` | `status`, `metadata.go` |
| `seam.ts` / `desc.ts` | `FrameHandler`/`TransportInfo`/`ConnAttacher`, method descriptors + codecs | `frame.go`, grpc codegen |
| `util.ts` | `Latch`, `FrameQueue` (drop-policy + reliable blocking put), `Sweeper`; the §4.2.1 flow-control primitives both endpoints share — `FlowSender` (per stream and per connection: `assume` / `observe` / `reassume`) / `FlowReceiver`, `PeerFlowRx` (the per-peer ledger: admit/retire/unadmitted, per-incarnation pending, the evicted-sender stash), `acquireBoth` / `tryAcquireBoth` (one park, one `T_stall` across both windows), `W_INIT` / `W_CONN`; compression + size caps | Go channels/goroutines; `flow.go`, `frame.go`/`callinfo.go` |
| `conn.ts` | `Conn` + `ClientStream`, client unreliable-mode machinery, the client half of the connection window (`connTx`/`connRx`, `connWindow` on every OPEN, the advertisement adopted from the first H or T of a server incarnation — live call or released — the server-epoch lock, the sid-0 WINDOW arm, credit for every non-buffered frame) | `conn.go`, `stream.go`, `unreliable.go`, `flow.go` |
| `server.ts` | `Server` + server stream, per-peer state, sweep, caps, the server half of the connection window (the per-transport-peer ledger, the container's sender created from the OPEN's advertisement — admitted or rejected — `advertise` on every H and T, unstash-before-evict, release on `disconnectPeer`) | `server.go`, `stream.go`, `unreliable_server.go`, `flow.go` |
| `stats.ts` | `ProtocolStats` observer type, `ProtocolEvent`, `Counters` — the §14 gap counter, the other datagram-only events, and the stream / peer flow-stall pairs | `stats.go` (the `ProtocolStats` half; the `stats.Handler` half has no TS twin) |
| `interceptor.ts` | Unary/stream, client/server interceptor types in a `(…, next)` shape, and the chain fold — element 0 outermost, the last element gets the real invoker/handler | the interceptor chains of `conn.go` / `server.go` (grpc-go's order; TS-native signatures, arrays instead of single-vs-chain options) |
| `transport/webrtc/` | `DataChannelTransport` (client) + `DataChannelGateway` (server, mixed-mode) | `transport/pion/*.go` |
| `transport/websocket/` | `WebSocketTransport` + `dialWebSocket` → `Conn` (client), gateway/`servePeer` (server), reliable | `transport/gorilla/*.go` |
| `transport/webtransport/` | `WebTransportDatagramTransport` + `dialWebTransport` → `Conn` (client only; Node has no `WebTransport`, the server is Go), unreliable over the session's datagrams; teardown from `closed`/`ready` | `transport/webtransport/*.go` |
| `transport/port/` | `PortTransport` + `dialWorker` → `Conn`, `PortGateway` over `postMessage`, reliable; teardown is the empty-message goodbye plus `close(cause)` | `transport/jsport/*.go` |
| `transport/node-udp/` | `UdpTransport`/`UdpGateway` + `dialUdp` → `Conn` / `listenUdp` (Node `dgram`) | `transport/udp/*.go` |
| `transport/protobuf-es/` | `fromService`/`fromMethod` — derive descriptors from generated protobuf-es | grpc-go codegen (G2) |
| `transport/connect/` | `createDrpcTransport` — a Connect-ES `Transport` over a dRPC `Conn` | — (Connect interop) |

Verified at this commit:

- `pnpm test` → **569 passing** (27 files). Unit and per-adapter tests are
  co-located next to their source (`src/wire.test.ts`,
  `src/transport/connect/index.test.ts`, …); cross-cutting integration tests
  (e2e, timeout, restart, limits, flow, flow_peer_client, flow_peer_server,
  conformance, wasm, protobufes-gen, stats —
  every `ProtocolEvent` kind from both ends: sid/method on call-scope events,
  peer on every server event, `count` for skipped/dropped/off-shape, and
  `Counters`) stay in `test/`;
  shared fixtures + generated code live in `src/testing/` (not an entry, never
  in `dist/`). Mirrors the Go suites, plus the audit regression pins
  (`transport/node-udp/index.test.ts`, `server.test.ts`, `util.test.ts`):
  `wire.test.ts` (the §5 golden byte vectors, **byte-identical to Go** — the
  cross-implementation contract), `e2e.test.ts` (four RPC types, EOF, metadata,
  deadlines, cancel, reliable-mode fail-loud, lifecycle), `timeout.test.ts`
  (the §10 system under deterministic fake-timer loss — blackhole, lost
  terminal/ack/half-close, probe, liveness, at-most-once), `restart.test.ts`
  (§6.5 walkthroughs), `limits.test.ts` (§15 caps, §4.2 drop policies, §6.3
  DATA_LOSS, §9.4 watermark, per-peer mode, the `maxPeerWindow` scope per
  transport peer), `flow.test.ts` (the §4.2.1 stream window end to end),
  `flow_peer_client.test.ts` / `flow_peer_server.test.ts` (the twins of
  `flow_peer_client_test.go` / `flow_peer_server_test.go` and the peer-window
  halves of `server_internal_test.go`: the `W_conn` assumption until the
  advertisement, the advertisement on every OPEN / H / T and its once-only
  adoption — a unary T, a rejected first OPEN, a released call's H or T,
  absent = off, a later one ignored — the sid-0 gate, overrun failing only
  the offender, credit for every non-buffered frame, the starvation clause,
  one stall budget, no credit held while parked, credit granted to the
  incarnation that spent it, the evicted sender's stash, TS↔TS past `W_conn`
  both ways),
  `src/transport/webrtc/index.test.ts` (adapter against a mock RTCDataChannel
  pair, incl. the reliable-datachannel echo — the project's final-goal demo
  shape), `src/transport/{websocket,port}/index.test.ts` (their adapters
  against mock pairs and a real `MessageChannel`, incl. the goodbye and the
  §4.5 teardown), `src/transport/webtransport/index.test.ts` (the datagram
  client against a mock session pair: the `ready` gate, the ceiling read live
  from `maxDatagramSize`, §4.5 from `closed` and `ready`, `dialWebTransport`),
  `src/wasm/{index,worker}.test.ts` (`open()` against a fake
  `Go`: the ordering rules a dropped frame would otherwise hide),
  `protobufes*.test.ts` (the binding, verified
  against real `protoc-gen-es` output), and the two cross-language suites —
  **`conformance.test.ts`** (a TS client driving a **real Go `drpc.Server`**
  over UDP, unreliable and reliable-annotated) and **`wasm.test.ts`** (the
  same server built `GOOS=js GOARCH=wasm` and driven over a `MessageChannel`)
  — each of which now moves more than `W_conn` messages each way across three
  streams and asserts the `sid = 0` grants in both directions — see below.
- `pnpm check` (`tsc --noEmit`, strict) → clean.
- `pnpm build` (tsdown) → clean; emits `dist/index.mjs`, one
  `dist/transport/*.mjs` per adapter entry, and `dist/wasm.mjs` +
  `dist/wasm/worker.mjs` — the last one is shipped code rather than an API
  surface: it is the worker `open()` starts, resolved relative to `wasm.mjs`,
  so the two must stay in that layout. All with `.d.mts`. `dist/` is
  gitignored.

## Adversarial audit — done (4 findings fixed)

A three-way audit (spec ↔ `ts/src` ↔ Go reference) ran across the highest-risk
translations: server map restructuring, the async/no-mutex claim, wire decode +
seq wrap, and the adapters. (An earlier attempt was killed by a token limit;
this one completed.) Four findings, all fixed with teeth-verified regression
tests:

- **`transport/node-udp/index.ts` — connected-socket ICMP unreachable tore the
  endpoint down** (major). A connected UDP socket delivers
  ECONNREFUSED/EHOSTUNREACH/ENETUNREACH as an `'error'` event (not the send
  callback), and the socket stays usable —
  but the handler called `close()` unconditionally, so the first ICMP unreachable
  from a restarting server permanently closed the socket and failed the call
  `UNAVAILABLE`, breaking the restart-ride-out §4.5 contract Go's `transport/udp`
  honors. Fixed to ignore `transient()` errors, matching Go.
  (`src/transport/node-udp/index.test.ts`)
- **`server.ts` — §15 cap under-count after disconnect + same-key reuse** (major,
  low exposure). `finish()` decremented `this.slots.get(peer).liveCalls`; if the
  slot was deleted by `disconnectPeer` and the key reused before the call
  unwound, it decremented the *new* slot's counter, under-enforcing
  `MaxLiveCalls`. Fixed to decrement the slot the call was created on
  (`st.slot`), mirroring Go's `livePeer` map surviving `DisconnectPeer`. Not
  reachable through the shipped fresh-key adapters, but the new `node-udp`
  gateway uses stable keys. (`src/server.test.ts`)
- **`util.ts` — `FrameQueue.putBlocking` not FIFO-safe** (low, latent). With ≥2
  putters parked on one queue, freeing a slot woke all and let the first to run
  `tryPut` win, so a later frame could jump an earlier one (reliable-mode
  reorder). Not reachable via a conforming sequential-delivery adapter, but the
  primitive stands in for a Go channel (a true FIFO), so hardened with a
  call-order chain. (`src/util.test.ts`)
- **`wire.ts` — metadata entry order** (minor, harmless at the time). Emitted
  in JS insertion order; both sides decoded fine. It now sorts keys ascending —
  since then a §11 MUST (entries are a repeated message, not a map) and pinned
  by the metadata golden vectors both suites share.

Everything else the audit attacked was verified clean: all other counter
paths and GC, the demux→open no-await double-create window, re-entrant
transmit, `wire.ts` hostile-input decode (truncation/overlong varint/wrong
wire-type all throw or skip safely), negative/boundary Duration, explicit
presence, `seq.ts` 32-bit wrap and window verdicts (byte-identical to Go),
adapter teardown paths, §4.4 synchronous size refusal, webrtc backpressure,
and unhandled-rejection/leak review.

The Connect-ES transport got its own review (streaming contract, error/metadata
mapping, cancellation/leak). One finding, fixed: `transport/connect/index.ts`
fed raw dRPC metadata straight into the WHATWG `Headers` API, which throws a `TypeError` on
a value with a newline/control char or a non-latin1 codepoint (emoji) or a
non-token key — so a spec-legal server response (§11 imposes no character
limit) crashed the call with a raw `TypeError` instead of returning the message
or a `ConnectError`, at all five conversion sites. Fixed with a total,
never-throwing `safeAppend` that drops only the entries HTTP headers cannot
represent; the message and status always surface.
(`src/transport/connect/index.test.ts`)

## Deliberately NOT ported (not gaps)

Do not "fix" these — they are intentional, matching the Go feature set or TS
idiom: the **`stats.Handler` bridge** (Go's
`WithStatsHandler` takes a grpc-go type with no TS counterpart — the drpc
half, `ProtocolStats`/`Counters`, IS ported: `src/stats.ts`,
`docs/observability.md`); **a batcher / `Coalescer`** (neither language ships
one and neither will — the batching policy is workload-specific,
`docs/TODO.md` §1; the *seam* to install one against is here, `sendFrames`
on every exported adapter class); handler signatures are
TS-native functions (not grpc-go codegen); `context.Context` → `AbortSignal` +
`CallOptions`; `metadata.MD` → `Record<string,string[]>`. One genuine
environmental difference, documented in `transport/webrtc/index.ts`: a browser `RTCDataChannel`
cannot pause delivery, so reliable-mode backpressure (§4.2) bounds ordering and
loss but **not** adapter rx memory — inbound messages queue while a slow
consumer drains. The Node/pion read-loop blocking has no browser equivalent.

## Remaining work

1. **Adversarial audit** — **done** (see the section above; 4 findings fixed).
2. **Go ↔ TS conformance** — **done** (`test/conformance.test.ts`). A TS `Conn`
   using the generated `Echo` descriptors — and, additionally, a **standard
   Connect client** via `createDrpcTransport` — drives a real Go `drpc.Server`
   (`conformance/udpserver`, serving `internal/echo` over `transport/udp`) via
   the Node UDP adapter. It asserts the wire/behavior contract points that can
   only be confirmed where the two implementations meet: all four RPC shapes +
   payload values (`CircularShift`, ascending sequence), header/trailer
   metadata, a Go-encoded proto `Timestamp`, **a Go-returned non-OK status
   (code + desc) decoded exactly**, **unknown-method → `UNIMPLEMENTED`**, and
   **edge payloads (0-byte / UTF-8 / large) round-tripping the proto codec** —
   plus the same shapes through a Connect client. This pins the *behavior*
   across implementations, where the golden vectors pin the *encoding*.
   `skipIf(!go)`, and CI runs it (the `ts` job sets up Go).
   **Reliable-mode interop — also done** (`test/wasm.test.ts`), and it needed
   the message-port adapter to be possible. The same `internal/echo` service
   (`conformance/wasmserver`) is built `GOOS=js GOARCH=wasm`, loaded into the
   vitest process with the toolchain's own `wasm_exec.js`, and served over a
   `MessageChannel` — so the channel between the two implementations really is
   reliable instead of being annotated as such per frame, which is all loopback
   UDP could offer. That buys the part of v1.1 that exists in reliable mode
   only: mode discovered from the transport with zero options on either side,
   the §4.2.1 windows advertised — the stream window on the OPEN and the
   creation ack, the connection window on the OPEN and on every H and T —
   credit granted in both directions as each side's handler consumes — per
   stream and, since the connection-window round, on `sid = 0` once the
   aggregate passes `W_conn/2` (the same case runs on the reliable-annotated
   UDP endpoint in `conformance.test.ts`, one message in flight so loopback
   never queues). It
   also pins both teardown
   paths, which are what §4.5 costs on a channel with no death to detect —
   `drpcStop()` (the Go gateway's goodbye → the TS calls fail `UNAVAILABLE`)
   and `drpcExit()` on a second instance (`os.Exit` says nothing → `go.run()`
   resolves → the host's `transport.close(cause)` fails them with that cause).
   Plus a Go-returned status and gzip in both directions.
   **Deliberately not pursued: loss-recovery interop.** Neither loopback UDP
   nor a message port loses anything, so exercising the §10 retransmission path
   across implementations would mean injecting loss in code — which is exactly
   what each language's own suite already does (fake timers, lossy filters,
   injected frames). It would add negligible interop-specific coverage over the
   happy path (the same already-golden frames flow; recovery *logic* is
   per-side) for the cost of a lossy proxy between the two. Not worth it; the
   boundary is covered by the cases above.
3. ~~**`examples/`**~~ — **done**: `../examples/browser-webrtc` runs the
   browser↔Go WebRTC DataChannel echo against this port's `dist/`, and
   `../examples/browser-wasm` runs a Go server compiled to `js/wasm` inside the
   page over `transport/port` — both import `dist/` through an import map, so
   `pnpm build` is their prerequisite (`../examples/` also has a UDP sensor
   stream and a WebSocket echo).
4. ~~**Packaging**~~ — **done**: `@lesomnus/grpc-dgram` **0.0.1**, Apache-2.0,
   `publishConfig.access: public`, `private` gone. `files` stays `["dist"]` —
   tsdown emits no source maps, so there is nothing for `src` to be shipped
   *for*. Verified the way a consumer meets it rather than the way a test does:
   the tarball installed into an empty project, all eight entry points
   imported, and a real unary round-trip driven through the **published
   bundle** — `PortGateway`/`Server` on one end of a `MessageChannel`,
   `Conn`/`PortTransport` on the other — because the suites run against `src`
   and it is `dist` that is published. *(The
   protobuf-es binding — `@lesomnus/grpc-dgram/transport/protobuf-es`, `fromService` /
   `fromMethod`, `@bufbuild/protobuf` optional peer dep — is **done and
   verified against real `protoc-gen-es` output**: `test/protobufes-gen.test.ts`
   imports `EchoService` generated by `buf generate` from the same
   `proto/echo/echo.proto` the Go module uses, derives every method via
   `fromService`, and round-trips all four RPC types with genuine proto wire
   bytes — so a TS client and the Go server address the same methods with the
   same encoding. Regenerate the fixture with `pnpm gen`. Core stays zero-dep;
   verified the core bundles carry no `@bufbuild/protobuf` reference.)*
5. **The envelope-level send seam** — **done** (`sendFrames` on every
   exported adapter class; `src/transport/node-udp/batcher.test.ts` shows
   the shape, including the per-peer batching a gateway must respect). A
   batcher itself stays deliberately absent in both languages: the policy is
   the application's, and [the measurement](../docs/batching-measurement.md)
   is why.

## Build / test

```
cd ts
pnpm install
pnpm test     # vitest, 569 tests (the two cross-language suites need `go` on PATH)
pnpm check    # tsc --noEmit (strict)
pnpm build    # tsdown → dist/
```
