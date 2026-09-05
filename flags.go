package drpc

// Frame flags. See PROTOCOL.md §7.
//
// The first five bits name the frame's SHAPE and are what every routing
// decision looks at; FlagCompressed is an orthogonal marker that may ride any
// payload-bearing frame. Shape tests therefore mask (shape()) instead of
// comparing the whole bitmask — a compressed data frame must still read as a
// data frame.
const (
	// FlagOpen creates the call. Client→server only; seq MUST be 1.
	FlagOpen uint32 = 1 << iota
	// FlagClose means the sender's direction is finished. Without code
	// (client only) it is a half-close; with code it is terminal.
	FlagClose
	// FlagReset is a stateless "I have no such call"; epoch echoes the
	// offending frame.
	FlagReset
	// FlagPing is a liveness keepalive (sid 0) or stream probe (sid != 0).
	FlagPing
	// FlagWindow is a stateless flow-control grant: its window field adds
	// that many messages of credit — for the call named by sid, or for the
	// peer's connection window when sid is 0 (reliable mode, §4.2.1).
	FlagWindow
	// FlagCompressed marks a frame whose payload is compressed with the
	// call's compressor (§12.1). Orthogonal to the shape flags.
	FlagCompressed
)

// flagShape is the mask of shape-bearing flags; flagKnown adds every modifier
// bit this implementation understands. A frame carrying a bit outside
// flagKnown was built by a newer peer and MUST NOT be delivered: the receiver
// cannot know what the bit changes about the payload (PROTOCOL.md §7.1).
const (
	flagShape = FlagOpen | FlagClose | FlagReset | FlagPing | FlagWindow
	flagKnown = flagShape | FlagCompressed
)

// hasUnknownFlags reports whether the frame carries a modifier bit this
// implementation does not understand.
func (x *Frame) hasUnknownFlags() bool { return x.GetFlags()&^flagKnown != 0 }

// legalShape reports whether a shape is one the protocol defines. Shape bits
// are mutually exclusive with one exception — OPEN|CLOSE, the §8 unary and
// server-streaming request — so every other combination is a frame no
// receiver can route (PROTOCOL.md §7.1).
func legalShape(shape uint32) bool {
	switch shape {
	case 0, FlagOpen, FlagClose, FlagOpen | FlagClose, FlagReset, FlagPing, FlagWindow:
		return true
	}
	return false
}

// shape returns the frame's shape bits, with orthogonal markers stripped.
func (x *Frame) shape() uint32 { return x.GetFlags() & flagShape }

func (x *Frame) isOpen() bool       { return x.GetFlags()&FlagOpen != 0 }
func (x *Frame) isClose() bool      { return x.GetFlags()&FlagClose != 0 }
func (x *Frame) isReset() bool      { return x.GetFlags()&FlagReset != 0 }
func (x *Frame) isPing() bool       { return x.GetFlags()&FlagPing != 0 }
func (x *Frame) isWindow() bool     { return x.GetFlags()&FlagWindow != 0 }
func (x *Frame) isCompressed() bool { return x.GetFlags()&FlagCompressed != 0 }

// isTerminal reports whether x is a terminal CLOSE: a call result from the
// server, or an abort from the client.
func (x *Frame) isTerminal() bool { return x.shape() == FlagClose && x.HasCode() }

// isHalfClose reports whether x is a client half-close: send direction done,
// call continues.
func (x *Frame) isHalfClose() bool { return x.shape() == FlagClose && !x.HasCode() }

// isData reports whether x is a data frame: no shape flags, payload present.
// Payload presence is meaningful even for 0-byte messages (§7).
func (x *Frame) isData() bool { return x.shape() == 0 && x.HasPayload() }

// isHeaderFrame reports whether x is a header frame H: no shape flags, no
// payload.
func (x *Frame) isHeaderFrame() bool { return x.shape() == 0 && !x.HasPayload() }
