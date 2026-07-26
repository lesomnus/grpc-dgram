package main

import (
	"context"
	"math/rand/v2"

	drpc "github.com/lesomnus/grpc-dgram"
	"github.com/lesomnus/grpc-dgram/transport/udp"
)

// lossy sits in front of the UDP gateway's send path and throws away a
// fraction of outbound *data* frames.
//
// Loopback UDP is effectively lossless, so without it this example would
// demonstrate nothing: with it, the client receives the ordered subsequence
// the protocol promises and the §14 gap counter moves. On a real link — a
// drone over Wi-Fi, a browser over a cellular hop — the network does this for
// you; delete the wrapper and pass gw directly.
//
// Only data frames are dropped. Control frames (OPEN/CLOSE/RESET/PING/WINDOW)
// pass through untouched: losing a reading is the point, and the recovery
// machinery for lost control frames already has its own test suite.
type lossy struct {
	gw   *udp.Gateway
	rate float64
}

// Handle is drpc.FrameHandler: the core hands it one frame to transmit.
func (l *lossy) Handle(ctx context.Context, f *drpc.Frame) error {
	if isData(f) && rand.Float64() < l.rate {
		return nil // "the network ate it" — indistinguishable from real loss
	}
	return l.gw.Handle(ctx, f)
}

// Reliable forwards the gateway's own answer instead of hiding it: a wrapper
// must not mask capability discovery (PROTOCOL.md §4.3). UDP says false, so
// the core runs the full unreliable-mode machinery.
func (l *lossy) Reliable() bool { return l.gw.Reliable() }

// isData reports whether f carries a message rather than protocol control: no
// shape flag set, payload present. FlagCompressed is a modifier, not a shape,
// so it is masked out (PROTOCOL.md §7).
func isData(f *drpc.Frame) bool {
	return f.GetFlags()&^drpc.FlagCompressed == 0 && f.HasPayload()
}
