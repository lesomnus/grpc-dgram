// @ts-check
// The browser half of the demo: a todo UI driven by a real Go gRPC service,
// which by default runs in a Worker this page starts as a wasm instance. Plain
// ES modules — no build step, no bundler, no framework. The import map in
// index.html points the package entries at the port's build output (ts/dist).
//
// Everything past `connect()` — the descriptors, the calls, the Watch loop,
// the error rendering — is written once: the same code drives the wasm server
// and the server process behind ?server=ws. Only the link that switches
// between the two, and the status line that names them, know which is which.

import { Code, Conn, serverStreamingMethod, StatusError, unaryMethod } from '@lesomnus/grpc-dgram'
import { dialWebSocket } from '@lesomnus/grpc-dgram/transport/websocket'
import { open } from '@lesomnus/grpc-dgram/wasm'

// The messages, as protojson writes them (see jsoncodec/). Every field is
// optional because protojson omits zero values: a task that is not done
// carries no `done`, and the first task carries no `id`… if its id were 0.
/** @typedef {{ id?: number, title?: string, done?: boolean }} Task */
/** @typedef {{ tasks?: Task[], servedBy?: string }} ListResponse */
/** @typedef {{ kind?: string, task?: Task }} Event */

const enc = new TextEncoder()
const dec = new TextDecoder()

// The page's only payload codec: JSON in, JSON out, for every message type.
// It is typed loosely on purpose — JSON.parse cannot check a shape at
// runtime — and the typedefs above are what the rest of the file is checked
// against.
/** @type {import('@lesomnus/grpc-dgram').PayloadCodec<any>} */
const json = {
  marshal: (v) => enc.encode(JSON.stringify(v)),
  unmarshal: (b) => JSON.parse(dec.decode(b)),
}

// The wire codec, named on the OPEN frame (PROTOCOL.md §12). The Go server
// resolves the name "json" against its codec registry and marshals with
// protojson, so its handlers keep their generated protobuf stubs. Set once as
// the connection's call default rather than repeated per call; drop it and the
// default protobuf codec applies — that is the bundler path, with
// @lesomnus/grpc-dgram/transport/protobuf-es and protoc-gen-es output.
const wireCodec = { name: 'json', request: json, response: json }

// The method descriptors. Each path is the full gRPC method name — the same
// one the Go service registers from todo.proto (PROTOCOL.md §13).
/** @type {import('@lesomnus/grpc-dgram').UnaryDesc<Record<string, never>, ListResponse>} */
const List = unaryMethod('/todo.TodoService/List', { request: json, response: json })
/** @type {import('@lesomnus/grpc-dgram').UnaryDesc<{ title: string }, Task>} */
const Add = unaryMethod('/todo.TodoService/Add', { request: json, response: json })
/** @type {import('@lesomnus/grpc-dgram').UnaryDesc<{ id: number }, Task>} */
const Toggle = unaryMethod('/todo.TodoService/Toggle', { request: json, response: json })
/** @type {import('@lesomnus/grpc-dgram').UnaryDesc<{ id: number }, { id?: number }>} */
const Remove = unaryMethod('/todo.TodoService/Remove', { request: json, response: json })
/** @type {import('@lesomnus/grpc-dgram').ServerStreamingDesc<Record<string, never>, Event>} */
const Watch = serverStreamingMethod('/todo.TodoService/Watch', { request: json, response: json })

/** @type {import('@lesomnus/grpc-dgram').ConnOptions} */
const connOptions = { defaultCallOptions: { codec: wireCodec } }

/** @typedef {{ conn: Conn, label: string }} Session */

// ---------------------------------------------------------------------------
// the two servers
// ---------------------------------------------------------------------------

