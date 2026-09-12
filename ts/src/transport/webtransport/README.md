# `@lesomnus/grpc-dgram/transport/webtransport`

dRPC over **WebTransport datagrams** — the TS twin of the Go
`transport/webtransport` adapter, and wire-compatible with it. One datagram
carries one marshaled `Envelope`; the channel is unreliable (dRPC's default
mode), so the core runs its full timer machinery; nothing is ever fragmented —
a message over the size limit is refused at send with `MessageTooLargeError`,
which the core maps to `RESOURCE_EXHAUSTED` on the owning call (§4.4).

This is the browser's unreliable channel with no signaling: a page reaches a Go
server with a URL and nothing else — no offer/answer, no ICE, no second server.
Uses the platform `WebTransport` — **no npm dependency, no node builtin**
(`WebTransportLike` is structural, so test mocks and other implementations fit).
Only the session's datagram side is used; its streams are left alone.

## Client — `dialWebTransport` / `WebTransportDatagramTransport`

```ts
import { dialWebTransport } from '@lesomnus/grpc-dgram/transport/webtransport'

const conn = dialWebTransport('https://host:4433/rpc') // unreliable mode, full timer machinery
await conn.invoke(Once, req)

conn.close() // one call closes the Conn, the transport, and the session
```

`dialWebTransport` opens a session with the runtime's global `WebTransport` and
hands back a **`Conn`** — the endpoint you make calls on, the way `net.Dial`
hands back a `net.Conn`. It is synchronous and returns *before* the handshake:
sends are gated on `ready`, so a call made on this very tick queues rather than
fails. The `Conn` attaches the transport (`ConnAttacher`), so the receive pump
starts by itself — nothing to manage — and the transport owns the session from
then on.

Options are one bag with no key in common between the halves: the session's
constructor options (`serverCertificateHashes`, `allowPooling`,
`congestionControl`, `protocols`, `requireUnreliable`) go to `new WebTransport`,
`maxMessageSize` to the adapter, everything `ConnOptions` declares to the core.

Bring your own session — a runtime with no global `WebTransport`, constructor
options this does not expose, or a test that needs the transport itself — with
the explicit pair:

```ts
import { Conn } from '@lesomnus/grpc-dgram'
import { WebTransportDatagramTransport } from '@lesomnus/grpc-dgram/transport/webtransport'

const conn = new Conn(new WebTransportDatagramTransport(wt, { maxMessageSize }), connOpts)
```

Nothing is lost by wrapping late: the platform queues inbound datagrams in the
readable until someone reads (bounded by `incomingHighWaterMark`, then dropped
as any datagram may be).

## Server

**Go only, in this step.** Node has no `WebTransport` — neither the global
this dial looks for nor a server — and nothing in this package serves one. The
server side is the Go `transport/webtransport` module: a `webtransport.Gateway`
behind `drpc.NewServer`, one `gw.ServePeer(ctx, srv, sess)` per accepted
session. A browser client here talks to that on the wire.

## Options

| Option | Default | Meaning |
|---|---|---|
| `maxMessageSize` | once the session is up, its `datagrams.maxDatagramSize` at each write, else 1200 B; 1200 B while it is still connecting | largest marshaled `Envelope` this endpoint sends (§4.4); bounds sends only. A browser reports a placeholder until `ready` (Chromium: 1024), not the ceiling the datagram will meet, so a call issued while connecting is judged against the default and again, at the write, against the ceiling then in force — the QUIC path's real one, which may still grow with path MTU discovery. `0` removes the adapter's check; the platform still drops what its own ceiling refuses, silently |
| `requireUnreliable` | `true` | fail the handshake unless the server supports datagrams — the platform defaults to `false`, this adapter is made of them |
| `serverCertificateHashes`, `allowPooling`, `congestionControl`, `protocols` | platform defaults | passed through to `new WebTransport(url, options)` |

## Certificates

- **`https://` only.** WebTransport is HTTP/3 over QUIC, which is always TLS:
  there is no plaintext form the way `ws://` is one. The URL must be `https:`
  (the platform throws otherwise) and the server needs a certificate.
- **A self-signed development certificate** is accepted through
  `serverCertificateHashes` — the SHA-256 of the certificate's DER encoding —
  but only one the platform is willing to pin: an X.509v3 certificate whose
  validity period is at most **two weeks** and whose key is **ECDSA P-256**
  — the one algorithm every platform must accept; RSA is refused — on a
  dedicated connection (`allowPooling: false`, the default). Generate such a
  certificate on the server and hand the page the hash:

  ```ts
  const conn = dialWebTransport('https://localhost:4433/rpc', {
    serverCertificateHashes: [{ algorithm: 'sha-256', value: certSha256 }],
  })
  ```

- **In production** use a certificate the browser's trust store already
  accepts, and no hash at all.

## Caveats

- **Browser-only today.** Node 24 has no `WebTransport`; `dialWebTransport`
  throws there and names the way out (construct a session and pass it to
  `new WebTransportDatagramTransport()`). Any runtime that exposes a
  `WebTransport` with the same surface fits the structural type.
- **Teardown comes from the session, not the read loop (§4.5).** The session is
  connection-oriented: `closed` settling — resolved by a graceful close from
  either side, rejected by an abrupt one — and `ready` rejecting each fail the
  `Conn`'s live calls with `UNAVAILABLE`, carrying the close code and reason or
  the error as detail; `conn.close()` closes the session. A close with code 0
  and no reason carries no detail.
- **Loss is not death.** The channel is unreliable, so a peer that vanishes
  without closing is bounded by the core's own timers (T_call, liveness), as on
  UDP; the adapter runs no keepalive and never tears the session down for a
  lost datagram.
- **Oversize datagrams are dropped silently by the platform** — the write
  resolves and nothing is sent. The adapter refuses them first, before the
  platform sees them, with `MessageTooLargeError`, so the call fails
  `RESOURCE_EXHAUSTED` instead of timing out: synchronously against an
  explicit `maxMessageSize` or, while the session is connecting, the 1200 B
  default; and at the write against the session's ceiling then in force.
- **The datagram sink has two shapes, and engines ship either.** The current
  spec has only `createWritable()` — WebKit (Safari 26.4+) ships that and no
  `writable`; Chromium ships only the older `writable` attribute (which is
  what the DOM lib declares); Gecko ships both. The adapter takes
  `createWritable()` where it exists and `writable` otherwise, so
  `dialWebTransport` works on all three; a session offering neither is
  refused at construction. `createWritable()` on a session already closed
  throws `InvalidStateError`, which becomes the `Conn`'s death cause, not a
  construction failure.
- Undecodable datagrams are ignored, and nothing is delivered after the session
  dies; neither tears the connection down (§4.2).
- **Interop.** A TS client here talks to a Go `drpc.Server` over
  `transport/webtransport` on the wire. The cross-language conformance run needs
  a runtime with `WebTransport`, which the Node here lacks — the Go-only suite
  is the bar for this step.
