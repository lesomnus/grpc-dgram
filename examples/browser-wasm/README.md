# browser-wasm

**A Go server whose UI you develop by running the real server inside the
page.** `GET /app.wasm` compiles `./wasm` — the same `todo.TodoService` this
process serves — and the page starts it on a `MessageChannel`, so a browser
reload restarts the whole server and, with `-rebuild`, recompiles it first
(about half a second). The Go side is [`transport/jsport`](../../transport/jsport),
the page is the TypeScript port's [`transport/port`](../../ts/src/transport/port),
and the wire between them is dRPC v1.1 — the same bytes the WebSocket endpoint
at `/rpc` carries.

That endpoint is the second half of the argument: `?server=ws` points the page
at the server *process* instead, and not a line of the UI code changes. The
in-page server is not a mock of the server; it is the server.

## Run it

```sh
# 1. build the TypeScript port (the page imports its dist/ output)
cd ts && pnpm install && pnpm build

# 2. serve the page, build the in-page server on demand, and answer /rpc
cd ../examples/browser-wasm && go run .

# 3. open http://127.0.0.1:8080
```

Add a task, tick it, remove it: every mutation comes back over the `Watch`
server stream, which is what the list renders from. Add with an empty title for
`INVALID_ARGUMENT`, press **Toggle #999** for `NOT_FOUND` — real statuses from a
real handler. The line under the heading names whoever answered `List`
(`in-page (wasm)`, or the process and its pid).

Then reload the page: the seeded list is back, because that is a new server.
Follow **Talk to the server process instead** and the same UI runs against
`ws://127.0.0.1:8080/rpc`, where the state survives the reload and is shared
between tabs.

`pnpm build` is required because `@lesomnus/grpc-dgram` is not published: the
page imports the build output, mounted by this server at `/ts/dist/` and named
by the import map in `web/index.html`.

## What it demonstrates

- **A reload is a server restart.** Starting it is two lines, one on each side:

  ```js
  // web/main.js — a client on a channel to the server this page just started
  const transport = await startWasmServer('/app.wasm')
  const conn = new Conn(transport, connOptions)
  ```

  ```go
  // wasm/main.go — publish the entry point, then serve every port handed to it
  log.Fatal(gw.Serve(context.Background(), srv))
  ```

  Nothing survives the reload, which is the point — the UI loop is the
  browser's own, against handlers that really run.
- **…and a rebuild, when you want one.** With `-rebuild` (the default) every
  `GET /app.wasm` runs `GOOS=js GOARCH=wasm go build ./wasm` and logs how long
  it took: measured here, 0.45 s for the first request, 0.5–0.7 s after editing
  a handler, 0.09 s when nothing changed (the Go build cache does the work — a
  cold cache pays for grpc-go once). A build failure is answered as a 500 carrying the
  compiler output, so a handler that stopped compiling shows up in the browser
  rather than as silence.
- **One wire, two channels, one UI.** `connect()` is the only place in
  `web/main.js` that decides anything: it returns a `Conn` over a
  `PortTransport` or over `dialWebSocket`, and everything after it —
  descriptors, calls, the `Watch` loop, the error rendering — is written once.
  The only other mentions of `?server=ws` are the link that toggles it and the
  status line that names who answered. Both adapters report
  `reliable`, so both cores run with every protocol timer off (§10.6) and the
  page never sees a datagram caveat.
- **The teardown wiring, and why it is mandatory.** A message port has no death
  to detect: with timers off, the *only* thing that can fail a live call is the
  adapter's §4.5 teardown. So the host tells the transport what only the host
  knows, which is what `startWasmServer` is doing on the page's behalf —

  ```js
  run.then(
    () => transport.close(new Error('the wasm instance exited')),
    (err) => transport.close(err),
  )
  ```

  — and a server that exits or panics fails the calls in flight instead of
  hanging the UI forever. In the other direction the goodbye (an empty message)
  does the same job for a peer that leaves cleanly. Nothing in `web/main.js`
  has to remember this; anything on the manual path below does.
- **What does not follow a server into the page.** Sockets, files, databases.
  `todo.Store` is that seam, and it is the only thing the two builds could
  differ in: the handlers, their validation, their statuses and their streaming
  are the real code either way. In a real project this is where you pay —
  IndexedDB, a stub, or a fixture — and the bill stops there.
- **A page with no build step.** `web/main.js` is plain ES modules with
  `// @ts-check` and JSDoc types — no bundler, no framework. It has no protobuf
  runtime, so it names the `"json"` codec on its OPEN frame (§12) and both
  servers marshal with `protojson` (`jsoncodec/`), keeping their generated
  stubs. Note what that costs the page: protojson omits zero values, so every
  field arrives optional.

## What the helper does, and the channel it hides

Those two lines hide exactly one thing, a `MessageChannel`, and it is worth
spelling out because it is what everyone asks about. A channel has **two ends,
entangled and symmetric**: what you post into one comes out of the other, and
nothing distinguishes `port1` from `port2` except which one you give away. You
keep one and hand the server the other —

```js
const channel = new MessageChannel()
const transport = new PortTransport(channel.port1) // the end you keep
globalThis.drpcServe(channel.port2)                // the end you give away
```

