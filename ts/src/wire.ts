// The drpc wire format (PROTOCOL.md §5): a hand-rolled protobuf codec for
// exactly three messages — Frame, Envelop, Metadata (plus the two well-known
// messages they embed, Duration and Any) — so the core carries no protobuf
// runtime dependency. The encoding must stay byte-identical to the Go
// implementation for the §5 golden vectors (wire.test.ts): fields are written
// in field-number order, implicit-presence fields are omitted at their
// default, and `payload`/`code` carry explicit presence (undefined = absent
// on the wire).

import { decodeMetadataValue, encodeMetadataValue, type Metadata } from './metadata'
import { Code, StatusError, toStatusError } from './status'

// Frame flags (PROTOCOL.md §7).
//
// The first five bits name the frame's SHAPE and are what every routing
// decision looks at; FlagCompressed is an orthogonal marker that may ride any
// payload-bearing frame. Shape tests therefore mask (shapeOf) instead of
// comparing the whole bitmask — a compressed data frame must still read as a
// data frame.
export const FlagOpen = 1
export const FlagClose = 2
export const FlagReset = 4
export const FlagPing = 8
// FlagWindow is a stateless flow-control grant for the frame's sid: its
// `window` field adds that many messages of credit (reliable mode, §4.2).
export const FlagWindow = 16
// FlagCompressed marks a frame whose payload is compressed with the call's
// compressor (§12.1). Orthogonal to the shape flags.
export const FlagCompressed = 32

// SHAPE_MASK is the mask of shape-bearing flags; KNOWN_FLAGS adds every
// modifier bit this implementation understands. A frame carrying a bit
// outside KNOWN_FLAGS was built by a newer peer and MUST NOT be delivered:
// the receiver cannot know what the bit changes about the payload (§7.1).
export const SHAPE_MASK = FlagOpen | FlagClose | FlagReset | FlagPing | FlagWindow // 0x1f
export const KNOWN_FLAGS = SHAPE_MASK | FlagCompressed // 0x3f

// google.protobuf.Any, as carried by `Frame.details` (§5). Hand-modeled like
// Duration: the codec owns these two fields and nothing else.
export interface Any {
  typeUrl: string
  value: Uint8Array
}

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
  // Flow-control credit, in messages, for this stream — reliable mode only
  // (§4.2). On OPEN and on the creation-ack H it advertises the sender's
  // initial receive window; on a WINDOW frame it is an additive grant.
  // 0 = absent, i.e. "this side does no flow control".
  window: number
  // Message compressor name; OPEN frames only; '' = none. Like `codec`, it
  // governs the whole call in both directions (§12.1).
  compressor: string
  // Rich status details — the payload of google.rpc.Status.details; terminal
  // frames only (§5). A repeated field: absent and empty are the same bytes,
  // so undefined is the canonical "none".
  details?: Any[]
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
    window: 0,
    compressor: '',
    ...init,
  }
}

export const isOpen = (f: Frame): boolean => (f.flags & FlagOpen) !== 0
export const isClose = (f: Frame): boolean => (f.flags & FlagClose) !== 0
export const isReset = (f: Frame): boolean => (f.flags & FlagReset) !== 0
export const isPing = (f: Frame): boolean => (f.flags & FlagPing) !== 0
export const isWindow = (f: Frame): boolean => (f.flags & FlagWindow) !== 0
export const isCompressed = (f: Frame): boolean => (f.flags & FlagCompressed) !== 0

// shapeOf returns the frame's shape bits, with orthogonal markers stripped.
export const shapeOf = (f: Frame): number => f.flags & SHAPE_MASK

// hasUnknownFlags reports whether the frame carries a modifier bit this
// implementation does not understand (§7.1).
export const hasUnknownFlags = (f: Frame): boolean => (f.flags & ~KNOWN_FLAGS) !== 0

// legalShape reports whether a shape is one the protocol defines. Shape bits
// are mutually exclusive with one exception — OPEN|CLOSE, the §8 unary and
// server-streaming request — so every other combination is a frame no
// receiver can route (§7.1). A receiver must neither deliver nor silently
// drop such a frame: it fails the call with INTERNAL.
export function legalShape(shape: number): boolean {
  switch (shape) {
    case 0:
    case FlagOpen:
    case FlagClose:
    case FlagOpen | FlagClose:
    case FlagReset:
    case FlagPing:
    case FlagWindow:
      return true
    default:
      return false
  }
}

