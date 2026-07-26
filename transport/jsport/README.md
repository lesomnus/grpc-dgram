# transport/jsport

dRPC over a JS message port: **one posted message carries one marshaled
`Envelop`** as a `Uint8Array` — byte for byte the WebSocket wire, so a peer
here and a peer behind [`transport/gorilla`](../gorilla) speak the same
protocol. A port neither loses, duplicates nor reorders, so the core
auto-detects reliable mode and runs with every protocol timer off: plain gRPC
semantics between two things that share a JS event loop.

A **message port** is anything with `postMessage(data)` and a `message`
event — both ends of a `MessageChannel`, a `Worker` seen from the main thread,
the dedicated worker global scope (`self`) seen from inside it.
`window.postMessage` deliberately does *not* qualify: its second argument is
`targetOrigin`, not a transfer list. For an iframe, transfer a `MessagePort`
through the window and hand that port here.

Part of the core module and `js/wasm` only (`//go:build js && wasm`):
`syscall/js` is stdlib, so importing this pulls no third-party dependency, and
on any other GOOS the package is silently skipped by `go build ./...`.

```go
import "github.com/lesomnus/grpc-dgram/transport/jsport"
```

The twin on the other side of the port is
[`ts/src/transport/port`](../../ts/src/transport/port); either end may be Go or
TypeScript.

## The deployment this exists for

A Go `drpc.Server` compiled to `GOOS=js GOARCH=wasm` **running in the
browser**, with the UI as its client. The service is real gRPC — generated
stubs, interceptors, streaming, deadlines — but nothing leaves the tab, and a
page reload restarts the whole server. Where the instance runs is the host's
choice and changes nothing here: the TypeScript entry starts it in a Worker, so
that a handler which computes does not freeze the page, and both ends could
equally be two TypeScript endpoints.

## Server

One `Gateway` serves many ports, one peer each. `Serve` is the whole of it:

```go
gw := jsport.NewGateway()
srv := drpc.NewServer(gw)
pb.RegisterEchoServiceServer(srv, &myHandler{})

// Publishes globalThis.drpcServe(port) and serves every port handed to it.
// It blocks, which is also what keeps main from returning.
log.Fatal(gw.Serve(context.Background(), srv))
```

**Publishing the entry point is the readiness signal**, and it is the only one.
`js.Global().Set` reaches JS as `Reflect.set(globalThis, name, fn)`, which
triggers an accessor property — so a host that defines one before `go.run()` is
woken by the assignment itself: no `drpcReady` callback, no second name, nothing
to poll. Which is why nothing may be published before the server can serve:
register every service first (`PROTOCOL.md` §13), then call `Serve`.

The host's half is [`ts/src/wasm`](../../ts/src/wasm)'s `open()`, and from the
page it is two lines — the worker it runs in comes with the package:

```js
import { open } from '@lesomnus/grpc-dgram/wasm'

const sock = await open('/app.wasm')  // resolves when this Serve publishes
const conn = sock.dial()              // one port, one peer
```

What that hides, for a host writing its own:

```js
const ready = new Promise((resolve) => {
  let fn
  Object.defineProperty(globalThis, 'drpcServe', {
    configurable: true,
    get: () => fn,                       // undefined until the publish, or Serve
    set: (v) => { fn = v; resolve(v) },  // would read the accessor as "taken"
  })
})
const run = go.run(instance)  // the property appears when the server can serve
// Raced, not awaited: an instance that dies on the way up resolves run() — a Go
// panic does too — and waiting on `ready` alone would park forever.
const serve = await Promise.race([ready, run.then(() => { throw new Error('the wasm instance exited before publishing') })])

const ch = new MessageChannel()
const transport = new PortTransport(ch.port1) // the end you keep, listening first
serve(ch.port2)                               // the end you give away: one port is one peer
const conn = new Conn(transport)
```

