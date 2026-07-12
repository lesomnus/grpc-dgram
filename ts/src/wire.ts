// The drpc wire format (PROTOCOL.md §5): a hand-rolled protobuf codec for
// exactly three messages — Frame, Envelop, Metadata — so the core carries no
// protobuf runtime dependency. The encoding must stay byte-identical to the
// Go implementation for the §5 golden vectors (wire.test.ts): fields are
// written in field-number order, implicit-presence fields are omitted at
// their default, and `payload`/`code` carry explicit presence (undefined =
// absent on the wire).

import type { Metadata } from './metadata'
import { Code, StatusError, toStatusError } from './status'

// Frame flags (PROTOCOL.md §7).
export const FlagOpen = 1
export const FlagClose = 2
export const FlagReset = 4
export const FlagPing = 8

export interface Frame {
  // Sender's incarnation nonce; RESET echoes the offending frame's instead
  // (PROTOCOL.md §6.1, §9.3). fixed32; 0 = unset.
  epoch: number
  // Stream id (§6.2). 0 = peer-scope control (PING).
  sid: number
  // Per-stream, per-direction sequence (§6.3). Stateless frames carry 0.
  seq: number
  // Flag bitmask (§7).
  flags: number
  // Full method name; OPEN frames only (§13).
  method: string
  // Codec name; OPEN frames only; '' = proto (§12).
  codec: string
  // Remaining call budget in milliseconds; OPEN only (§10.2).
  // google.protobuf.Duration on the wire; undefined = absent.
  timeoutMs?: number
  // One marshaled message. Presence is meaningful (§7): a flag-less frame
  // WITH payload is a data frame even at 0 bytes; WITHOUT it, a header frame.
  payload?: Uint8Array
  // gRPC status code; CLOSE only, where presence distinguishes terminal
  // CLOSE (set) from half-close (unset).
  code?: number
  // Status description.
  desc: string
  header?: Metadata
  trailer?: Metadata
  // The client incarnation this frame addresses (§6.1); 0 = absent.
  peerEpoch: number
}

// frame builds a Frame with every implicit-presence field at its default.
export function frame(init?: Partial<Frame>): Frame {
  return {
    epoch: 0,
    sid: 0,
    seq: 0,
    flags: 0,
    method: '',
    codec: '',
    desc: '',
    peerEpoch: 0,
    ...init,
  }
}

export const isOpen = (f: Frame): boolean => (f.flags & FlagOpen) !== 0
export const isClose = (f: Frame): boolean => (f.flags & FlagClose) !== 0
export const isReset = (f: Frame): boolean => (f.flags & FlagReset) !== 0
export const isPing = (f: Frame): boolean => (f.flags & FlagPing) !== 0

// Terminal CLOSE: a call result from the server, or an abort from the client.
export const isTerminal = (f: Frame): boolean => isClose(f) && f.code !== undefined
// Client half-close: send direction done, call continues.
export const isHalfClose = (f: Frame): boolean => isClose(f) && f.code === undefined
// Data frame: no flags, payload present (even 0 bytes, §7).
export const isData = (f: Frame): boolean => f.flags === 0 && f.payload !== undefined
// Header frame H: no flags, no payload.
export const isHeaderFrame = (f: Frame): boolean => f.flags === 0 && f.payload === undefined

// frameStatus reads the terminal status a CLOSE frame carries.
export function frameStatus(f: Frame): StatusError {
  return new StatusError((f.code ?? 0) as Code, f.desc)
}

// setFrameError writes err as the frame's terminal status.
export function setFrameError(f: Frame, err: unknown): void {
  const st = toStatusError(err)
  f.code = st.code
  f.desc = st.desc
}

// resetFor builds a RESET answering f: the epoch echoes the offending frame —
// the one exception to the sender-epoch rule — and the peer_epoch is echoed
// too, so a client→server RESET resets exactly that incarnation's call
// (PROTOCOL.md §9.3).
export function resetFor(f: Frame): Frame {
  return frame({ flags: FlagReset, epoch: f.epoch, sid: f.sid, peerEpoch: f.peerEpoch })
}

// ---------------------------------------------------------------------------
// protobuf wire primitives
// ---------------------------------------------------------------------------

const textEncoder = new TextEncoder()
const textDecoder = new TextDecoder()

class Writer {
  private buf: number[] = []

  byte(b: number): void {
    this.buf.push(b & 0xff)
  }

  varint(v: number): void {
    // Values are uint32-ranged here (flags, code, lengths).
    let n = v >>> 0
    while (n > 0x7f) {
      this.buf.push((n & 0x7f) | 0x80)
      n >>>= 7
    }
    this.buf.push(n)
  }

