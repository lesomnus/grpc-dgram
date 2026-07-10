package drpc

import "time"

// TransportInfo advertises transport capabilities. Adapters implement it
// alongside their handler; NewConn/NewServer discover it once at
// construction by type-asserting their tx argument. Explicit options always
// override discovery. See PROTOCOL.md §4.3.
type TransportInfo interface {
	// Reliable reports that the transport neither loses, duplicates, nor
	// reorders messages. In reliable mode the protocol runs without timers
	// or retransmission (PROTOCOL.md §10.6).
	Reliable() bool
	// MaxMessageSize is the max marshaled Envelop bytes per transport
	// message; 0 means unlimited.
	MaxMessageSize() int
}

// Timing holds the protocol timers of unreliable mode (PROTOCOL.md §10.1).
// The zero value of a field selects its default.
type Timing struct {
	// Call is T_call: the default unary deadline injected when the caller's
	// ctx has none. Client side only.
	Call time.Duration
	// Liveness is T_live: the peer-liveness window. The probe threshold and
	// cadence T_probe is Liveness/3.
	Liveness time.Duration
	// Retransmit is RTI: the control-frame retransmission base interval;
	// it doubles per attempt up to the probe cadence.
	Retransmit time.Duration
	// Tombstone is TTL_tomb: how long finished calls are remembered.
	Tombstone time.Duration
	// Hold is T_hold: the delayed-RESET grace for unknown-sid frames whose
	// OPEN may merely be late.
	Hold time.Duration
}

func (t Timing) withDefaults() Timing {
	if t.Call == 0 {
		t.Call = 5 * time.Second
	}
	if t.Liveness == 0 {
		t.Liveness = 15 * time.Second
	}
	if t.Retransmit == 0 {
		t.Retransmit = time.Second
	}
	if t.Tombstone == 0 {
		t.Tombstone = 30 * time.Second
	}
	if t.Tombstone < 2*t.Liveness {
		// TTL_tomb floor while liveness is enabled (PROTOCOL.md §9.2).
		t.Tombstone = 2 * t.Liveness
	}
	if t.Hold == 0 {
		t.Hold = t.Retransmit
	}
	return t
}

// probe is T_probe: stream-probe idle threshold and cadence, and the
// retransmission backoff cap.
func (t Timing) probe() time.Duration {
	return t.Liveness / 3
}

// tick is the coarse sweep period (PROTOCOL.md Appendix C): fine enough that
// every timer tolerates the jitter, bounded so idle cost stays negligible.
func (t Timing) tick() time.Duration {
	d := min(t.Retransmit, t.Hold) / 2
	return max(time.Millisecond, min(d, 500*time.Millisecond))
}

// mode aggregates the resolved transport profile.
type mode struct {
	reliable bool
	timing   Timing
}

// resolveMode derives the mode from an explicit option (tri-state) or the
// transport's TransportInfo, defaulting to unreliable — the transport class
// this library exists for.
func resolveMode(tx FrameHandler, reliable *bool, timing Timing) mode {
	m := mode{timing: timing.withDefaults()}
	if reliable != nil {
		m.reliable = *reliable
	} else if info, ok := tx.(TransportInfo); ok {
		m.reliable = info.Reliable()
	}
	return m
}

func WithReliable(v bool) interface {
	ConnOption
	ServerOption
} {
	return modeOption{reliable: &v}
}

// WithTiming overrides protocol timers (unreliable mode only). Zero fields
// keep their defaults.
func WithTiming(t Timing) interface {
	ConnOption
	ServerOption
} {
	return modeOption{timing: &t}
}

type modeOption struct {
	reliable *bool
	timing   *Timing
}

func (o modeOption) apply(c *connOption) {
	if o.reliable != nil {
		c.reliable = o.reliable
	}
	if o.timing != nil {
		c.timing = *o.timing
	}
}

func (o modeOption) applyServer(s *serverOption) {
	if o.reliable != nil {
		s.reliable = o.reliable
	}
	if o.timing != nil {
		s.timing = *o.timing
	}
}

// WithMaxHandlerTimeout clamps client-asserted timeouts on the server
// (PROTOCOL.md §10.2, decision Q5). Off unless set.
func WithMaxHandlerTimeout(d time.Duration) ServerOption {
	return serverOptionFunc(func(o *serverOption) {
		o.maxHandlerTimeout = d
	})
}