Each call binds the port **synchronously**, before the callback returns, so a
client that opens a call on that same tick loses nothing; serving then runs on
its own goroutine, because a `js.FuncOf` that blocks holds the JS event loop for
exactly as long as it blocks. One port is one peer (§6.4), so calling the entry
point again with another port serves a second peer — a Worker, another tab's
channel — off the same handlers, each with its own epoch, sid space, windows
and per-peer caps (§15), and a teardown that reaches only it. On the page that
second call is a second `sock.dial()`. A call carrying no port is ignored
rather than fatal: `args[0]` unguarded panics, and a panic takes the whole
instance down over what is only unreadable input (§4.2). What counts as a port is the duck
type the adapter needs — an object with a callable `postMessage` — so
`drpcServe(ev)` in place of `drpcServe(ev.data.port)` is dropped too, instead of
becoming a peer that can never receive anything.

`WithEntryPoint("name")` changes the global. `Serve` refuses a name that is
already set rather than steal another server's entry point, and on ctx
cancellation it unpublishes the global, releases the `js.Func` and closes the
gateway — so every peer gets the goodbye and its §4.5 teardown. It unpublishes
only while the property still holds what it published: catching the publish is
what a host does with the name, not a lease on it, and `open()` takes the
property straight back off `globalThis` once it has caught it.

### When the host hands ports over some other way

`Bind` + `ServePeer` is the pair `Serve` is built from, and what to use inside a
Worker (the port is `js.Global()` itself), behind your own JS glue, or when
ports arrive from somewhere that has no entry point to call:

```go
js.Global().Set("drpcAttach", js.FuncOf(func(_ js.Value, args []js.Value) any {
    port := args[0]
    gw.Bind(port)                                     // buffering starts here
    go gw.ServePeer(context.Background(), srv, port)  // blocks until the peer goes away
    return nil
}))

// main must never return: a returned main kills the instance and every
// registered js.Func with it.
select {}
```

`Bind` is what makes an early message safe. A `MessagePort` queues what arrives
until `start()` and loses nothing on its own, but a port wired through
`onmessage` — the worker global scope, any hand-rolled shim — drops every
message posted before the handler is set, and a client that opens a call the
instant it hands the port over is the normal case. `ServePeer` blocks until the
peer says goodbye, so it needs its own goroutine; on every exit it calls
`srv.DisconnectPeer`.

## Client

```go
conn := drpc.NewConn(jsport.New(port)) // reliable mode auto-detected via TransportInfo
client := pb.NewEchoServiceClient(conn)

// shutdown — one call closes the conn, the transport, and the port:
conn.Close(nil)
```

`drpc.NewConn` attaches the transport (`drpc.ConnAttacher`): the pump starts by
itself — no goroutine to manage — and the transport owns the port from then on.

## The goodbye: an empty message

There is no socket to die here, so death has to be said out loud. **A 0-byte
message is this adapter's close frame.** It is a marshaled `Envelop` with zero
frames, which the wire never otherwise carries (`PROTOCOL.md` §4.1 says 1..n),
so it is free to mean *this endpoint is going away*. `Close` posts one; a
receiver that reads an empty message treats it as EOF, and the §4.5 teardown
runs — `Conn.Close` on the client, `Server.DisconnectPeer` on the server.

The close frame is the *empty message*, not merely one that decodes to no
frames: protobuf keeps fields it does not know, so a later envelop extension —
or two bytes of another library's traffic on the same port — decodes to zero
frames as well, and reading any of those as EOF would tear a healthy channel
down over input §4.2 says to drop. Both halves check the byte length;
[the TypeScript twin](../../ts/src/transport/port) does the same.

This is what WebSocket's close handshake is, and it is the only reason a peer
that goes away does not leave live calls hanging forever: with protocol timers
off, nothing else would ever fail them.

The port cannot report every death by itself, so the host tells it the things
only the host knows — a wasm instance that exited or panicked, a terminated
worker, a page teardown:

```js
const go = new Go()
const { instance } = await WebAssembly.instantiate(module, go.importObject)
const ch = new MessageChannel()
const transport = new PortTransport(ch.port1)   // ts/src/transport/port
const conn = new Conn(transport)

go.run(instance).then(
  () => transport.close(new Error('the wasm instance exited')),
  (e) => transport.close(e),
)
globalThis.drpcAttach(ch.port2)
```

