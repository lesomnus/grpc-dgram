package drpc

// Frame flags. See PROTOCOL.md §7.
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
)

func (x *Frame) isOpen() bool  { return x.GetFlags()&FlagOpen != 0 }
func (x *Frame) isClose() bool { return x.GetFlags()&FlagClose != 0 }
func (x *Frame) isReset() bool { return x.GetFlags()&FlagReset != 0 }
func (x *Frame) isPing() bool  { return x.GetFlags()&FlagPing != 0 }

// isTerminal reports whether x is a terminal CLOSE: a call result from the
// server, or an abort from the client.
func (x *Frame) isTerminal() bool { return x.isClose() && x.HasCode() }

// isHalfClose reports whether x is a client half-close: send direction done,
// call continues.
func (x *Frame) isHalfClose() bool { return x.isClose() && !x.HasCode() }

// isData reports whether x is a data frame: no flags, payload present.
// Payload presence is meaningful even for 0-byte messages (§7).
func (x *Frame) isData() bool { return x.GetFlags() == 0 && x.HasPayload() }

// isHeaderFrame reports whether x is a header frame H: no flags, no payload.
func (x *Frame) isHeaderFrame() bool { return x.GetFlags() == 0 && !x.HasPayload() }
