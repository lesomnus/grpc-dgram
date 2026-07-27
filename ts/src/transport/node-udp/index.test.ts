// The Node UDP adapter's transport-death policy (PROTOCOL.md §4.4/§4.5),
// regression-pinning the audit finding that a connected socket's ICMP
// unreachable ('error' event) was tearing the endpoint down instead of being
// ridden out as datagram loss (the Go transport/udp behavior) — plus the
// one-line dialUdp path, over a real socket.

import { EventEmitter } from 'node:events'
import type { Socket } from 'node:dgram'
import { describe, expect, it, vi } from 'vitest'
import type { Conn } from '../../conn'
import { Code, type StatusError } from '../../status'
import { echo } from '../../testing'
import { dialUdp, listenUdp, UdpTransport } from './index'

// A minimal dgram.Socket stand-in: the transport only uses on/close/send.
class FakeSocket extends EventEmitter {
  closed = false
  close(): void {
    this.closed = true
    this.emit('close')
  }
  send(): void {
    /* unused here */
  }
}

function attach(socket: FakeSocket) {
  const conn = { close: vi.fn() }
  const transport = new UdpTransport(socket as unknown as Socket)
  transport.attachConn(conn as unknown as Conn)
  return { conn, transport }
}

const err = (code: string): NodeJS.ErrnoException => Object.assign(new Error(code), { code })

describe('UdpTransport transport-death policy (§4.4/§4.5)', () => {
  it('rides out ICMP-unreachable errors as datagram loss (does not tear down)', () => {
    for (const code of ['ECONNREFUSED', 'EHOSTUNREACH', 'ENETUNREACH']) {
      const socket = new FakeSocket()
      const { conn } = attach(socket)
      socket.emit('error', err(code))
      expect(conn.close).not.toHaveBeenCalled() // the call must survive to ride out a restart
      expect(socket.closed).toBe(false) // and the socket stays usable
    }
  })

  it('tears down on a genuinely fatal socket error', () => {
    const socket = new FakeSocket()
    const { conn } = attach(socket)
    const fatal = err('EBADF')
    socket.emit('error', fatal)
    expect(conn.close).toHaveBeenCalledWith(fatal)
    expect(socket.closed).toBe(true)
  })

  it("a socket 'close' fails live calls (UNAVAILABLE teardown)", () => {
    const socket = new FakeSocket()
    const { conn } = attach(socket)
    socket.close()
    expect(conn.close).toHaveBeenCalled()
  })
})

describe('dialUdp (the one-line client path)', () => {
  it('hands back a Conn, with one options bag split between the two halves', async () => {
    // A real socket, and a real bound port on the far side so nothing here
    // depends on ICMP behaviour. Nothing is served there: what is asserted is
    // where each option landed, and the oversize send is refused before a
    // datagram exists.
    const { gateway, port } = await listenUdp(0, '127.0.0.1')
    try {
      // reliable is the Conn's, overriding what the transport advertises
      // (§4.3); maxMessageSize is the adapter's ceiling (§4.4). One bag, two
      // consumers, no key in common.
      const conn = await dialUdp(port, '127.0.0.1', { maxMessageSize: 32, reliable: true })
      expect(conn.reliable).toBe(true)
      const err = (await conn.invoke(echo.once, { text: 'x'.repeat(200) }).catch((e) => e)) as StatusError
      expect(err.code).toBe(Code.RESOURCE_EXHAUSTED)
      conn.close() // the transport and its socket go with it
    } finally {
      gateway.close()
    }
  })
})