— and since that is the same three lines every time, `startWasmServer` makes
the channel itself: `port1`/`port2` never appear in `web/main.js`. Around it,
it does the four things the hand-written version has to get right — reading a
non-ok response's body, so a broken build reports the compiler output instead
of a MIME-type complaint; installing an accessor on `globalThis.drpcServe`
*before* `go.run`, so the publish that `Gateway.Serve` performs cannot be
missed; racing that publish against the instance dying on the way up (a Go
panic *resolves* `go.run()`, so a settled run is a failure, not a success); and
the teardown wiring above. The
[adapter's README](../../ts/src/transport/port/README.md#the-wasm-page--startwasmserver)
is the long version.

You hold the ports yourself as soon as the server is not a wasm instance in
this page. For a `Worker` there is no channel to make at all — a `Worker` and
the `self` inside it are already the two ends of one:

```js
// the page
const conn = new Conn(new PortTransport(new Worker('./server.js', { type: 'module' })))
```

```js
// inside the worker
const gw = new PortGateway()
const server = new Server(gw)
// server.register(…)
gw.bind(self)
void gw.servePeer(server, self)
```

For an iframe, `window.postMessage` is not a port — its second argument is a
`targetOrigin`, not a transfer list — so make a `MessageChannel`, transfer one
end through the window, and hand *that* port to the same two APIs. Either way
the wire is unchanged; only who creates the channel moves.

## The size of the thing

The in-page server is a whole Go runtime plus grpc-go. Measured on this
checkout with go1.26.4:

| build | raw | `gzip -9` |
|---|---|---|
| `go build` | 21.0 MB | 4.64 MB |
| `-ldflags="-s -w" -tags grpcnotrace` | 18.8 MB | 4.19 MB |

On localhost that is a non-issue — it is the 0.09 s at the end of the numbers
above. Shipping it to users is a real decision, and one this example does not
pretend to make for you: strip it, serve it compressed, cache it across
reloads, or keep the in-page server for development and deploy only the
WebSocket path. Nothing in the page changes either way.

## Type-checking the page

The page is JavaScript, so there is nothing to compile — but it is checked
against the port's own `.d.mts` types:

```sh
cd ts && pnpm exec tsc -p ../examples/browser-wasm
```

`tsconfig.json`'s `paths` entries stand in for the import map, pointing the
package name at `ts/dist`.

## Files

| | |
|---|---|
| `main.go` | the dev server: the page, `/ts/dist/`, `/wasm_exec.js`, `/app.wasm` built on demand, and the `/rpc` WebSocket |
| `wasm/main.go` | the same service compiled for the page: a `jsport.Gateway`, and `Serve` for the whole of its JS surface |
| `todo/service.go` | the `TodoService` handlers — ordinary gRPC code |
| `todo/store.go` | the `Store` seam and its in-memory implementation |
| `jsoncodec/jsoncodec.go` | the `"json"` wire codec, imported by both mains, so the page needs no protobuf runtime |
| `web/index.html` | the page, the import map, and the styling |
| `web/main.js` | the browser client: descriptors, the two `connect()` paths, the UI |
| `proto/todo.proto`, `todopb/` | the service and its checked-in bindings |

The JS surface of the wasm build is one global, published by `Gateway.Serve`
and named by neither side's source (`jsport.DefaultEntryPoint` on one,
`DefaultEntryPoint` on the other):

| | |
|---|---|
| `globalThis.drpcServe(port)` | hand over one end of a `MessageChannel`. Its *appearance* is the readiness signal — the page waits for the property, so there is no ready callback anywhere — and one port is one peer, so calling it again serves another client (a Worker, another tab's channel) off the same handlers |

Regenerate the bindings (needs [buf](https://buf.build); the generated files
are committed, so running the example does not):

```sh
buf generate examples/browser-wasm/proto --template examples/browser-wasm/buf.gen.yaml   # from the repo root
```

## Flags

| Flag | Default | |
|---|---|---|
| `-addr` | `127.0.0.1:8080` | HTTP address for the page, `/app.wasm` and the WebSocket endpoint |
| `-rebuild` | `true` | rebuild `./wasm` on every `GET /app.wasm`. With `-rebuild=false` the prebuilt `web/app.wasm` is served instead — build it with `GOOS=js GOARCH=wasm go build -o web/app.wasm ./wasm` |

## Notes

- `wasm_exec.js` is served from `go env GOROOT`, not committed here: it is the
  JS half of the Go runtime and is version-coupled to the compiler that built
  the module, so a copy next to the page would keep working right up until the
  toolchain is upgraded.
- Everything is answered `Cache-Control: no-store`. A cached `app.wasm` or a
  cached `ts/dist` would quietly break the one claim the example makes.
- An in-page server is a server with no trust boundary in front of it:
  the port is exactly as trustworthy as the code holding its other end, and the
  page holds both (`PROTOCOL.md` §15). The `/rpc` endpoint is plain `ws://` on
  loopback for the same reason the other examples are — deploy `wss://`.
- Serving the port's `dist/` over HTTP is what an unpublished package costs.
  Once it is published the import map disappears and the page imports
  `@lesomnus/grpc-dgram` from a CDN or a bundle like any other dependency.
