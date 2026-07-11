package drpc

const (
	// wFwd is the max forward seq jump a receiver accepts (PROTOCOL.md §6.3).
	wFwd uint32 = 4096
	// kLoud consecutive, mutually consistent beyond-window arrivals are
	// evidence of genuine sender progress past a >wFwd loss burst; the call
	// fails loudly with DATA_LOSS (PROTOCOL.md §6.3).
	kLoud = 3
)

// txSeq numbers outgoing frames of one stream direction, starting at 1.
// Callers serialize access (the stream's tx mutex).
type txSeq struct{ v uint32 }

func (s *txSeq) next() uint32 { s.v++; return s.v }

// undo returns seq to the pool after the adapter refused its frame
// synchronously (drpc.ErrMessageTooLarge): the frame never reached the wire,
// so the number must be reused — a permanent hole would fail every later
// frame of the stream under reliable mode's strict window (PROTOCOL.md
// §4.4, §10.6). No-op unless seq is the latest allocation.
func (s *txSeq) undo(seq uint32) {
	if seq != 0 && s.v == seq {
		s.v--
	}
}

type rxVerdict int

const (
	// rxAccept: in-window forward step; deliver.
	rxAccept rxVerdict = iota
	// rxDup: duplicate or older frame; dropped, but still a validated frame
	// from the peer (it refreshes liveness/idle clocks, PROTOCOL.md §9.1).
	rxDup
	// rxBeyond: lone beyond-window frame; dropped, NOT validated.
	rxBeyond
	// rxDataLoss: kLoud consistent beyond-window arrivals; fail the call.
	rxDataLoss
	// rxProtocolError: reliable mode saw a gap or duplicate — the transport
	// is broken; fail the call with INTERNAL (PROTOCOL.md §10.6).
	rxProtocolError
)

// rxWindow validates per-stream, per-direction sequence numbers.
// L initializes to 0: the server direction starts with the OPEN (seq 1);
// the client side may legitimately accept any first seq in [1, wFwd]
// (PROTOCOL.md §6.3). Callers serialize access per stream.
type rxWindow struct {
	l          uint32 // highest accepted seq
	beyondN    int    // length of the current consistent beyond-window run
	beyondLast uint32 // last beyond-window seq seen
	strict     bool   // reliable mode: require exactly l+1, else fail loud
}

func (w *rxWindow) check(seq uint32) rxVerdict {
	if seq == 0 {
		// Stateless frames (RESET, PING) never reach seq validation;
		// a sequenced frame with seq 0 is malformed.
		return rxBeyond
	}
	if w.strict {
		// Reliable, ordered transport: exactly one forward step is legal.
		// Anything else means the transport lost, duplicated, or reordered a
		// frame — a contract violation, not expected noise (PROTOCOL.md §10.6).
		if seq == w.l+1 {
			w.l = seq
			return rxAccept
		}
		return rxProtocolError
	}
	switch d := seq - w.l; { // mod 2^32
	case d == 0 || d >= 1<<31:
		// Duplicate or older: dedup. Neutral for the beyond-run (§6.3).
		return rxDup
	case d <= wFwd:
		w.l = seq
		w.beyondN = 0
		return rxAccept
	default:
		// Beyond-window. Delta from the previous beyond-window frame must be
		// in [0, wFwd] to count as consistent — delta 0 included, so
		// byte-identical replays of a beyond-window T accumulate (§6.3).
		if w.beyondN > 0 && seq-w.beyondLast <= wFwd {
			w.beyondN++
		} else {
			w.beyondN = 1
		}
		w.beyondLast = seq
		if w.beyondN >= kLoud {
			return rxDataLoss
		}
		return rxBeyond
	}
}
