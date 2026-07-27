# `@lesomnus/grpc-dgram/transport/port`

dRPC over a **JS message port** — the TS twin of the Go `transport/jsport`
adapter, and wire-compatible with it. **One posted message carries one
marshaled `Envelop`**, byte for byte the WebSocket wire. A port neither loses,
duplicates nor reorders, so the core auto-detects reliable mode and runs with
every protocol timer off (§10.6): plain gRPC semantics between two endpoints in
one process.

A "port" is anything with `postMessage(data)` and a `message` event —
`MessagePort` (both ends of a `MessageChannel`), a `Worker` from the main
thread, `self` inside a dedicated worker. `PortLike` is structural, so all of
them and test mocks fit; **no npm dependency, no node builtin**. It
deliberately does *not* cover `window.postMessage`, whose second argument is a
`targetOrigin` rather than a transfer list — for an iframe, transfer a
`MessagePort` through the window and hand that port here.

The motivating deployment is a Go `drpc.Server` compiled to `GOOS=js
GOARCH=wasm` **running in the browser**, with the UI as its client, so a page
reload restarts the whole server. Neither end knows about wasm, though: it is a
port, and both could equally be TS endpoints across a `Worker` boundary. That
page is [`@lesomnus/grpc-dgram/wasm`](../../wasm)'s two lines and none of this
one's; everything here is what the other shapes — a worker of your own, an
iframe, two TS endpoints — are built from.

## Client — `PortTransport`

The `Conn` attaches the transport (`ConnAttacher`): the receive pump starts by
itself — nothing to manage — and the transport owns the port from then on.

```ts
import { Conn } from '@lesomnus/grpc-dgram'
import { PortTransport } from '@lesomnus/grpc-dgram/transport/port'

const transport = new PortTransport(port) // an end you kept, or a worker's self
const conn = new Conn(transport)          // reliable mode auto-detected

await conn.invoke(Once, req)

conn.close() // says goodbye, closes the port, fails every live call
```

Construct it promptly after the port exists: a `MessagePort` queues what
arrives until someone starts it, but a `Worker` — or a worker's own `self` —
drops every message posted before a listener is registered, which is why
`dialWorker` below transfers a port instead of wrapping the worker.

When the host knows *why* the endpoint died — the classic case being a wasm
instance that exited or panicked — say so, and every hanging call reports it:

```ts
const go = new Go()
const inst = await WebAssembly.instantiateStreaming(fetch('/server.wasm'), go.importObject)
void go.run(inst.instance).finally(() => transport.close('the wasm instance exited'))
```

For a wasm instance, [`open()`](../../wasm) does that wiring — and the rest of
the handshake around it — for you.

## A worker — `dialWorker`

One connection to a worker that is already running: a fresh `MessageChannel`,
one end transferred, a **`Conn`** over the other. `dial`, because the peer
exists — what starts one is [`open()`](../../wasm), below.

```ts
import { dialWorker } from '@lesomnus/grpc-dgram/transport/port'

const worker = new Worker('./server.js', { type: 'module' })
const conn = dialWorker(worker)                  // call again for another peer
```

Its options are one bag with no key in common between the halves:
`maxMessageSize`, `transfer` and `message` are the adapter's, everything
`ConnOptions` declares is the core's. When you need the transport itself, or
the port reaches the peer by some other road, build the pair explicitly — a
`MessageChannel`, `new PortTransport(ch.port1, opts)`, the transfer, `new
Conn(tx, opts)` — which is exactly what this one line does.

Why a transferred port rather than `new PortTransport(worker)`: a `MessagePort`
**queues** everything posted into it until the far side binds it, so a call
opened on this very tick is delivered late rather than dropped, where a
worker's own global scope is wired through `onmessage` and loses whatever
arrives before its handler is registered. It is also what makes a second
connection possible at all — the worker object itself is one channel shared by
everything on it, while each transferred port is its own peer (§6.4).

The port arrives at the worker as `ev.ports[0]`, alongside `{ drpc: 'serve' }`
— the message the [shipped wasm worker](../../wasm) answers, and one your own
worker can ignore entirely (the `message` option sets it to anything else you
like). The worker is never terminated here, not even when the transport dies:
killing a worker is the host's decision, taken after its endpoints have torn
down.

## The wasm page — `open()`

A Go server compiled to `js/wasm` has its own entry,
[`@lesomnus/grpc-dgram/wasm`](../../wasm), because it is the only part of this
that knows what wasm is — and it ships the worker, so the page is two lines:

```ts
import { open } from '@lesomnus/grpc-dgram/wasm'

const sock = await open('/app.wasm')
const conn = sock.dial()      // again for a second, independent peer (§6.4)
```

It pairs with `jsport.Gateway.Serve` on the Go side: the readiness handshake,
the instantiation, the channel per connection and the §4.5 teardown a dying
instance cannot perform for itself are all in there, with the reasoning.

## The manual path

For an iframe, a worker of your own on both ends, or two TS endpoints in one
page, `new PortTransport(port)` on one side and `PortGateway` on the other is
the whole API. A `MessageChannel` is one channel with two *entangled,
symmetric* ends — what is posted into one arrives at the other, and the only
thing distinguishing them is which one you give away:

```ts
const ch = new MessageChannel()
const conn = new Conn(new PortTransport(ch.port1)) // the end you keep
iframe.contentWindow.postMessage({ drpc: 'serve' }, targetOrigin, [ch.port2])
```

