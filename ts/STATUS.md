# TS port — status & handoff

Snapshot as of the `ts port: drpc v1.0 core …` commit (M6, TS port). Read this
before continuing the port.

## TL;DR

The TypeScript port of drpc v1.0 (`../PROTOCOL.md`) is **functionally complete,
green, and adversarially audited** (4 findings found and fixed — see below).
Cross-language interop with the Go server is verified at runtime over UDP. What
remains is packaging polish and optional Go-parity stretch items.

## Done

Client + server core and the WebRTC DataChannel adapter, wire-compatible with
the Go reference. The zero-dep core lives in `ts/src/`; each third-party /
platform adapter is its **own directory** under `ts/src/transport/` — an
`index.ts` plus a `README.md` — exported as `@lesomnus/grpc-dgram/transport/*`,
mirroring Go's `transport/{udp,pion,gorilla}/` layout (dir + README each).

| File | Role | Go twin |
|---|---|---|
| `wire.ts` | zero-dep protobuf codec for `Frame`/`Envelop`/`Metadata` | `*.pb.go`, `frame.go` |
| `seq.ts` | tx seq + rx window (dedup, beyond-window fail-loud, strict mode) | `seq.go` |
| `timing.ts` / `limits.ts` | timer + resource-cap resolution | `timing.go`, `limits.go` |
| `status.ts` / `metadata.ts` | `StatusError`/`Code`, `Metadata` | `status`, `metadata.go` |
| `seam.ts` / `desc.ts` | `FrameHandler`/`TransportInfo`/`ConnAttacher`, method descriptors + codecs | `frame.go`, grpc codegen |
| `util.ts` | `Latch`, `FrameQueue` (drop-policy + reliable blocking put), `Sweeper` | Go channels/goroutines |
| `conn.ts` | `Conn` + `ClientStream`, client unreliable-mode machinery | `conn.go`, `stream.go`, `unreliable.go` |
| `server.ts` | `Server` + server stream, per-peer state, sweep, caps | `server.go`, `stream.go`, `unreliable_server.go` |
| `transport/webrtc/` | `DataChannelTransport` (client) + `DataChannelGateway` (server, mixed-mode) | `transport/pion/*.go` |
| `transport/node-udp/` | `UdpTransport`/`UdpGateway` + `dialUdp`/`listenUdp` (Node `dgram`) | `transport/udp/*.go` |
| `transport/protobuf-es/` | `fromService`/`fromMethod` — derive descriptors from generated protobuf-es | grpc-go codegen (G2) |
| `transport/connect/` | `createDrpcTransport` — a Connect-ES `Transport` over a drpc `Conn` | — (Connect interop) |

Verified at this commit:

- `pnpm test` → **126 passing** (13 files). Mirrors the Go suites, plus the
  audit regression pins (`node-udp.test.ts`, `server-cap.test.ts`,
  `util.test.ts`):
  `wire.test.ts` (the §5 golden byte vectors, **byte-identical to Go** — the
  cross-implementation contract), `e2e.test.ts` (four RPC types, EOF, metadata,
  deadlines, cancel, reliable-mode fail-loud, lifecycle), `timeout.test.ts`
  (the §10 system under deterministic fake-timer loss — blackhole, lost
  terminal/ack/half-close, probe, liveness, at-most-once), `restart.test.ts`
  (§6.5 walkthroughs), `limits.test.ts` (§15 caps, §4.2 drop policies, §6.3
  DATA_LOSS, §9.4 watermark, per-peer mode), `datachannel.test.ts` (adapter
  against a mock RTCDataChannel pair, incl. the reliable-datachannel echo — the
  project's final-goal demo shape), `protobufes*.test.ts` (the binding, verified
  against real `protoc-gen-es` output), and **`conformance.test.ts`** (a TS
  client driving a **real Go `drpc.Server`** over UDP — see below).
- `pnpm check` (`tsc --noEmit`, strict) → clean.
- `pnpm build` (tsdown) → clean; emits `dist/index.mjs` + `dist/webrtc.mjs`
  with `.d.mts`. `dist/` is gitignored.

## Adversarial audit — done (4 findings fixed)

A three-way audit (spec ↔ `ts/src` ↔ Go reference) ran across the highest-risk
translations: server map restructuring, the async/no-mutex claim, wire decode +
seq wrap, and the adapters. (An earlier attempt was killed by a token limit;
this one completed.) Four findings, all fixed with teeth-verified regression
tests:

- **`node-udp.ts` — connected-socket ICMP unreachable tore the endpoint down**
  (major). A connected UDP socket delivers ECONNREFUSED/EHOSTUNREACH/ENETUNREACH
  as an `'error'` event (not the send callback), and the socket stays usable —
  but the handler called `close()` unconditionally, so the first ICMP unreachable
  from a restarting server permanently closed the socket and failed the call
  `UNAVAILABLE`, breaking the restart-ride-out §4.5 contract Go's `transport/udp`
  honors. Fixed to ignore `transient()` errors, matching Go. (`test/node-udp.test.ts`)