/**
 * Starts the Go server in a Worker and returns a client connected to it. The
 * module is fetched fresh on every load, so this *is* a server restart — new
 * instance, new store, no leftovers.
 *
 * These two lines are the whole page-side wiring. `open()` spawns the worker
 * the package ships, loads `/wasm_exec.js` in it, and waits for
 * `wasm/main.go`'s `Gateway.Serve` to publish its entry point — publishing it
 * IS the readiness signal, so there is no ready callback anywhere. It also
 * wires the death: an instance that exits or panics fails every call in flight
 * with a cause instead of hanging this UI, and with every protocol timer off
 * (§10.6) that §4.5 teardown is the only thing that ever unblocks a call here.
 *
 * `dial()` is synchronous because the `MessagePort` it transfers queues
 * everything posted into it until the Go side binds it — the `List` below
 * cannot be too early — and calling it again would open a second, independent
 * peer (§6.4). Nothing holds the sock: `sock.close()` would end the
 * connections and terminate the worker, and a page that is being reloaded is
 * doing exactly that anyway.
 * @returns {Promise<Session>}
 */
async function connectWasm() {
  const sock = await open('/app.wasm')
  return { conn: sock.dial(connOptions), label: 'in a Worker (wasm)' }
}

/**
 * Connects to the server process at /rpc instead. Same handlers, same wire,
 * a WebSocket in place of the message port.
 * @returns {Promise<Session>}
 */
async function connectWebSocket() {
  const url = new URL('/rpc', location.href)
  url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:'
  return { conn: new Conn(dialWebSocket(url.href), connOptions), label: `WebSocket to ${url.href}` }
}

/**
 * Returns a client on whichever server the URL selects. Everything after this
 * point is written once and runs against either.
 * @returns {Promise<Session>}
 */
function connect() {
  return new URLSearchParams(location.search).get('server') === 'ws' ? connectWebSocket() : connectWasm()
}

// ---------------------------------------------------------------------------
// page wiring
// ---------------------------------------------------------------------------

const state = /** @type {HTMLElement} */ (document.getElementById('state'))
const form = /** @type {HTMLFormElement} */ (document.getElementById('form'))
const titleInput = /** @type {HTMLInputElement} */ (document.getElementById('title'))
const addButton = /** @type {HTMLButtonElement} */ (document.getElementById('add'))
const missingButton = /** @type {HTMLButtonElement} */ (document.getElementById('missing'))
const taskList = /** @type {HTMLElement} */ (document.getElementById('tasks'))
const errorLine = /** @type {HTMLElement} */ (document.getElementById('error'))
const switchLink = /** @type {HTMLAnchorElement} */ (document.getElementById('switch'))
const log = /** @type {HTMLElement} */ (document.getElementById('log'))

const onWebSocket = new URLSearchParams(location.search).get('server') === 'ws'
switchLink.href = onWebSocket ? '?' : '?server=ws'
switchLink.textContent = onWebSocket ? 'Back to the wasm server' : 'Talk to the server process instead'
state.textContent = onWebSocket ? 'connecting to the server process…' : 'starting the server in a Worker…'

/**
 * @param {string} line
 * @param {'ok' | 'err' | ''} [kind]
 */
function write(line, kind = '') {
  const el = document.createElement('div')
  if (kind !== '') el.className = kind
  el.textContent = line
  log.append(el)
  log.scrollTop = log.scrollHeight
}

/**
 * Renders a failed call: a handler's gRPC status, or anything else verbatim.
 * @param {unknown} err
 */
function fail(err) {
  errorLine.textContent = err instanceof StatusError ? `${Code[err.code]}: ${err.desc}` : String(err)
}

/** @type {Task[]} */
let tasks = []

function render() {
  taskList.replaceChildren(
    ...tasks.map((task) => {
      const li = document.createElement('li')
      if (task.done === true) li.className = 'done'

      const box = document.createElement('input')
      box.type = 'checkbox'
      box.checked = task.done === true
      box.addEventListener('change', () => void call(Toggle, { id: task.id ?? 0 }))

      const label = document.createElement('span')
      label.textContent = `#${task.id ?? 0}  ${task.title ?? ''}`

      const remove = document.createElement('button')
      remove.type = 'button'
      remove.textContent = 'remove'
      remove.addEventListener('click', () => void call(Remove, { id: task.id ?? 0 }))

      li.append(box, label, remove)
      return li
    }),
  )
}

