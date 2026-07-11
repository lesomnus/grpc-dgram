package drpc

// This file holds the configurable delivery buffers (decision Q3) and the
// resource caps (PROTOCOL.md §15).

// DropPolicy selects what a full per-stream rx buffer discards in unreliable
// mode (PROTOCOL.md §4.2).
type DropPolicy int

const (
	// DropNewest discards the arriving frame (the default): the buffered
	// prefix is preserved.
	DropNewest DropPolicy = iota
	// DropOldest discards the oldest buffered frame to admit the newest —
	// freshest-wins, suited to state-sync / sensor streams where the latest
	// reading supersedes older ones.
	DropOldest
)

// rxConfig is a resolved per-stream buffer setting.
type rxConfig struct {
	size   int
	policy DropPolicy
}

const (
	defaultRxBuffer       = 32
	defaultMaxTombs       = 1024
	defaultMaxTombBytes   = 1 << 20
	defaultMaxDeadPeers   = 4
	defaultMaxResets      = 1024
	defaultMaxLiveCalls   = 4096
	defaultRxDropNewest   = DropNewest
	defaultMaxHandlerZero = 0
)

func (c rxConfig) withDefaults() rxConfig {
	if c.size <= 0 {
		c.size = defaultRxBuffer
	}
	return c
}

// Limits bounds the server's per-peer bookkeeping (PROTOCOL.md §15). Zero
// fields keep their defaults. On a Conn only MaxPendingResets applies.
type Limits struct {
	// MaxTombstones caps stored tombstone entries per peer incarnation;
	// oldest are dropped to key-only past it.
	MaxTombstones int
	// MaxTombstoneBytes caps stored terminal-frame payload bytes per peer;
	// oldest stored terminals degrade to key-only past it.
	MaxTombstoneBytes int
	// MaxDeadPeers caps retained finished peer incarnations per transport
	// peer; oldest are evicted (never one with live calls).
	MaxDeadPeers int
	// MaxPendingResets caps the RESET rate-limit / delayed-RESET maps.
	MaxPendingResets int
	// MaxLiveCalls caps concurrently live calls per peer incarnation; an
	// OPEN past it is refused with RESOURCE_EXHAUSTED, bounding the handler
	// goroutines a single peer can spawn.
	MaxLiveCalls int
}

func (l Limits) withDefaults() Limits {
	if l.MaxTombstones <= 0 {
		l.MaxTombstones = defaultMaxTombs
	}
	if l.MaxTombstoneBytes <= 0 {
		l.MaxTombstoneBytes = defaultMaxTombBytes
	}
	if l.MaxDeadPeers <= 0 {
		l.MaxDeadPeers = defaultMaxDeadPeers
	}
	if l.MaxPendingResets <= 0 {
		l.MaxPendingResets = defaultMaxResets
	}
	if l.MaxLiveCalls <= 0 {
		l.MaxLiveCalls = defaultMaxLiveCalls
	}
	return l
}

// WithRxBuffer sets the default per-stream rx buffer size and drop policy for
// every call (PROTOCOL.md §4.2, decision Q3). size <= 0 keeps the default 32.
func WithRxBuffer(size int, policy DropPolicy) interface {
	ConnOption
	ServerOption
} {
	return limitsOption{rx: &rxConfig{size: size, policy: policy}}
}

// WithMethodRxBuffer overrides the rx buffer for one full method name
// (e.g. "/echo.EchoService/Live") on the server — a high-rate sensor stream
// can carry a deeper buffer and DropOldest while control RPCs keep the
// default. Most-specific wins: method override, else the WithRxBuffer
// default, else the built-in default.
func WithMethodRxBuffer(fullMethod string, size int, policy DropPolicy) ServerOption {
	return serverOptionFunc(func(o *serverOption) {
		if o.methodRx == nil {
			o.methodRx = map[string]rxConfig{}
		}
		o.methodRx[fullMethod] = rxConfig{size: size, policy: policy}
	})
}

// WithLimits sets the resource caps (PROTOCOL.md §15). Zero fields keep
// defaults.
func WithLimits(l Limits) interface {
	ConnOption
	ServerOption
} {
	return limitsOption{limits: &l}
}

type limitsOption struct {
	rx     *rxConfig
	limits *Limits
}

func (o limitsOption) apply(c *connOption) {
	if o.rx != nil {
		c.rx = *o.rx
	}
	if o.limits != nil {
		c.limits = *o.limits
	}
}

func (o limitsOption) applyServer(s *serverOption) {
	if o.rx != nil {
		s.rx = *o.rx
	}
	if o.limits != nil {
		s.limits = *o.limits
	}
}
