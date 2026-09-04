# `@lesomnus/grpc-dgram/wasm`

**A Go `drpc.Server` compiled to `GOOS=js GOARCH=wasm`, started from the page
in two lines.**

```ts
import { open } from '@lesomnus/grpc-dgram/wasm'

const sock = await open('/app.wasm')
const conn = sock.dial()               // a drpc Conn, ready to call
```

and with Connect-ES, if that is your client:

```ts
const client = createClient(EchoService, createDrpcTransport(sock.dial()))
```

The Go half is already that short, and this is its counterpart — nothing else
in this package knows what wasm is:

```go
gw := jsport.NewGateway()
srv := drpc.NewServer(gw)
pb.RegisterEchoServiceServer(srv, impl)
log.Fatal(gw.Serve(context.Background(), srv)) // publishes the entry point
```

The package **ships the worker the instance runs in**, so there is no worker
script to write, no `MessageChannel` to make, no readiness to invent and no
teardown to remember. What is left for the application is what only it can
decide: when to dial another connection, and when to close.

The wire underneath is [`transport/port`](../transport/port) — one posted
message per marshaled `Envelop` (`PROTOCOL.md` §4.1), byte for byte the
WebSocket wire — and the running example is
[`examples/browser-wasm`](../../../examples/browser-wasm).

## `open(app, opts?)`

`open`, not `dial`, because at this point there is no peer: a `.wasm` URL is a
program, and nothing exists to reach until this has fetched it, instantiated it
and waited for it to say it can serve. `open` is the verb for bringing
something into existence; `dial` is the verb for reaching something that
exists, which is why what comes back is a **`Sock`** — and the connection is
the `sock.dial()` after it. (`Sock` and not `Server` for the mirror reason:
this side is the client, and what it names is the thing it dials into. For a
worker that is *already* running a server of its own, there is nothing to open:
that is [`dialWorker`](../transport/port).)

`app` is the compiled program in any form that survives a trip to a worker: a
URL (`string` or `URL`), the module's bytes, or a `WebAssembly.Module` compiled
once and instantiated many times. A `Response` is deliberately not one — it
cannot be structured-cloned, so it could never reach the realm that runs the
instance; pass its URL and the fetch happens there.

It resolves once the instance can serve, and rejects — having left nothing
behind — if the module cannot be fetched or instantiated, if the instance dies
on the way up, or if it never publishes its entry point. A page that merely
hangs blames the wrong half, so every one of those is an error with a sentence
in it. The one wait that is not bounded is a worker which answers *nothing*:
`readyTimeoutMs` is counted by the realm running the instance, so a worker that
never got that far cannot count it, and a worker of your own that does not run
the shipped module has no `error` event to fall back on either.

