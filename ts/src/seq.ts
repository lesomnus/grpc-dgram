// Sequence numbering and the receive window (PROTOCOL.md §6.3).

// W_FWD is the max forward seq jump a receiver accepts; K_LOUD consecutive,
// mutually consistent beyond-window arrivals fail the call with DATA_LOSS.
// Both are fixed protocol constants, not options (PROTOCOL.md §10.1).
export const W_FWD = 4096
export const K_LOUD = 3

// TxSeq numbers outgoing frames of one stream direction, starting at 1.
export class TxSeq {
  private v = 0

  next(): number {
    this.v = (this.v + 1) >>> 0
    return this.v
  }

  // undo returns seq to the pool after the adapter refused its frame
  // synchronously (MessageTooLargeError): the frame never reached the wire,
  // so the number must be reused — a permanent hole would fail every later
  // frame of the stream under reliable mode's strict window (PROTOCOL.md
  // §4.4, §10.6). No-op unless seq is the latest allocation.
  undo(seq: number): void {
    if (seq !== 0 && this.v === seq) this.v = (this.v - 1) >>> 0
  }
}

export enum RxVerdict {
  // In-window forward step; deliver.
  Accept,
  // Duplicate or older frame; dropped, but still a validated frame from the
  // peer (it refreshes liveness/idle clocks, PROTOCOL.md §9.1).
  Dup,
  // Lone beyond-window frame; dropped, NOT validated.
  Beyond,
  // K_LOUD consistent beyond-window arrivals; fail the call.
  DataLoss,
  // Reliable mode saw a gap or duplicate — the transport is broken; fail the
  // call with INTERNAL (PROTOCOL.md §10.6).
  ProtocolError,
}

// RxWindow validates per-stream, per-direction sequence numbers.
// l initializes to 0: the server direction starts with the OPEN (seq 1);
// the client side may legitimately accept any first seq in [1, W_FWD]
// (PROTOCOL.md §6.3).
export class RxWindow {
  l = 0 // highest accepted seq
  strict = false // reliable mode: require exactly l+1, else fail loud
  private beyondN = 0 // length of the current consistent beyond-window run
  private beyondLast = 0 // last beyond-window seq seen

  check(seq: number): RxVerdict {
    if (seq === 0) {
      // Stateless frames (RESET, PING) never reach seq validation; a
      // sequenced frame with seq 0 is malformed.
      return RxVerdict.Beyond
    }
    if (this.strict) {
      // Reliable, ordered transport: exactly one forward step is legal.
      // Anything else means the transport lost, duplicated, or reordered a
      // frame — a contract violation, not expected noise (PROTOCOL.md §10.6).
      if (seq === ((this.l + 1) >>> 0)) {
        this.l = seq
        return RxVerdict.Accept
      }
      return RxVerdict.ProtocolError
    }
    const d = (seq - this.l) >>> 0 // mod 2^32
    if (d === 0 || d >= 0x80000000) {
      // Duplicate or older: dedup. Neutral for the beyond-run (§6.3).
      return RxVerdict.Dup
    }
    if (d <= W_FWD) {
      this.l = seq
      this.beyondN = 0
      return RxVerdict.Accept
    }
    // Beyond-window. Delta from the previous beyond-window frame must be in
    // [0, W_FWD] to count as consistent — delta 0 included, so byte-identical
    // replays of a beyond-window T accumulate (§6.3).
    if (this.beyondN > 0 && ((seq - this.beyondLast) >>> 0) <= W_FWD) {
      this.beyondN++
    } else {
      this.beyondN = 1
    }
    this.beyondLast = seq
    if (this.beyondN >= K_LOUD) return RxVerdict.DataLoss
    return RxVerdict.Beyond
  }
}
