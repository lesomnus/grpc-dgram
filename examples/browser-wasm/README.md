# browser-wasm

**A Go server whose UI you develop by running the real server in the browser.**
The page-side half of that is two lines:

```js
const sock = await open('/app.wasm')      // a Go drpc.Server, in a Worker
const conn = sock.dial(connOptions)       // ready to call
```

`GET /app.wasm` compiles `./wasm` — a `todo.TodoService` server — and
[`open()`](../../ts/src/wasm) starts it in the Worker the package ships, so a
browser reload restarts the whole server and, with `-rebuild`, recompiles it
first (about half a second). The Go side is
[`transport/jsport`](../../transport/jsport), and it is the same length:

```go
gw := jsport.NewGateway()
srv := drpc.NewServer(gw)
todopb.RegisterTodoServiceServer(srv, impl)
log.Fatal(gw.Serve(context.Background(), srv))
```

The wire between them is dRPC v1.1 — the same bytes a WebSocket carries, one
marshaled `Envelop` per posted message (§4.1). And the server is not a mock of
the server: `todo/` implements `todopb.TodoServiceServer` with generated stubs,
request validation, real statuses and a server-streaming method, and nothing in
it is browser-specific — `GOOS=js GOARCH=wasm` is the entire difference between
this and a process. Serving handlers like these from a process, over a socket,
is [`websocket-echo`](../websocket-echo); this example does not repeat it.

## Run it

```sh
# 1. build the TypeScript port (the page imports its dist/ output)
cd ts && pnpm install && pnpm build

# 2. serve the page, and build the wasm server on demand
cd ../examples/browser-wasm && go run .

# 3. open http://127.0.0.1:8080
```

Add a task, tick it, remove it: every mutation comes back over the `Watch`
server stream, which is what the list renders from. Add with an empty title for
`INVALID_ARGUMENT`, press **Toggle #999** for `NOT_FOUND` — real statuses from a
real handler. The line under the heading reads `served by the js/wasm build —
in a Worker, reliable mode`. Its first half is the server's own answer to
`List` (`served_by` in `proto/todo.proto`): which *build* answered, observed
rather than assumed, and the page renders what it was told. The second half is
the page's, because where that build runs is the page's own doing.

Then reload the page: the seeded list is back, because that is a new server.

`pnpm build` is required because `@lesomnus/grpc-dgram` is not published: the
page imports the build output, mounted by this server at `/ts/dist/` and named
by the import map in `web/index.html`.

## What it demonstrates

- **A reload is a server restart.** Starting it is two lines, one on each side:

  ```js
  // web/main.js — a client on a connection to the server this page just started
  const sock = await open('/app.wasm')
  const conn = sock.dial(connOptions)
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
  cold cache pays for grpc-go once). A build failure is answered as a 500
  carrying the compiler output, and `open()` rejects with that text rather than
  with the MIME-type complaint streaming instantiation would produce — so a
  handler that stopped compiling shows up in the browser rather than as
  silence.
- **The teardown wiring, and why it is mandatory.** A message port has no death
  to detect: with timers off, the *only* thing that can fail a live call is the
  adapter's §4.5 teardown. The page cannot perform it — from where it stands
  there is no `go.run()` promise to watch, only a Worker that has gone quiet —
  so the shipped worker posts the goodbye (an empty message) on every port it
  holds the moment its instance dies, and `open()` fails every `Conn` dialled
  through the sock with the cause. A server that exits or panics therefore
  fails the calls in flight instead of hanging the UI forever. Nothing in
  `web/main.js` has to remember this; anything on the manual path below does.
- **What does not follow a server into the browser.** Sockets, files,
  databases. `todo.Store` is that seam, and it is the only thing an in-page
  build could differ in: the handlers, their validation, their statuses and
  their streaming are the real code either way. In a real project this is where
  you pay — IndexedDB, a stub, or a fixture — and the bill stops there.
- **A page with no build step.** `web/main.js` is plain ES modules with
  `// @ts-check` and JSDoc types — no bundler, no framework. It has no protobuf
  runtime, so it names the `"json"` codec on its OPEN frame (§12) and the
  server marshals with `protojson` (`jsoncodec/`), keeping its generated stubs.
  Note what that costs the page: protojson omits zero values, so every field
  arrives optional.

## Three things those two lines settle

**Why a Worker.** Go's scheduler shares whatever thread it runs on, so a
handler that computes for 50 ms freezes the page for 50 ms if that thread is
the main one — and this one is a server, with a stream fanning every mutation
out to every watcher. Off the main thread the UI stays live and the only thing
crossing the boundary is bytes. `open(app, { worker: false })` runs the
instance in this realm instead, which is what node and `ts/test/wasm.test.ts`
do; nothing else about the API changes.