| Option | Default | Meaning |
|---|---|---|
| `worker` | `true` | `false` runs the instance in **this** realm — node, tests, a page that does not want a worker. A `Worker` you made yourself is accepted too, as long as it runs [the shipped worker](#the-worker), and then stays yours: only a worker `open()` made is one it terminates |
| `workerUrl` | resolved from this module's URL | where the shipped worker module lives. For bundlers, below |
| `wasmExec` | `'/wasm_exec.js'` | where to fetch the JS half of the Go runtime, in whichever realm runs the instance. Nothing is fetched where that realm already has `globalThis.Go` |
| `entryPoint` | `'drpcServe'` | the global the instance publishes its port-taking function as; must match the Go side's (`jsport.WithEntryPoint`) |
| `readyTimeoutMs` | `10_000` | how long to wait for that publish, measured from instantiation; `<= 0` waits forever |
| `go` | `new Go()` in the running realm | a `Go` instance you built — the way to pass argv or env. It belongs to the realm that made it, so it goes with `{ worker: false }`; `open()` refuses it otherwise rather than ignore it |
| `maxMessageSize`, `transfer` | see [`transport/port`](../transport/port#options) | passed to the transport under every `dial()` |

## `WasmSock`

```ts
interface WasmSock {
  readonly worker?: WasmWorker
  readonly exited: Promise<unknown>
  dial(opts?: DialOptions): Conn   // ConnOptions & { entryPoint?: string }
  close(): void
}
```

`dial()` opens one connection: a fresh channel, one end to the instance as its
own peer (§6.4), a `Conn` over the other. **Call it again for another,
independent connection** — its own epoch, sid space, flow-control windows and
per-peer caps, and a teardown that reaches only it. It is synchronous on
purpose: a transferred `MessagePort` queues everything posted into it until the
far side binds it, so a call opened on the tick `open()` resolved is delivered
late rather than dropped, and there is nothing to wait for. It throws once the
instance has exited or the sock is closed — a connection to a corpse would hang
forever rather than fail (§10.6 leaves no timer to end it), so it is refused
out loud.

What a second `dial()` does **not** give you, by itself, is a second server.
Every connection reaches the same handlers, the same application state and the
same thread: an instance is one Go program, and its goroutines interleave
cooperatively on whichever realm it runs in, so one connection's slow handler
delays the others (a Worker keeps that off the page's thread, not off its
own). The split is worth stating plainly, because it is the whole basis for
deciding how many to open:

| per connection | shared by all of them |
|---|---|
| epoch, sid space, seq, flow-control windows, rx buffers | the service registry and every handler behind it |
| per-peer resource caps (`MaxLiveCalls`, tombstones — §15) | whatever state those handlers hold |
| teardown: closing one leaves the others running | the instance's single thread, and its death |

So the reason to dial twice is *isolation of the connection*, not of the
server: a part of the page you want to tear down on its own, or a `Worker` or
iframe that should hold its own peer rather than share yours. A second
*server* is the next section.

## Two servers in one instance

A Go program may run more than one `drpc.Server` — a second registry, a second
interceptor chain, the shape of an admin listener sitting beside a control one
— and publish it under a name of its own:

```go
control := jsport.NewGateway()                                  // drpcServe
admin := jsport.NewGateway(jsport.WithEntryPoint("drpcAdmin"))

go func() { log.Fatal(control.Serve(ctx, controlSrv)) }()
log.Fatal(admin.Serve(ctx, adminSrv))
```

```ts
const sock = await open('/app.wasm')
const control = sock.dial()
const admin = sock.dial({ entryPoint: 'drpcAdmin' })
```

One module, one download, one compile, one runtime, one memory, one lifetime —
and two servers with nothing in common but the program they live in. The
alternative is two `open()` calls, which is two of all of that and two
deployments that have to agree.

**Only the first entry point is readiness.** `open()` resolves when the name it
started on publishes, and a program that does anything asynchronous between its
two gateways — a fetch, a database opening, the `time.Sleep` in
[`conformance/wasmserver`](../../../conformance/wasmserver) — hands control
back to the page before the second `Serve` has run. Whether that happens is
decided by Go's scheduler, and it flips when you reorder two lines of `main`.

So a dial to a name that is not there **yet** waits for it rather than failing.
It stays synchronous: the transferred port queues the calls opened on it while
the far side waits, exactly as it queues the ones opened before the instance
was ready. A name that never arrives ends that one connection with a cause
after `readyTimeoutMs` — the same clock the start used — while the instance and
every other connection to it go on working. Nothing about the ordering of your
`Serve` calls is load-bearing.

A word of warning about the Go side: `Serve` refuses a name that is already
published rather than steal it, and returns that error *without* publishing. A
`go gw.Serve(...)` that drops the error turns a name collision into a page that
waits out its timeout blaming the name. Report it.

`exited` resolves — never rejects — with the cause when the instance is gone.
`close()` resolves it too: after that this sock has stopped watching, and a
promise that could only hang would be worse than one that answers.

`close()` ends every `Conn` dialled here (their live calls fail with a cause
rather than hang) and *then* terminates the worker, if `open()` made one. That
order matters: a worker terminated out from under a live call leaves it
hanging, since `terminate()` also discards the goodbye that would have ended
it. It is idempotent, and with `{ worker: false }` it cannot stop the Go
program itself — `wasm_exec` has no kill switch, and `os.Exit` is the
instance's own decision.

## What `open()` does, and why each part is there

Four hazards, each of which fails in a way that points somewhere else:

1. **Readiness.** The entry point exists only once the instance's `main` has
   published it, and `go.run()`'s promise settles at *exit*, not at startup —
   there is nothing to await but the property appearing. So an accessor goes on
   `globalThis[entryPoint]` **before** `go.run`, and the publish cannot be
   missed: `js.Global().Set` reaches JS as `Reflect.set`, which triggers an
   accessor. Publishing **is** the readiness signal; no second magic name
   exists on either side, and the property is handed back afterwards, so a
   second `open()` finds `globalThis` as it was.
2. **Death on the way up.** A Go panic *resolves* `go.run()` (wasm_exec exits
   with code 2), so a settled run before readiness is a failure, never a
   success. Readiness is raced against it, and against a wall clock for the
   module that neither publishes nor exits.
3. **A broken build.** A dev server answers one with a 500 whose body is the
   compiler output, and `instantiateStreaming` discards it: what reaches the
   console is *the MIME type was not application/wasm*, the one message that
   does not say what stopped compiling. The response is checked first, and its
   body is the error.
4. **Teardown (§4.5).** Below — it is the whole reason this is a helper.

## Teardown is the whole reason

A message port has no death to detect. Both endpoints live in one process:
there is no socket to break, nothing for a keepalive to measure, and a
`MessagePort` whose peer stopped existing looks exactly like one whose peer is
merely quiet. With every protocol timer off (§10.6), the adapter's §4.5
teardown is the **only** thing that ever unblocks a live call — so death here
is said out loud, and saying it is what `open()` is for:

- **In a worker**, `go.run()`'s promise is visible only from inside it, so the
  worker posts the goodbye (an empty message, §4.1) on every port it holds, and
  then tells the page. `open()` fails every `Conn` dialled through the sock
  with the cause. Nothing else is in a position to do either.
- **With `{ worker: false }`**, `go.run()`'s promise is right here and is wired
  to the same thing.
- Either way the corpse is defused before the goodbye reaches it: an exited
  instance leaves its `js.Func`s registered on its ports, and `wasm_exec`
  re-enters the dead runtime for every event that still arrives, throwing *Go
  program has already exited* out of an event handler — which a page logs and
  node dies of.

## The worker

**Why one at all:** Go's scheduler shares whatever thread it runs on, so a
handler that computes for 50 ms freezes the page for 50 ms if that thread is
the main one. Off it, the UI stays live and the only thing crossing the
boundary is bytes.

The worker is a module worker loaded from a URL relative to this entry's own
(`dist/wasm.mjs` beside `dist/wasm/worker.mjs`), which is right for a plain
`<script type="module">` and for an import map: nothing to copy, and no entry
of its own to map. `./protocol.ts` is what the two halves say to each other,
and every message in it is a plain tagged object, never a `Uint8Array` — the
drpc wire only ever crosses the *transferred ports*, so it can never be
confused with a frame, and a worker of the application's own can share the
channel.

Two ordering rules make it correct, and both are silent when broken:

1. **A `dial()` may precede the instance.** What is transferred is a
   `MessagePort`, and a `MessagePort` queues everything posted into it until
   its owner starts it — which the Go adapter does when it binds. Handing the
   `Worker` *object* to a `PortTransport` would not be safe: a worker's global
   scope is wired through `onmessage` and drops what arrives before Go
   registers its handler, which is the rule `jsport.Bind`'s doc states.
2. **The worker registers its `message` listener synchronously**, at module
   evaluation, before any `await`. A module worker's top-level await yields to
   the event loop, and a `start` or `serve` dispatched during that yield would
   be lost with nothing to report it.

**Under a bundler.** A bundler rewrites workers by pattern-matching a literal
`new Worker(new URL('./x', import.meta.url))`, which this is deliberately not
(the constructor is looked up, so a realm without one is told so instead of
throwing *Worker is not defined*). Hand it the URL it produced:

```ts
import workerUrl from '@lesomnus/grpc-dgram/wasm/worker?worker&url'  // Vite

const sock = await open('/app.wasm', { workerUrl })
```

**Bring your own worker.** `open(app, { worker })` uses one you already have —
it must run this module (`import '@lesomnus/grpc-dgram/wasm/worker'`, or
`serveIn(self)`), since that half is what starts the instance and says the
goodbye — and never terminates it, because it may be hosting something else of
yours. One worker is one instance, so a second `open()` on the same worker is
refused with a message saying so: a second *server* in one instance is a second
entry point (above), not a second start. Sharing it is safe in both directions: every message either half posts
carries a `drpc` tag and anything untagged is dropped in silence, and an
uncaught `error` in the worker is taken as a failure to start only *before*
`ready` — after that a worker survives its own exceptions, and somebody else's
bug does not tear these connections down. If what you want is a connection to a
worker that is *not* a wasm instance at all — one already running, with a
server of its own — that is [`dialWorker`](../transport/port) in the port
adapter: nothing to open, so it is a `dial`, and it hands back the same `Conn`
this one does over the same channel — one end transferred, a `PortTransport`
over the other — with no wasm anywhere.

## `wasm_exec.js` is not vendored

It is the JS half of the Go runtime and is version-coupled to the compiler that
built your module, so a copy here would pin the wrong one. Serve your
toolchain's:

```sh
cp "$(go env GOROOT)/lib/wasm/wasm_exec.js" ./public/
```

The realm that runs the instance fetches it from `wasmExec` (`/wasm_exec.js` by
default) and evaluates it — it is a classic script, not a module, which in a
module worker is the only way in. A realm that already has `globalThis.Go` (a
page that loaded it as a `<script>`, a test that evaluated it) fetches nothing.
When it is missing, the error names the command above; nothing else in the
stack knows the file's name, let alone where it comes from.

## Caveats

- **The instance's lifetime is not owned here.** Nothing can stop a Go program
  once `go.run` has started it — `wasm_exec` has no kill switch and `os.Exit`
  is the instance's own decision — so all a host can do is end the *realm*. A
  worker `open()` made is therefore terminated when a start fails after
  `go.run`, which is the only thing that ends such an instance; with
  `{ worker: false }`, or a worker of yours, `open()` rejects with it still
  running, and a page reload or the module's own shutdown entry point are what
  are left. After a successful start it is `close()` that ends the realm, and a
  dead instance does **not** end it by itself: `exited` is what to close on.
- **One worker is one instance.** A second `start` has no entry point left to
  publish under and no second lifetime to report — that is what a second
  `open()` is. Two overlapping starts in one realm are refused for the same
  reason, unless each is given its own `entryPoint`.
- **A wasm server has no trust boundary in front of it.** The port is exactly
  as trustworthy as the code holding its other end (`PROTOCOL.md` §15), and
  with `{ worker: false }` the page holds both.
- Everything the transport itself does — the goodbye, the ignored non-envelop
  message, backpressure, the `Worker` it never terminates — is
  [`transport/port`](../transport/port), unchanged.
