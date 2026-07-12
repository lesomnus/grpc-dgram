# TS port — status & handoff

Snapshot as of the `ts port: drpc v1.0 core …` commit (M6, TS port). Read this
before continuing the port.

## TL;DR

The TypeScript port of drpc v1.0 (`../PROTOCOL.md`) is **functionally
complete and green**, but has **not yet been adversarially validated** the way
the Go implementation was — that validation pass was launched and then **killed
by a token limit** (details below). Treat the port as "works, all tests pass,
not yet audited."

## Done

Client + server core and the WebRTC DataChannel adapter, wire-compatible with
the Go reference. Layout under `ts/src/`:

| File | Role | Go twin |
|---|---|---|
| `wire.ts` | zero-dep protobuf codec for `Frame`/`Envelop`/`Metadata` | `*.pb.go`, `frame.go` |
| `seq.ts` | tx seq + rx window (dedup, beyond-window fail-loud, strict mode) | `seq.go` |
| `timing.ts` / `limits.ts` | timer + resource-cap resolution | `timing.go`, `limits.go` |
| `status.ts` / `metadata.ts` | `StatusError`/`Code`, `Metadata` | `status`, `metadata.go` |
| `transport.ts` / `desc.ts` | `FrameHandler`/`TransportInfo`/`ConnAttacher`, method descriptors + codecs | `frame.go`, grpc codegen |
| `util.ts` | `Latch`, `FrameQueue` (drop-policy + reliable blocking put), `Sweeper` | Go channels/goroutines |
| `conn.ts` | `Conn` + `ClientStream`, client unreliable-mode machinery | `conn.go`, `stream.go`, `unreliable.go` |
| `server.ts` | `Server` + server stream, per-peer state, sweep, caps | `server.go`, `stream.go`, `unreliable_server.go` |
| `webrtc.ts` | `DataChannelTransport` (client) + `DataChannelGateway` (server, mixed-mode) | `transport/pion/*.go` |

Verified at this commit:

- `pnpm test` → **90 passing** (6 files). Mirrors the Go suites:
  `wire.test.ts` (the §5 golden byte vectors, **byte-identical to Go** — the
  cross-implementation contract), `e2e.test.ts` (four RPC types, EOF, metadata,
  deadlines, cancel, reliable-mode fail-loud, lifecycle), `timeout.test.ts`
  (the §10 system under deterministic fake-timer loss — blackhole, lost
  terminal/ack/half-close, probe, liveness, at-most-once), `restart.test.ts`
  (§6.5 walkthroughs), `limits.test.ts` (§15 caps, §4.2 drop policies, §6.3
  DATA_LOSS, §9.4 watermark, per-peer mode), `datachannel.test.ts` (adapter
  against a mock RTCDataChannel pair, incl. the reliable-datachannel echo — the
  project's final-goal demo shape).
- `pnpm check` (`tsc --noEmit`, strict) → clean.
- `pnpm build` (tsdown) → clean; emits `dist/index.mjs` + `dist/webrtc.mjs`
  with `.d.mts`. `dist/` is gitignored.

## Failed / interrupted (token limit)

A **three-way adversarial audit** (spec ↔ `ts/src` ↔ Go reference) was launched
as a background workflow — 10 focused reviewers (wire, seq, conn-rx, conn-tx,
conn-sweep, server-demux, server-stream, server-sweep, async-translation,
test-adequacy) feeding an adversarial multi-lens verify stage, the same shape
that found and fixed dozens of bugs in the Go implementation.

It was **killed mid-Review phase when the Fable 5 token limit was hit**
(`status: killed`, "You've reached your Fable 5 limit"). **No findings were
produced** — the port has therefore had *no* adversarial review yet. This is
the **#1 follow-up**: re-run the audit on a model with budget. Highest-risk
areas to point it at, because they are the least mechanical translations:

- **`server.ts` map restructuring.** Go's flat maps keyed by `(peer,epoch,sid)`
  became nested `slot → epoch → PeerState` with server-wide counters
  (`pendingResetTotal`, `resetAtTotal`, `replyBudgetTotal`) standing in for
  Go's flat-map `len()` cap checks. Verify every insert/delete keeps those
  counters exact and that slot/container GC can't strand or double-count.
- **The async translation claim.** The port drops Go's mutexes on the argument
  that "state transitions are synchronous between `await` points." The reliable-
  mode blocking puts (`FrameQueue.putBlocking`) and the synchronous test pipes
  (transmit → peer handle → back into this endpoint within one microtask chain)
  are where that claim is most load-bearing. Audit for interleavings Go's locks
  would have excluded.
- **`wire.ts` decode on hostile input** (truncation, huge varints, malformed
  metadata) and **`seq.ts` 32-bit wrap** vs Go `uint32`.

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

## Remaining M6 work (after the audit)

1. **Re-run the adversarial audit** and fix confirmed findings (see above).
2. **Go ↔ TS conformance CI.** The strongest guarantee: stand a Go `drpc.Server`
   behind a real transport (or a pipe), drive it from the TS `Conn`, and assert
   all four RPC types + loss recovery. The golden-vector tests already pin the
   encoding statically; this pins the *behavior* across implementations.
3. **`examples/`** — a runnable browser↔Go WebRTC datachannel echo (the
   final-goal demo, cross-language this time).
4. **Packaging** — decide the published name/scope (currently
   `@lesomnus/grpc-dgram`, `private: true`) and a real `version`. *(The
   protobuf-es binding — `@lesomnus/grpc-dgram/protobuf-es`, `fromService` /
   `fromMethod`, `@bufbuild/protobuf` optional peer dep — is **done**: it
   derives path + streaming kind + codec from a generated `protoc-gen-es`
   service, so RPC types aren't re-declared. Core stays zero-dep; verified the
   core bundles carry no `@bufbuild/protobuf` reference.)*
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