**Why `/wasm_exec.js` comes from `go env GOROOT`.** It is the JS half of the Go
runtime and is version-coupled to the compiler that built the module, so
neither this example nor the package vendors a copy — a vendored one would pin
the wrong compiler. The dev server serves the toolchain's, and the worker
fetches it from `/wasm_exec.js`. In a real deployment that is one line of your
build:

```sh
cp "$(go env GOROOT)/lib/wasm/wasm_exec.js" ./public/
```

Serve it elsewhere and say so: `open(app, { wasmExec: '/vendor/wasm_exec.js' })`.

**Why `dial()` returns the connection and `open()` does not.** One port is one
peer (`PROTOCOL.md` §6.4), so a second `dial()` is a second *independent* peer
of the same server — its own epoch, sid space, flow-control windows and
per-peer caps, and a teardown that reaches only it. This page needs one; a page
with a second component, or one handing a port on to a worker of its own, calls
it again. It is synchronous because a transferred `MessagePort` queues
everything posted into it until the far side binds it, so the `List` on the
next line cannot be too early.

Every other shape — a `Worker` you wrote, an iframe, two TS endpoints — is the
manual path in the [adapter's README](../../ts/src/transport/port/README.md),
unchanged: `new PortTransport(port)` on one side, `PortGateway` on the other,
and the §4.5 wiring above is then yours to write.

## The size of the thing

The wasm server is a whole Go runtime plus grpc-go. Measured on this checkout
with go1.26.4:

| build | raw | `gzip -9` |
|---|---|---|
| `go build` | 21.0 MB | 4.64 MB |
| `-ldflags="-s -w" -tags grpcnotrace` | 18.8 MB | 4.19 MB |

On localhost that is a non-issue — it is the 0.09 s at the end of the numbers
above. Shipping it to users is a real decision, and one this example does not
pretend to make for you: strip it, serve it compressed, cache it across
reloads, or conclude that this service belongs on a machine after all. Only
`connect()` would change — everything past it is written against a `Conn`.

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
| `main.go` | the dev server: the page, `/ts/dist/`, `/wasm_exec.js`, and `/app.wasm` built on demand. It serves files; it does not serve the service |
| `wasm/main.go` | the same service compiled for the browser: a `jsport.Gateway`, and `Serve` for the whole of its JS surface |
| `todo/service.go` | the `TodoService` handlers — ordinary gRPC code |
| `todo/store.go` | the `Store` seam and its in-memory implementation |
| `jsoncodec/jsoncodec.go` | the `"json"` wire codec, so the page needs no protobuf runtime |
| `web/index.html` | the page, the import map, and the styling |
| `web/main.js` | the browser client: `connect()`, the descriptors, the calls, the `Watch` loop, the UI |
| `proto/todo.proto`, `todopb/` | the service and its checked-in bindings |

The JS surface of the wasm build is one global, published by `Gateway.Serve`
and named by neither side's source (`jsport.DefaultEntryPoint` on one,
`DefaultEntryPoint` on the other):

| | |
|---|---|
| `globalThis.drpcServe(port)` | hand over one end of a `MessageChannel`. Its *appearance* is the readiness signal — `open()` waits for the property, so there is no ready callback anywhere — and one port is one peer, so calling it again serves another client off the same handlers, which is exactly what a second `dial()` does |

Regenerate the bindings (needs [buf](https://buf.build); the generated files
are committed, so running the example does not):

```sh
buf generate examples/browser-wasm/proto --template examples/browser-wasm/buf.gen.yaml   # from the repo root
```

## Flags

| Flag | Default | |
|---|---|---|
| `-addr` | `127.0.0.1:8080` | HTTP address for the page and `/app.wasm` |
| `-rebuild` | `true` | rebuild `./wasm` on every `GET /app.wasm`. With `-rebuild=false` the prebuilt `web/app.wasm` is served instead — build it with `GOOS=js GOARCH=wasm go build -o web/app.wasm ./wasm` |

## Notes

- Everything is answered `Cache-Control: no-store`. A cached `app.wasm` or a
  cached `ts/dist` would quietly break the one claim the example makes.
- A server in the browser is a server with no trust boundary in front of it:
  the port is exactly as trustworthy as the code holding its other end, and the
  page holds both (`PROTOCOL.md` §15).
- Serving the port's `dist/` over HTTP is what an unpublished package costs.
  Once it is published the import map disappears and the page imports
  `@lesomnus/grpc-dgram` from a CDN or a bundle like any other dependency. The
  worker needs no entry of its own either way — `open()` loads it from a URL
  relative to its own module — except under a bundler, which rewrites workers
  by matching a literal `new Worker(new URL(…))` that `open()` deliberately is
  not: hand it the URL it produced, with `open(app, { workerUrl })`.