  varint64(v: bigint): void {
    let n = BigInt.asUintN(64, v)
    while (n > 0x7fn) {
      this.buf.push(Number(n & 0x7fn) | 0x80)
      n >>= 7n
    }
    this.buf.push(Number(n))
  }

  tag(field: number, wire: number): void {
    this.varint((field << 3) | wire)
  }

  fixed32(field: number, v: number): void {
    this.tag(field, 5)
    const n = v >>> 0
    this.buf.push(n & 0xff, (n >>> 8) & 0xff, (n >>> 16) & 0xff, (n >>> 24) & 0xff)
  }

  bytes(field: number, v: Uint8Array): void {
    this.tag(field, 2)
    this.varint(v.length)
    for (let i = 0; i < v.length; i++) this.buf.push(v[i]!)
  }

  string(field: number, v: string): void {
    this.bytes(field, textEncoder.encode(v))
  }

  finish(): Uint8Array {
    return Uint8Array.from(this.buf)
  }
}

class Reader {
  private pos = 0

  constructor(private readonly buf: Uint8Array) {}

  get eof(): boolean {
    return this.pos >= this.buf.length
  }

  private next(): number {
    if (this.pos >= this.buf.length) throw new Error('drpc: malformed frame: truncated')
    return this.buf[this.pos++]!
  }

  varint(): bigint {
    let out = 0n
    for (let shift = 0n; shift < 70n; shift += 7n) {
      const b = this.next()
      out |= BigInt(b & 0x7f) << shift
      if ((b & 0x80) === 0) return BigInt.asUintN(64, out)
    }
    throw new Error('drpc: malformed frame: varint too long')
  }

  varint32(): number {
    return Number(this.varint() & 0xffffffffn)
  }

  fixed32(): number {
    if (this.pos + 4 > this.buf.length) throw new Error('drpc: malformed frame: truncated')
    const b = this.buf
    const p = this.pos
    this.pos += 4
    return (b[p]! | (b[p + 1]! << 8) | (b[p + 2]! << 16) | (b[p + 3]! << 24)) >>> 0
  }

  bytes(): Uint8Array {
    const n = this.varint32()
    if (this.pos + n > this.buf.length) throw new Error('drpc: malformed frame: truncated')
    const out = this.buf.subarray(this.pos, this.pos + n)
    this.pos += n
    return out
  }

  string(): string {
    return textDecoder.decode(this.bytes())
  }

  skip(wire: number): void {
    switch (wire) {
      case 0:
        this.varint()
        return
      case 1:
        if (this.pos + 8 > this.buf.length) throw new Error('drpc: malformed frame: truncated')
        this.pos += 8
        return
      case 2:
        this.bytes()
        return
      case 5:
        this.fixed32()
        return
      default:
        throw new Error(`drpc: malformed frame: unsupported wire type ${wire}`)
    }
  }
}

// ---------------------------------------------------------------------------
// google.protobuf.Duration
// ---------------------------------------------------------------------------

function encodeDuration(ms: number): Uint8Array {
  const w = new Writer()
  const totalNs = BigInt(Math.round(ms * 1e6))
  const seconds = totalNs / 1_000_000_000n
  const nanos = totalNs % 1_000_000_000n // sign follows the dividend, as Duration requires
  if (seconds !== 0n) {
    w.tag(1, 0)
    w.varint64(seconds)
  }
  if (nanos !== 0n) {
    w.tag(2, 0)
    w.varint64(nanos)
  }
  return w.finish()
}

function decodeDuration(data: Uint8Array): number {
  const r = new Reader(data)
  let seconds = 0n
  let nanos = 0n
  while (!r.eof) {
    const tag = r.varint32()
    const field = tag >>> 3
    const wire = tag & 7
    if (field === 1 && wire === 0) seconds = BigInt.asIntN(64, r.varint())
    else if (field === 2 && wire === 0) nanos = BigInt.asIntN(32, r.varint())
    else r.skip(wire)
  }
  return Number(seconds) * 1000 + Number(nanos) / 1e6
}

// ---------------------------------------------------------------------------
// Metadata
// ---------------------------------------------------------------------------

function encodeMetadata(md: Metadata): Uint8Array {
  const w = new Writer()
  // Emit map entries sorted by key: protobuf map ordering is semantically
  // insignificant, but a stable order makes the encoding deterministic and
  // aligns it with Go's proto.Marshal Deterministic mode (which sorts map
  // keys), so re-encodings are byte-comparable. Value order within an entry
  // is preserved (it IS significant).
  for (const key of Object.keys(md).sort()) {
    const entry = new Writer()
    for (const v of md[key]!) entry.string(1, v)
    const kv = new Writer()
    kv.string(1, key)
    kv.bytes(2, entry.finish())
    w.bytes(1, kv.finish())
  }
  return w.finish()
}

