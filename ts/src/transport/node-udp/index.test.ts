// The Node UDP adapter's transport-death policy (PROTOCOL.md §4.4/§4.5),
// regression-pinning the audit finding that a connected socket's ICMP
// unreachable ('error' event) was tearing the endpoint down instead of being
// ridden out as datagram loss (the Go transport/udp behavior).

import { EventEmitter } from 'node:events'
import type { Socket } from 'node:dgram'
import { describe, expect, it, vi } from 'vitest'
import type { Conn } from '../../conn'
import { UdpTransport } from './index'

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
