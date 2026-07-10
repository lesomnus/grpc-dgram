package lossy

import (
	"context"
	"math/rand/v2"
	"sync"
	"sync/atomic"

	drpc "github.com/lesomnus/grpc-dgram"
	"google.golang.org/protobuf/proto"
)

// Options configures a frame-level fault injector.
type Options struct {
	// Seed makes every run reproducible; print it when a test fails.
	Seed uint64

	// Probabilities in [0, 1], evaluated per selected frame in this order.
	Drop float64
	Dup  float64
	// Hold keeps the frame back and releases it right after the next frame
	// is delivered — a one-step reordering. At most one frame is held.
	Hold float64

	// Filter selects which frames faults apply to; nil selects every frame.
	// Unselected frames pass through untouched, but still release a held
	// frame behind them.
	Filter func(f *drpc.Frame) bool
}

// Lossy decorates a FrameHandler with seeded, reproducible fault injection:
// dropped, duplicated, and one-step-reordered frames. Duplicates and held
// frames are clones; originals are never mutated.
type Lossy struct {
	next drpc.FrameHandler
	opt  Options

	mu   sync.Mutex
	rng  *rand.Rand
	held *drpc.Frame

	Dropped   atomic.Uint64
	Dupped    atomic.Uint64
	Reordered atomic.Uint64
}

func New(next drpc.FrameHandler, opt Options) *Lossy {
	return &Lossy{
		next: next,
		opt:  opt,
		rng:  rand.New(rand.NewPCG(opt.Seed, 0)),
	}
}

func (l *Lossy) Handle(ctx context.Context, f *drpc.Frame) error {
	l.mu.Lock()
	apply := l.opt.Filter == nil || l.opt.Filter(f)
	drop := apply && l.rng.Float64() < l.opt.Drop
	dup := apply && !drop && l.rng.Float64() < l.opt.Dup
	hold := apply && !drop && l.held == nil && l.rng.Float64() < l.opt.Hold

	var out []*drpc.Frame
	switch {
	case drop:
		l.Dropped.Add(1)
	case hold:
		l.held = proto.CloneOf(f)
		l.Reordered.Add(1)
	default:
		out = append(out, f)
		if dup {
			out = append(out, proto.CloneOf(f))
			l.Dupped.Add(1)
		}
	}
	if l.held != nil && !hold {
		// Release the held frame behind the newer one.
		out = append(out, l.held)
		l.held = nil
	}
	l.mu.Unlock()

	for _, g := range out {
		if err := l.next.Handle(ctx, g); err != nil {
			return err
		}
	}
	return nil
}

// Flush delivers a held frame, if any. Call at teardown so the injector
// itself never eats the last frame of an exchange.
func (l *Lossy) Flush(ctx context.Context) error {
	l.mu.Lock()
	f := l.held
	l.held = nil
	l.mu.Unlock()
	if f == nil {
		return nil
	}
	return l.next.Handle(ctx, f)
}