// Terminal CLOSE: a call result from the server, or an abort from the client.
export const isTerminal = (f: Frame): boolean => shapeOf(f) === FlagClose && f.code !== undefined
// Client half-close: send direction done, call continues.
export const isHalfClose = (f: Frame): boolean => shapeOf(f) === FlagClose && f.code === undefined
// Data frame: no shape flags, payload present (even 0 bytes, §7). The shape
// mask is what makes a COMPRESSED data frame still read as a data frame.
export const isData = (f: Frame): boolean => shapeOf(f) === 0 && f.payload !== undefined
// Header frame H: no shape flags, no payload.
export const isHeaderFrame = (f: Frame): boolean => shapeOf(f) === 0 && f.payload === undefined

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
// google.protobuf.Any
// ---------------------------------------------------------------------------

function encodeAny(a: Any): Uint8Array {
  const w = new Writer()
  if (a.typeUrl !== '') w.string(1, a.typeUrl)
  if (a.value.length > 0) w.bytes(2, a.value)
  return w.finish()
}

function decodeAny(data: Uint8Array): Any {
  const r = new Reader(data)
  // A fresh empty array per Any (never a shared one): `value` is a mutable
  // buffer the caller owns.
  const a: Any = { typeUrl: '', value: new Uint8Array(0) }
  while (!r.eof) {
    const tag = r.varint32()
    const field = tag >>> 3
    const wire = tag & 7
    if (field === 1 && wire === 2) a.typeUrl = r.string()
    else if (field === 2 && wire === 2) a.value = r.bytes().slice()
    else r.skip(wire)
  }
  return a
}

// ---------------------------------------------------------------------------
// Metadata
// ---------------------------------------------------------------------------
//
// `Metadata.Entry.values` is `repeated bytes` (wire v1.1): the per-key
// transform between the TS representation and those octets lives in
// metadata.ts ("-bin" = base64 here / raw octets there; anything else UTF-8).

function encodeMetadata(md: Metadata): Uint8Array {
  const w = new Writer()
  // Emit map entries sorted by key: protobuf map ordering is semantically
  // insignificant, but a stable order makes the encoding deterministic and
  // aligns it with Go's proto.Marshal Deterministic mode (which sorts map
  // keys), so re-encodings are byte-comparable. Value order within an entry
  // is preserved (it IS significant).
  for (const key of Object.keys(md).sort()) {
    const entry = new Writer()
    for (const v of md[key]!) entry.bytes(1, encodeMetadataValue(key, v))
    const kv = new Writer()
    // Both map-entry fields are emitted unconditionally, even at their
    // default: that is what protobuf-go's map encoder does, and an empty key
    // or an entry with no values must produce identical bytes on both sides.
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
      // Values are collected as raw octets first: field order inside a map
      // entry is not guaranteed (the key may arrive AFTER the values), and
      // the per-value transform depends on the key. It therefore runs once
      // the entry has closed, never inside the loop.
      const raw: Uint8Array[] = []
      while (!kv.eof) {
        const t = kv.varint32()
        if (t >>> 3 === 1 && (t & 7) === 2) key = kv.string()
        else if (t >>> 3 === 2 && (t & 7) === 2) {
          // A repeated `value` field merges, as proto message merging does.
          const entry = new Reader(kv.bytes())
          while (!entry.eof) {
            const et = entry.varint32()
            if (et >>> 3 === 1 && (et & 7) === 2) raw.push(entry.bytes())
            else entry.skip(et & 7)
          }
        } else kv.skip(t & 7)
      }
      md[key] = raw.map((b) => decodeMetadataValue(key, b)) // map semantics: last entry wins
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
  // Truthiness, not `!== 0` / `!== ''`: a frame handed in by JS (or by a
  // hand-built partial literal) must still encode canonically rather than
  // emit `window: 0` or the string "undefined" as a compressor name.
  if (f.window) {
    w.tag(15, 0)
    w.varint(f.window)
  }
  if (f.compressor) w.string(16, f.compressor)
  if (f.details !== undefined) for (const d of f.details) w.bytes(17, encodeAny(d))
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
      case 15:
        if (wire !== 0) r.skip(wire)
        else f.window = r.varint32()
        break
      case 16:
        if (wire !== 2) r.skip(wire)
        else f.compressor = r.string()
        break
      case 17:
        if (wire !== 2) r.skip(wire)
        else (f.details ??= []).push(decodeAny(r.bytes()))
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
