// drpc over UDP datagrams on Node.js: one datagram carries one marshaled
// Envelop, the channel is unreliable (drpc's default mode), and nothing is
// ever fragmented — a message over the size limit is refused at send with
// MessageTooLargeError, which the core surfaces as RESOURCE_EXHAUSTED on the
// owning call (PROTOCOL.md §4.4). This is the TS twin of the Go
// `transport/udp` adapter, and interoperates with it on the wire.
//
// Entry: `@lesomnus/grpc-dgram/node-udp` (Node only — it imports `node:dgram`;
// the browser uses the WebRTC adapter instead).
//
// UDP is connectionless: there is no transport-death signal to hook the §4.5
// teardown on. Vanished peers are handled by the core's own liveness
// machinery; tearing the endpoint down is the application's move. An ICMP
// unreachable (surfaced as ECONNREFUSED) is treated as datagram loss, not an
// error — a momentarily absent peer (a restarting server) is exactly what the
// protocol rides out.

import { createSocket, type RemoteInfo, type Socket } from 'node:dgram'
import type { Conn } from './conn'
import { MessageTooLargeError } from './status'
import type { Server } from './server'
import type { ConnAttacher, FrameContext, FrameHandler, TransportInfo } from './transport'
import { unpack } from './transport'
import { decodeEnvelop, encodeEnvelop, type Frame } from './wire'

// DefaultMaxMessageSize keeps a datagram under the typical 1500-byte path MTU
// with room for IP/UDP headers and a tunnel or two.
export const DefaultMaxMessageSize = 1200

// transient reports errors that mean "this datagram went nowhere" (ICMP
// unreachable), not "this socket is broken": the socket stays usable and the
// condition is indistinguishable from the loss UDP already promises.
function transient(err: unknown): boolean {
  const code = (err as { code?: string } | undefined)?.code
  return code === 'ECONNREFUSED' || code === 'EHOSTUNREACH' || code === 'ENETUNREACH'
}

function sendDatagram(socket: Socket, data: Uint8Array, target?: { port: number; address: string }): Promise<void> {
  return new Promise((resolve, reject) => {
    const cb = (err: Error | null): void => {
      // A transient unreachable is loss, reported as success — failing the
      // call would defeat the retransmission that rides out a restarting peer.
      if (err && !transient(err)) reject(err)
      else resolve()
    }
    if (target !== undefined) socket.send(data, target.port, target.address, cb)
    else socket.send(data, cb)
  })
}

// UdpTransport is the client-side endpoint: a connected datagram socket
// talking to one server, so no peer key is needed (PROTOCOL.md §6.4). It is
// the tx handler for the Conn constructor — implementing TransportInfo and
// ConnAttacher directly. The Conn attaches it and the receive pump starts by
// itself; conn.close() (or close() here) tears everything down, socket
// included.
export class UdpTransport implements FrameHandler, TransportInfo, ConnAttacher {
  private readonly max: number
  private conn: Conn | undefined
  private closed = false

  constructor(
    private readonly socket: Socket,
    opts: { maxMessageSize?: number } = {},
  ) {
    this.max = opts.maxMessageSize ?? DefaultMaxMessageSize
  }

  // reliable reports false: UDP loses, duplicates, and reorders (§4.3).
  reliable(): boolean {
    return false
  }

  attachConn(conn: Conn): void {
    if (this.conn !== undefined) throw new Error('node-udp: transport already attached to a Conn')
    this.conn = conn
    this.socket.on('message', (data) => {
      let frames: Frame[]
      try {
        frames = decodeEnvelop(new Uint8Array(data.buffer, data.byteOffset, data.byteLength))
      } catch {
        return // malformed datagram: dropped, never a teardown (§4.2)
      }
      // Unreliable mode: conn.handle never blocks, so ordered awaited delivery
      // just drains the datagram's frames in order.
      void unpack(frames, conn, {})
    })
    this.socket.on('error', (err) => {
      // On a *connected* socket an ICMP unreachable (ECONNREFUSED etc.) is
      // delivered here, not to the send callback — and the socket stays
      // usable. That is datagram loss, not transport death: ride it out so a
      // momentarily absent peer (a restarting server) is survived, exactly as
      // Go's serve loop does (transport/udp/udp.go). Only a genuinely fatal
      // error tears the endpoint down (§4.5).
      if (transient(err)) return
      this.close(err)
    })
    this.socket.on('close', () => this.conn?.close())
  }