function decodeMetadata(data: Uint8Array): Metadata {
  const md: Metadata = {}
  const r = new Reader(data)
  while (!r.eof) {
    const tag = r.varint32()
    if (tag >>> 3 === 1 && (tag & 7) === 2) {
      const kv = new Reader(r.bytes())
      let key = ''
      let values: string[] = []
      while (!kv.eof) {
        const t = kv.varint32()
        if (t >>> 3 === 1 && (t & 7) === 2) key = kv.string()
        else if (t >>> 3 === 2 && (t & 7) === 2) {
          const entry = new Reader(kv.bytes())
          while (!entry.eof) {
            const et = entry.varint32()
            if (et >>> 3 === 1 && (et & 7) === 2) values.push(entry.string())
            else entry.skip(et & 7)
          }
        } else kv.skip(t & 7)
      }
      md[key] = values // map semantics: last entry wins
    } else r.skip(tag & 7)
  }
  return md
}

// ---------------------------------------------------------------------------
// Frame / Envelop
// ---------------------------------------------------------------------------

export function encodeFrame(f: Frame): Uint8Array {
  const w = new Writer()
  if (f.epoch !== 0) w.fixed32(1, f.epoch)
  if (f.sid !== 0) w.fixed32(2, f.sid)
  if (f.seq !== 0) w.fixed32(3, f.seq)
  if (f.flags !== 0) {
    w.tag(4, 0)
    w.varint(f.flags)
  }
  if (f.method !== '') w.string(5, f.method)
  // field 6 reserved: was method_index, removed pre-release (§13).
  if (f.codec !== '') w.string(7, f.codec)
  if (f.timeoutMs !== undefined) w.bytes(8, encodeDuration(f.timeoutMs))
  if (f.payload !== undefined) w.bytes(9, f.payload)
  if (f.code !== undefined) {
    w.tag(10, 0)
    w.varint(f.code)
  }
  if (f.desc !== '') w.string(11, f.desc)
  if (f.header !== undefined) w.bytes(12, encodeMetadata(f.header))
  if (f.trailer !== undefined) w.bytes(13, encodeMetadata(f.trailer))
  if (f.peerEpoch !== 0) w.fixed32(14, f.peerEpoch)
  return w.finish()
}

export function decodeFrame(data: Uint8Array): Frame {
  const f = frame()
  const r = new Reader(data)
  while (!r.eof) {
    const tag = r.varint32()
    const field = tag >>> 3
    const wire = tag & 7
    switch (field) {
      case 1:
        if (wire !== 5) r.skip(wire)
        else f.epoch = r.fixed32()
        break
      case 2:
        if (wire !== 5) r.skip(wire)
        else f.sid = r.fixed32()
        break
      case 3:
        if (wire !== 5) r.skip(wire)
        else f.seq = r.fixed32()
        break
      case 4:
        if (wire !== 0) r.skip(wire)
        else f.flags = r.varint32()
        break
      case 5:
        if (wire !== 2) r.skip(wire)
        else f.method = r.string()
        break
      case 7:
        if (wire !== 2) r.skip(wire)
        else f.codec = r.string()
        break
      case 8:
        if (wire !== 2) r.skip(wire)
        else f.timeoutMs = decodeDuration(r.bytes())
        break
      case 9:
        if (wire !== 2) r.skip(wire)
        else f.payload = r.bytes().slice()
        break
      case 10:
        if (wire !== 0) r.skip(wire)
        else f.code = r.varint32()
        break
      case 11:
        if (wire !== 2) r.skip(wire)
        else f.desc = r.string()
        break
      case 12:
        if (wire !== 2) r.skip(wire)
        else f.header = decodeMetadata(r.bytes())
        break
      case 13:
        if (wire !== 2) r.skip(wire)
        else f.trailer = decodeMetadata(r.bytes())
        break
      case 14:
        if (wire !== 5) r.skip(wire)
        else f.peerEpoch = r.fixed32()
        break
      default:
        r.skip(wire)
    }
  }
  return f
}

// The wire unit is always one marshaled Envelop per transport message,
// holding 1..n frames processed in order (PROTOCOL.md §4.1).
export function encodeEnvelop(frames: readonly Frame[]): Uint8Array {
  const w = new Writer()
  for (const f of frames) w.bytes(1, encodeFrame(f))
  return w.finish()
}

export function decodeEnvelop(data: Uint8Array): Frame[] {
  const frames: Frame[] = []
  const r = new Reader(data)
  while (!r.eof) {
    const tag = r.varint32()
    if (tag >>> 3 === 1 && (tag & 7) === 2) frames.push(decodeFrame(r.bytes()))
    else r.skip(tag & 7)
  }
  return frames
}