`window.postMessage` is not a port — its second argument is a `targetOrigin`,
not a transfer list — so an iframe is exactly this: transfer a `MessagePort`
through the window, and hand *that* port to the two APIs. Either way the wire
is unchanged; only who creates the channel moves.

What the manual path owes, and the helpers do for you, is the §4.5 teardown:
whatever the host knows and the port cannot report — an instance that exited, a
worker about to be terminated, a page unloading — has to reach `close(cause)`,
or the calls in flight hang forever.

## Server — `PortGateway`

One `Server` serving many peers, one port each; the peer key is a fresh opaque
counter per port (§6.4). `bind` where the port arrives so no early message is
lost; `servePeer` runs until the peer goes away, then deregisters it and calls
`server.disconnectPeer` — failing that peer's live calls.

```ts
import { Server } from '@lesomnus/grpc-dgram'
import { PortGateway } from '@lesomnus/grpc-dgram/transport/port'

const gw = new PortGateway()
const server = new Server(gw)
// server.register(...)

// inside a worker: every client transfers one end of its own MessageChannel,
// which arrives on the event rather than in it — a MessagePort cannot be cloned
self.addEventListener('message', (ev) => {
  const port = ev.ports[0]
  if (port === undefined) return  // somebody else's traffic on this channel
  gw.bind(port)                   // synchronously: nothing queued is lost
  void gw.servePeer(server, port) // resolves with the death cause
})
```

`gw.close()` says goodbye on every served port — what a server runs before its
instance exits, so clients fail fast instead of waiting on a peer that is no
longer there.

## The goodbye: an empty message

There is no socket to die here, so **death has to be said out loud**. A 0-byte
message decodes to an `Envelop` with zero frames, which the wire never
otherwise carries (§4.1 says 1..n), so it is free to mean *this endpoint is
going away*. `close()` on either role posts one, best effort, before closing the
port; the peer that reads it treats it as EOF, stops its pump and performs the
§4.5 teardown — `conn.close()` / `server.disconnectPeer()`.

What means EOF is the *empty message*, recognized by its byte count — not an
envelop that merely decoded to no frames. `decodeEnvelop` skips envelop fields
it does not know, so a v1.2 extension, or another library's protobuf sharing
the port, would otherwise be read as a close frame and tear a healthy channel
down; anything non-empty that carries no frames is dropped like the malformed
input it is (§4.2).

This is the equivalent of the WebSocket close handshake, and it is the only
reason a peer that goes away does not leave live calls hanging forever: in
reliable mode there are no protocol timers, so the adapter's teardown is the
*only* mechanism that ever unblocks a call. A `close` event on the port is
wired too, where the runtime fires one — but nothing forces one to exist, which
is why the goodbye, and `close(cause)` from the host, are the load-bearing
signals.

There is deliberately **no keepalive**: two endpoints in one process cannot be
partitioned, and an unanswered ping would only measure how busy the peer is.

## Options

| Option | Default | Meaning |
|---|---|---|
| `maxMessageSize` | `0` (unlimited) | bound on sends; structured clone has no protocol ceiling, so nothing is refused by default (§4.4). Past it, the send is refused before it reaches the port and the call fails `RESOURCE_EXHAUSTED` |
| `transfer` | `true` | hand the message buffer to the peer instead of copying it. The adapter allocated it and never reads it again; a port that refuses transfer lists falls back to a plain post |

## Caveats

- **Teardown is the whole point.** With protocol timers off, the *only* thing
  that unblocks live calls is the goodbye — or `close(cause)` — reaching the
  §4.5 teardown. Wire `close()` to whatever the host knows: a wasm instance
  exiting, a worker being terminated, a page unloading.
- **A `Worker` is never terminated by this adapter.** `terminate()` aborts the
  worker at once, discarding the goodbye still queued for it; killing a worker
  is the host's decision, taken after its endpoint has torn down. A worker's
  own `self` is never `close()`d either — that call ends the worker just as
  surely, and one peer's teardown must not take the instance with it. What
  teardown does owe a port it no longer owns is releasing it: the listeners
  come off on `close()`, so a `Worker` or a `self` that outlives the endpoint
  does not keep it (rx buffer included) alive.
- **Keep the wire.** Marshaled bytes across a wasm boundary are 2.5–3× faster
  than building the equivalent JS object graph field by field — one memcpy
  beats thirty host calls — besides being what makes the two implementations
  interoperable.
- **No backpressure on `postMessage`**, so inbound messages queue in the
  adapter without bound. That is safe for the same reason the WebSocket
  adapter's rx queue is: in reliable mode a conforming peer cannot put more in
  flight than the per-stream windows it was granted (§4.2.1). A received frame
  is never dropped — a gap in reliable mode is a protocol error, not a lost
  datagram.
- A message that is not a binary envelop (a string, another library's object
  sharing the port), an undecodable one, and a `messageerror` are all ignored;
  none of them tears the channel down (§4.2).
- **If you write your own glue in Go/wasm**, the `message` callback must
  *enqueue and return*: a `js.FuncOf` callback that blocks holds the JS thread
  for exactly as long as it blocks (a 50 ms park freezes the event loop for
  50 ms). Delivery to the core belongs on a separate goroutine — see
  `transport/jsport`.
