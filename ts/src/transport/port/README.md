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
GOARCH=wasm` **running inside the page**, with the browser UI as its client, so
a page reload restarts the whole server. Neither end knows about wasm, though:
it is a port, and both could equally be TS endpoints across a `Worker`
boundary.

## Client — `PortTransport`

The `Conn` attaches the transport (`ConnAttacher`): the receive pump starts by
itself — nothing to manage — and the transport owns the port from then on.

```ts
import { Conn } from '@lesomnus/grpc-dgram'
import { PortTransport } from '@lesomnus/grpc-dgram/transport/port'

const worker = new Worker('./server.js', { type: 'module' })
const transport = new PortTransport(worker) // construct it on the same tick
const conn = new Conn(transport)            // reliable mode auto-detected

await conn.invoke(Once, req)

conn.close() // says goodbye, closes the port, fails every live call
```

Construct it promptly after the port exists: a `Worker` drops messages that
arrive before a listener is registered.

When the host knows *why* the endpoint died — the classic case being a wasm
instance that exited or panicked — say so, and every hanging call reports it:

```ts
const go = new Go()
const inst = await WebAssembly.instantiateStreaming(fetch('/server.wasm'), go.importObject)
void go.run(inst.instance).finally(() => transport.close('the wasm instance exited'))
```

For that deployment — a Go server in the page — `startWasmServer` below does
this, and the four other things around it, for you.

## The wasm page — `startWasmServer`

A separate entry, `@lesomnus/grpc-dgram/transport/port/wasm`, because it is
the only part of this adapter that knows what wasm is. It pairs with
`jsport.Gateway.Serve` on the Go side:

```ts
import { Conn } from '@lesomnus/grpc-dgram'
import { startWasmServer } from '@lesomnus/grpc-dgram/transport/port/wasm'

const conn = new Conn(await startWasmServer('/app.wasm'))
```

`globalThis.Go` comes from the toolchain's `wasm_exec.js`, which the page loads
as a classic `<script>`; it is version-coupled to the compiler that produced
the module, so nothing here imports or vendors it.

| Option | Default | Meaning |
|---|---|---|
| `entryPoint` | `'drpcServe'` (`DefaultEntryPoint`) | the global the instance publishes its port-taking function as; must match the name the Go side serves under |
| `go` | `new globalThis.Go()` | a `Go` instance you built yourself — the way to pass argv or env |
| `readyTimeoutMs` | `10_000` | how long to wait for that publish; `<= 0` waits forever |
| `maxMessageSize`, `transfer` | see [Options](#options) | passed to the `PortTransport` it returns |

The source may be a URL (`string` or `URL`), a `Response` or the promise of
one, the module's bytes, or a `WebAssembly.Module` compiled once and
instantiated many times.

### What it does under the hood

1. Fetches and instantiates — streaming where the response says
   `application/wasm`, buffered otherwise, so a server that mislabels the MIME
   type still works. **A non-ok response throws with the body text**: a dev
   server answers a broken build with a 500 whose body is the compiler output,
   and streaming instantiation would discard the one thing that names the line
   that stopped compiling.
2. Installs an accessor on `globalThis[entryPoint]` *before* `go.run`, so the
   publish cannot be missed. **Publishing the entry point is the readiness
   signal** — `js.Global().Set` reaches JS as `Reflect.set`, which triggers an
   accessor — so there is no second magic name on either side, and the property
   is removed again once caught: call it twice and the second start is as clean
   as the first.
3. Races that publish against `go.run()`'s promise and against
   `readyTimeoutMs`. An instance that dies on the way up **rejects** instead of
   hanging: a Go panic *resolves* `go.run()` (wasm_exec exits with code 2), so
   any settlement before readiness is a failure, and with every protocol timer
   off (§10.6) nothing else would ever end the wait.
4. Creates the `MessageChannel`, keeps one end for the returned
   `PortTransport` — constructed before the far end is handed over, so nothing
   the server posts arrives unlistened — and passes the other to the entry
   point.
5. Wires the §4.5 duty: `go.run()` settling closes the transport with a cause
   (`the wasm instance exited`, or the trap), which is the whole reason a live
   call on this channel ever fails. An exited instance keeps its `js.Func`s
   registered on the port it was handed — `os.Exit` detaches nothing — so
   wasm_exec's re-entry point is neutralized first: without that, the goodbye
   posted to a dead instance throws *Go program has already exited* out of an
   event handler, which a page logs and node dies of.

It does **not** own the instance's lifetime: nothing can stop a Go program once
`go.run` has started it. So a failure before that leaves nothing running, and a
failure after it — a module that never publishes — rejects with the instance
still alive, which only a page reload, a `terminate()` or the module's own
shutdown entry point can end. What it does clean up is what it made: the global
property, on every path including the successful one; and the channel, on every
path that does not hand it over — after a handover one end belongs to the
transport and the other to the instance.

`Gateway.Serve` on the Go side takes the same view of the name: it unpublishes
on ctx cancellation only while `globalThis[entryPoint]` still holds what it
published, because by then this helper has normally taken the property back and
restored whatever the page had under it.

Everything else stays on the manual path: for a `Worker`, an iframe or two TS
endpoints, `new PortTransport(port)` on one side and `PortGateway` on the other
is the whole API — a `MessageChannel` is one channel with two *entangled,
symmetric* ends, and the only thing distinguishing them is which one you give
away.

### More than one connection to the same instance

One port is one peer (§6.4), and an instance serves as many as it is given:
each gets its own epoch, sid space, flow-control windows and per-peer resource
caps, and a teardown that reaches only it. `wasmServer()` hands back the
instance `startWasmServer` started, so a second connection costs a line:

```ts
import { startWasmServer, wasmServer } from '@lesomnus/grpc-dgram/transport/port/wasm'

const conn  = new Conn(await startWasmServer('/app.wasm'))
const other = new Conn(wasmServer().connect())   // independent peer, same server
```

`connect()` wires the same §4.5 teardown as the first connection: when the
instance exits, *every* connection to it fails, because they share the process
that stopped existing. It throws once that has happened — a connection to a
corpse would hang forever rather than fail, so the error names which of the two
went wrong.

`openPort()` is `connect()` without the transport: the raw end to transfer
somewhere this page cannot reach.

```ts
const port = wasmServer().openPort()
worker.postMessage({ port }, [port])              // the worker wraps it itself
wasmServer().exited.then(() => worker.terminate())
```

That second line is not optional. A transferred port is out of this page's
reach, and an instance that dies posts no goodbye, so whoever holds the far end
owns its own §4.5 duty — `exited` (which resolves, never rejects, with the
cause) is what the page has to end it with. Give each instance its own
`entryPoint` if you run more than one, and `wasmServer(name)` picks it out.

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

// inside a worker: every client hands us one end of its own MessageChannel
self.addEventListener('message', (ev) => {
  const port = ev.data.port as MessagePort
  gw.bind(port)
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