- **`server.ts` — §15 cap under-count after disconnect + same-key reuse** (major,
  low exposure). `finish()` decremented `this.slots.get(peer).liveCalls`; if the
  slot was deleted by `disconnectPeer` and the key reused before the call
  unwound, it decremented the *new* slot's counter, under-enforcing
  `MaxLiveCalls`. Fixed to decrement the slot the call was created on
  (`st.slot`), mirroring Go's `livePeer` map surviving `DisconnectPeer`. Not
  reachable through the shipped fresh-key adapters, but the new `node-udp`
  gateway uses stable keys. (`test/server-cap.test.ts`)
- **`util.ts` — `FrameQueue.putBlocking` not FIFO-safe** (low, latent). With ≥2
  putters parked on one queue, freeing a slot woke all and let the first to run
  `tryPut` win, so a later frame could jump an earlier one (reliable-mode
  reorder). Not reachable via a conforming sequential-delivery adapter, but the
  primitive stands in for a Go channel (a true FIFO), so hardened with a
  call-order chain. (`test/util.test.ts`)
- **`wire.ts` — metadata map entry order** (minor, harmless). Emitted in JS
  insertion order; both sides decode fine and the golden vectors omit metadata,
  but sorting keys makes the encoding deterministic and matches Go's
  `Deterministic` marshal, so it now sorts.

Everything else the audit attacked was verified clean: all other counter
paths and GC, the demux→open no-await double-create window, re-entrant
transmit, `wire.ts` hostile-input decode (truncation/overlong varint/wrong
wire-type all throw or skip safely), negative/boundary Duration, explicit
presence, `seq.ts` 32-bit wrap and window verdicts (byte-identical to Go),
adapter teardown paths, §4.4 synchronous size refusal, webrtc backpressure,
and unhandled-rejection/leak review.

The Connect-ES transport got its own review (streaming contract, error/metadata
mapping, cancellation/leak). One finding, fixed: `connect.ts` fed raw drpc
metadata straight into the WHATWG `Headers` API, which throws a `TypeError` on
a value with a newline/control char or a non-latin1 codepoint (emoji) or a
non-token key — so a spec-legal server response (§11 imposes no character
limit) crashed the call with a raw `TypeError` instead of returning the message
or a `ConnectError`, at all five conversion sites. Fixed with a total,
never-throwing `safeAppend` that drops only the entries HTTP headers cannot
represent; the message and status always surface. (`test/connect.test.ts`)

## Deliberately NOT ported (not gaps)

Do not "fix" these — they are intentional, matching the Go feature set or TS
idiom: client/server **interceptors**; the **stats surface** (planned in Go
too, M6); **`Envelop` batching / `Coalescer`** (planned M6 in Go; every envelop
carries one frame, as the shipped Go adapters do); handler signatures are
TS-native functions (not grpc-go codegen); `context.Context` → `AbortSignal` +
`CallOptions`; `metadata.MD` → `Record<string,string[]>`. One genuine
environmental difference, documented in `webrtc.ts`: a browser `RTCDataChannel`
cannot pause delivery, so reliable-mode backpressure (§4.2) bounds ordering and
loss but **not** adapter rx memory — inbound messages queue while a slow
consumer drains. The Node/pion read-loop blocking has no browser equivalent.

## Remaining M6 work

1. **Adversarial audit** — **done** (see the section above; 4 findings fixed).
2. **Go ↔ TS conformance** — **done** (`test/conformance.test.ts`). A TS `Conn`
   using the generated `Echo` descriptors — and, additionally, a **standard
   Connect client** via `createDrpcTransport` — drives a real Go `drpc.Server`
   (`conformance/udpserver`, serving `internal/echo` over `transport/udp`) via
   the Node UDP adapter, and asserts all four RPC types plus metadata and a
   Go-encoded proto `Timestamp`, matching the Go handler's semantics
   (`CircularShift`, ascending sequence). This pins the *behavior* across
   implementations, where the golden vectors pin the encoding. The test
   `skipIf(!go)`, so `pnpm test` still runs without a Go toolchain, and CI is
   wired (the `ts` job sets up Go and runs it). **Remaining: loss-recovery
   conformance** — loopback UDP does not drop, so the §10 retransmission path
   is not yet exercised cross-language (needs a lossy proxy between the two).
3. **`examples/`** — a runnable browser↔Go WebRTC datachannel echo (the
   final-goal demo, cross-language this time).
4. **Packaging** — decide the published name/scope (currently
   `@lesomnus/grpc-dgram`, `private: true`) and a real `version`. *(The
   protobuf-es binding — `@lesomnus/grpc-dgram/transport/protobuf-es`, `fromService` /
   `fromMethod`, `@bufbuild/protobuf` optional peer dep — is **done and
   verified against real `protoc-gen-es` output**: `test/protobufes-gen.test.ts`
   imports `EchoService` generated by `buf generate` from the same
   `proto/echo/echo.proto` the Go module uses, derives every method via
   `fromService`, and round-trips all four RPC types with genuine proto wire
   bytes — so a TS client and the Go server address the same methods with the
   same encoding. Regenerate the fixture with `pnpm gen`. Core stays zero-dep;
   verified the core bundles carry no `@bufbuild/protobuf` reference.)*
5. Optional parity with Go stretch items if/when they land there: interceptors,
   stats handler, `Coalescer` batching.

## Build / test

```
cd ts
pnpm install
pnpm test     # vitest, 90 tests
pnpm check    # tsc --noEmit (strict)
pnpm build    # tsdown → dist/
```