  // handle sends one frame as a single-frame envelop, refusing an oversize
  // envelop synchronously with MessageTooLargeError (PROTOCOL.md §4.4).
  handle(f: Frame): Promise<void> {
    const data = encodeEnvelop([f])
    if (this.max > 0 && data.length > this.max) {
      throw new MessageTooLargeError(`node-udp: ${data.length}-byte envelop over the ${this.max}-byte limit`)
    }
    return sendDatagram(this.socket, data)
  }

  // close closes the socket (stopping the pump) and fails any live calls.
  // Idempotent.
  close(err?: unknown): void {
    if (this.closed) return
    this.closed = true
    try {
      this.socket.close()
    } catch {
      // an already-closed socket throws; the conn teardown is what matters
    }
    this.conn?.close(err)
  }
}

// peerKey is the string form of a source address:port — the netip.AddrPort
// analog the Go gateway uses. Same remote → same key → same peer state.
function peerKey(rinfo: RemoteInfo): string {
  return `${rinfo.address}:${rinfo.port}`
}

// UdpGateway is the server-side endpoint: one unconnected UDP socket serving
// many peers, the source address:port as the peer key. It is the tx handler
// for the Server constructor.
export class UdpGateway implements FrameHandler {
  private readonly max: number
  private readonly peers = new Map<string, { port: number; address: string }>()
  private server: Server | undefined
  private closed = false

  constructor(
    private readonly socket: Socket,
    opts: { maxMessageSize?: number } = {},
  ) {
    this.max = opts.maxMessageSize ?? DefaultMaxMessageSize
  }

  // serve delivers received frames to server with the source address attached
  // as the peer key and the unreliable-mode annotation (PROTOCOL.md §4.3),
  // until close(). Returns a promise that resolves when the socket closes.
  serve(server: Server): Promise<void> {
    if (this.server !== undefined) throw new Error('node-udp: gateway already serving')
    this.server = server
    return new Promise<void>((resolve) => {
      this.socket.on('message', (data, rinfo) => {
        const key = peerKey(rinfo)
        this.peers.set(key, { port: rinfo.port, address: rinfo.address })
        let frames: Frame[]
        try {
          frames = decodeEnvelop(new Uint8Array(data.buffer, data.byteOffset, data.byteLength))
        } catch {
          return
        }
        void unpack(frames, server, { peer: key, reliable: false })
      })
      this.socket.on('close', resolve)
      this.socket.on('error', () => this.close())
    })
  }

  // handle sends one frame as a single-frame envelop to the peer named in ctx.
  handle(f: Frame, ctx: FrameContext = {}): Promise<void> {
    const key = ctx.peer
    if (typeof key !== 'string') {
      return Promise.reject(new Error(`node-udp: no gateway peer in context (got ${String(key)})`))
    }
    const target = this.peers.get(key)
    if (target === undefined) {
      return Promise.reject(new Error(`node-udp: peer ${key} is unknown`))
    }
    const data = encodeEnvelop([f])
    if (this.max > 0 && data.length > this.max) {
      throw new MessageTooLargeError(`node-udp: ${data.length}-byte envelop over the ${this.max}-byte limit`)
    }
    return sendDatagram(this.socket, data, target)
  }

  close(): void {
    if (this.closed) return
    this.closed = true
    try {
      this.socket.close()
    } catch {
      // already closed
    }
  }
}

// dialUdp opens a connected client socket to host:port and returns a
// UdpTransport for it — the convenience path (Go's net.Dial("udp", addr) +
// udp.New). The socket connects so it only receives from that server.
export function dialUdp(port: number, host = '127.0.0.1', opts?: { maxMessageSize?: number }): Promise<UdpTransport> {
  const socket = createSocket('udp4')
  return new Promise((resolve, reject) => {
    socket.once('error', reject)
    socket.connect(port, host, () => {
      socket.off('error', reject)
      resolve(new UdpTransport(socket, opts))
    })
  })
}

// listenUdp binds a server socket on host:port (0 = ephemeral) and returns a
// UdpGateway plus the bound port.
export function listenUdp(port = 0, host = '127.0.0.1', opts?: { maxMessageSize?: number }): Promise<{ gateway: UdpGateway; port: number }> {
  const socket = createSocket('udp4')
  return new Promise((resolve, reject) => {
    socket.once('error', reject)
    socket.bind(port, host, () => {
      socket.off('error', reject)
      resolve({ gateway: new UdpGateway(socket, opts), port: socket.address().port })
    })
  })
}
