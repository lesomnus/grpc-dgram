# Changelog

`@lesomnus/grpc-dgram` is the TypeScript port of gRPC-dgram. The wire
protocol it speaks is unversioned by design (`docs/PROTOCOL.md`, §10.6); a
package version is a cut of that text at one date, and a minor bump while the
major is 0 means the wire broke. Both ends of a channel must be deployed
together.

## 0.1.0 — 2026-09-12

**Wire, breaking against 0.0.1** — a peer on 0.0.1 cannot talk to one on
0.1.0:

- `Metadata` is an ordered list of entries, `repeated Entry { key, values }`,
  emitted in ascending key order; a receiver merges a repeated key (§11).
- `Frame.conn_window` (field 18) advertises the connection window: the client
  on every OPEN, the server on every H and T. The assumed-then-raised window
  is gone, with everything that existed only for it (§4.2.1).
- The wire unit is spelled `Envelope`; the adapter seam is `EnvelopeSender`
  (`sendFrames`) beside `FrameHandler`, and batching is a seam, not a
  component (§4.1).
- Field 6 is unassigned and held for `ack`; a breaking generation of the wire
  sets flag bit 64 on the first frame of every call (§10.6).
- A proto `string` field that is not valid UTF-8 makes the envelope
  undecodable and dropped whole, as in the Go core (§5, §11).

**Behaviour**

- The per-peer connection window (`limits.maxPeerWindow`, §4.2.1): one ledger
  per peer, `sid = 0` grants, overrun fails only the offending call.
- A call RESET before the `Conn` has locked to a server incarnation refunds
  the connection credit its frames took (§4.2.1 *Sending*).
- Interceptors: `interceptors: [...]` on `ConnOptions` and `ServerOptions`,
  unary and stream, chained in gRPC's order.
- `ProtocolStats` and `Counters` (§14), so a browser client reports gaps.
- WebTransport datagrams: `@lesomnus/grpc-dgram/transport/webtransport`.
- The wasm bridge: `dial({ entryPoint })` for a second server in one
  instance, a per-dial `readyTimeoutMs`, and the timers a Go program leaves
  pending are cancelled when it exits.

## 0.0.1 — 2026-08-08

First publish. Deprecated on 2026-09-12: it speaks a wire that 0.1.0 broke.