`open()` wires that `then` for you — it is the whole reason a live call on this
channel ever fails when the instance dies — and where the instance runs in a
worker it is the only half that can: `go.run()`'s promise is not visible from
the page, so the worker says the goodbye on every port it holds and reports the
death, and every `Conn` dialled through the sock fails with the cause. The code
above is what you write when you own the instantiation yourself.

On the Go side the same hook is `Transport.Close` and `Gateway.Close`; the
latter says goodbye to every bound port at once, which is what a server about
to leave owes its peers. Neither takes a cause — `Close` is an `io.Closer` —
so a Go host with something to report says it through the core's own teardown
API instead, `conn.Close(err)` or `srv.DisconnectPeer(peer, err)`, and the
failed calls carry it. That is the equivalent of TypeScript's `close(cause)`. A `close` event on the port counts as death too where
the runtime fires one (newer `MessagePort`); `messageerror` does not — a
message that cannot be deserialized is malformed input, dropped, never a
teardown.

## Options

| Option | Default | Meaning |
|---|---|---|
| `WithMaxMessageSize(n)` | 0 (unlimited) | largest marshaled `Envelop` this endpoint will **send**; structured clone has no protocol ceiling, so set it only for a path that caps message size |
| `WithTransfer(v)` | `true` | hand the message's `ArrayBuffer` over on the transfer list instead of copying it — safe because the adapter allocates it per message |
| `WithLabel(s)` | `"port"` | what `Peer()` reports, and the address handlers read through `peer.FromContext`; a port has no address of its own |
| `WithEntryPoint(name)` | `"drpcServe"` | the global `Gateway.Serve` publishes; the name is the entire handshake with the host, so change it on both sides — and only when one realm runs two servers |

## Writing your own glue

**A `js.FuncOf` callback must not block.** It *can* — it will return
correctly — but it does so by holding the JS thread: a 50 ms park inside a
callback freezes the JS event loop for 50 ms, and with it every other consumer
of that loop. This adapter's `message` listener therefore copies out, enqueues
and returns, and a separate pump goroutine delivers into the core, where
blocking is allowed and correct in reliable mode. Anything you write on the
boundary owes the same discipline.

Two more js/wasm rules: `main` must never return, and every `js.Func` must be
`Release`d — `Close` does that for the ones this package registers, after
detaching them from the port, because a func released while JS can still reach
it turns every later call into a console error and a silently lost message.

## Running the tests

They drive a real `MessageChannel` with both ends in one wasm instance, under
node:

```sh
GOOS=js GOARCH=wasm go test -exec="$(go env GOROOT)/lib/wasm/go_js_wasm_exec" ./transport/jsport/...
```

Anything that waits for a port delivery must yield to JS — ordinary blocking Go
calls do exactly that; a busy spin wedges the process.

## Caveats

- **Teardown is the whole point.** With protocol timers off, the *only*
  mechanism that unblocks live calls is the goodbye above (or an explicit
  `Close`). An endpoint that vanishes without either — a worker killed with
  `terminate()`, a tab closed mid-call — leaves its peer's calls hanging until
  that peer's own deadlines fire, so give every call a deadline or watch the
  instance from the host side.
- **No keepalive, deliberately.** Two endpoints in one process cannot be
  partitioned, and an unanswered ping would only measure how busy the peer is.
- **`postMessage` applies no backpressure**, and that is fine: in reliable mode
  per-stream flow control (§4.2.1) bounds what a conforming peer can put in
  flight, so the receive queue cannot grow without limit. A received message is
  never dropped — in reliable mode a gap is a protocol error, not a lost
  datagram.
- **`Close` closes the port.** On a `MessagePort` that detaches it; on a
  dedicated worker's global scope `close()` ends the worker — which is what
  "this endpoint is going away" means there, but do not call it expecting the
  worker to survive.
- Messages that are not `ArrayBufferView`s, views that do not decode as an
  `Envelop`, and non-empty views that decode to no frames are all ignored: a
  page may post its own traffic down the same port, and frame-level errors
  never tear the channel down (§4.2).
- **The port is as trustworthy as the code on its other end.** There is no
  authentication and nothing to encrypt — the bytes never leave the process —
  but a port handed to untrusted code is a fully privileged client of your
  server (see `PROTOCOL.md` §15).