/**
 * Invokes one unary method and reports its status. The list is not touched
 * here: every mutation comes back on the Watch stream, so the UI has one
 * source of truth whichever client caused the change.
 * @template Req, Res
 * @param {import('@lesomnus/grpc-dgram').UnaryDesc<Req, Res>} desc
 * @param {Req} req
 * @returns {Promise<Res | undefined>}
 */
async function call(desc, req) {
  errorLine.textContent = ''
  write(`→ ${desc.path.split('/').pop()}(${JSON.stringify(req)})`)
  try {
    return await conn.invoke(desc, req, { timeoutMs: 5000 })
  } catch (err) {
    fail(err)
    write(`✗ ${errorLine.textContent}`, 'err')
    // Redraw from the last state the server confirmed: a refused Toggle must
    // not leave its checkbox showing a change that never happened.
    render()
    return undefined
  }
}

/**
 * Subscribes to the server's mutation stream and applies every event. It runs
 * until the call ends — which, with no protocol timers running, happens only
 * because the server said so or because the transport's teardown failed it.
 */
async function watch() {
  const stream = conn.newStream(Watch)
  await stream.send({})
  for await (const ev of stream) {
    const task = ev.task ?? {}
    switch (ev.kind) {
      case 'KIND_ADDED':
        tasks = [...tasks, task]
        break
      case 'KIND_CHANGED':
        tasks = tasks.map((t) => (t.id === task.id ? task : t))
        break
      case 'KIND_REMOVED':
        tasks = tasks.filter((t) => t.id !== task.id)
        break
    }
    write(`⟵ ${ev.kind ?? 'KIND_UNSPECIFIED'} #${task.id ?? 0} ${JSON.stringify(task.title ?? '')}`, 'ok')
    render()
  }
  // Reached only if a server ends the stream with OK. This one never does —
  // every path out of its handler carries a status (todo/service.go) — but a
  // subscription that stopped has to say so rather than sit there looking idle.
  write('the Watch stream ended')
}

let session
try {
  session = await connect()
} catch (err) {
  state.textContent = 'no server'
  fail(err)
  write(`✗ ${String(err)}`, 'err')
  throw err
}
const { conn } = session

// The first call is also the connection check: an unreachable WebSocket, or a
// server that never came up, fails here rather than looking idle.
const listed = await call(List, {})
tasks = listed?.tasks ?? []
render()
state.textContent = listed === undefined ? `no answer from ${session.label}` : `served by ${listed.servedBy ?? '?'} — ${session.label}, reliable mode`
addButton.disabled = false
missingButton.disabled = false

// The stream is started after the first List so the initial snapshot and the
// events that follow it cannot interleave.
watch().catch((err) => {
  fail(err)
  // In reliable mode nothing times a call out, so this stream ends only
  // because the server ended it: the adapter's §4.5 teardown — the goodbye
  // from a server that stopped, or the cause open() reports when the wasm
  // instance dies — or a status from the handler itself, RESOURCE_EXHAUSTED
  // when the store dropped this watcher for falling behind (todo/store.go).
  // Either way the list has stopped tracking the store, so it is stale from
  // here on and the buttons that would mutate it go away with it.
  state.textContent = `the event stream ended (${errorLine.textContent}) — reload to start over`
  addButton.disabled = true
  missingButton.disabled = true
  write(`✗ event stream: ${errorLine.textContent}`, 'err')
})

form.addEventListener('submit', (ev) => {
  ev.preventDefault()
  // Submitting an empty input is the INVALID_ARGUMENT demo, so the value is
  // sent as typed: validation belongs to the handler, not to the page.
  const title = titleInput.value
  titleInput.value = ''
  void call(Add, { title })
})

missingButton.addEventListener('click', () => void call(Toggle, { id: 999 }))

// One close says goodbye to the server, fails anything still in flight, and
// releases the channel — the same call whichever transport is underneath.
globalThis.addEventListener('beforeunload', () => conn.close())
