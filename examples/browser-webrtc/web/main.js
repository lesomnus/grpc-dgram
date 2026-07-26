// @ts-check
// The browser half of the demo: one unary RPC over a WebRTC DataChannel,
// spoken with the TypeScript port of grpc-dgram. Plain ES modules — no build
// step, no bundler, no framework. The import map in index.html points the two
// package entries at the port's build output (ts/dist).

import { Code, Conn, StatusError, unaryMethod } from '@lesomnus/grpc-dgram'
import { DataChannelTransport } from '@lesomnus/grpc-dgram/transport/webrtc'

/** @typedef {{ message: string }} EchoRequest */
/** @typedef {{ message: string, servedBy?: string }} EchoResponse */

const enc = new TextEncoder()
const dec = new TextDecoder()

// The core is codec-agnostic: a method's payloads go through whatever pair of
// marshallers you hand it. This page has no protobuf runtime, so it uses JSON
// and tells the server so (see `json` below).
/** @type {import('@lesomnus/grpc-dgram').PayloadCodec<EchoRequest>} */
const requestCodec = {
  marshal: (v) => enc.encode(JSON.stringify(v)),
  unmarshal: (b) => JSON.parse(dec.decode(b)),
}
/** @type {import('@lesomnus/grpc-dgram').PayloadCodec<EchoResponse>} */
const responseCodec = {
  marshal: (v) => enc.encode(JSON.stringify(v)),
  unmarshal: (b) => JSON.parse(dec.decode(b)),
}

// The method descriptor. The path is the full gRPC method name — the same one
// the Go service registers from its .proto (PROTOCOL.md §13).
const Echo = unaryMethod('/webecho.EchoService/Echo', {
  request: requestCodec,
  response: responseCodec,
})

// The wire codec, named on the OPEN frame (PROTOCOL.md §12). The Go server
// resolves the name "json" against its codec registry and marshals with
// protojson, so its handlers keep their generated protobuf stubs. Drop this
// and the default protobuf codec applies — that is the bundler path, with
// @lesomnus/grpc-dgram/transport/protobuf-es and protoc-gen-es output.
const json = { name: 'json', request: requestCodec, response: responseCodec }

/**
 * Negotiates one peer connection with the Go server and returns a drpc client
 * on its data channel.
 * @returns {Promise<{ pc: RTCPeerConnection, conn: Conn }>}
 */
async function connect() {
  const pc = new RTCPeerConnection()

  // Default channel configuration: ordered, no retransmit or lifetime cap.
  // The adapter derives *reliable* from exactly that, so the core runs with
  // every protocol timer off and delivers the exact sequence (§10.6). Pass
  // `{ ordered: false, maxRetransmits: 0 }` instead and the same code runs in
  // unreliable mode, which is what a sensor feed wants.
  const dc = pc.createDataChannel('rpc')

  // The transport attaches itself to the Conn: no pump to manage, and
  // conn.close() later tears down the channel too. The cast is a type-level
  // formality: the adapter's structural DataChannelLike asks for
  // `send(data: Uint8Array)`, while the DOM types RTCDataChannel.send over
  // ArrayBufferView<ArrayBuffer>. The runtime shapes are the same.
  const channel = /** @type {import('@lesomnus/grpc-dgram/transport/webrtc').DataChannelLike} */ (dc)
  const conn = new Conn(new DataChannelTransport(channel))

  await pc.setLocalDescription(await pc.createOffer())
  await gathered(pc) // one-shot signaling: no trickle ICE

  const res = await fetch('/offer', {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify(pc.localDescription),
  })
  if (!res.ok) throw new Error(`signaling failed: ${res.status} ${await res.text()}`)
  await pc.setRemoteDescription(await res.json())

  return { pc, conn }
}

/**
 * Resolves once ICE gathering is complete, so the offer carries every
 * candidate and the exchange needs a single request.
 * @param {RTCPeerConnection} pc
 * @returns {Promise<void>}
 */
function gathered(pc) {
  if (pc.iceGatheringState === 'complete') return Promise.resolve()
  return new Promise((resolve) => {
    pc.addEventListener('icegatheringstatechange', () => {
      if (pc.iceGatheringState === 'complete') resolve()
    })
  })
}

// ---------------------------------------------------------------------------
// page wiring
// ---------------------------------------------------------------------------

const state = /** @type {HTMLElement} */ (document.getElementById('state'))
const form = /** @type {HTMLFormElement} */ (document.getElementById('form'))
const input = /** @type {HTMLInputElement} */ (document.getElementById('input'))
const send = /** @type {HTMLButtonElement} */ (document.getElementById('send'))
const log = /** @type {HTMLElement} */ (document.getElementById('log'))

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

let session
try {
  session = await connect()
} catch (err) {
  state.textContent = 'not connected'
  write(`✗ ${String(err)}`, 'err')
  throw err
}
const { pc, conn } = session
state.textContent = `connected — ${pc.connectionState}, reliable data channel`
send.disabled = false
write('data channel open; the RPCs below never touch HTTP')

pc.addEventListener('connectionstatechange', () => {
  state.textContent = `peer connection: ${pc.connectionState}`
  if (pc.connectionState === 'failed' || pc.connectionState === 'closed') {
    send.disabled = true
    // Teardown duty (§4.5): with timers off, closing the Conn is what fails
    // any live call instead of leaving it hanging.
    conn.close(new Error(`peer connection ${pc.connectionState}`))
  }
})

form.addEventListener('submit', async (ev) => {
  ev.preventDefault()
  const message = input.value
  write(`→ Echo(${JSON.stringify(message)})`)
  send.disabled = true
  try {
    const res = await conn.invoke(Echo, { message }, { codec: json, timeoutMs: 5000 })
    write(`← ${JSON.stringify(res.message)}  (served by ${res.servedBy ?? '?'})`, 'ok')
  } catch (err) {
    // Handler failures arrive as gRPC statuses: try sending an empty message.
    if (err instanceof StatusError) write(`✗ ${Code[err.code]}: ${err.desc}`, 'err')
    else write(`✗ ${String(err)}`, 'err')
  } finally {
    send.disabled = pc.connectionState === 'failed' || pc.connectionState === 'closed'
  }
})

// One close tears down the Conn, the transport, and the channel.
globalThis.addEventListener('beforeunload', () => {
  conn.close()
  pc.close()
})
