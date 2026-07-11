package drpc

// White-box: the sid-exhaustion latch (PROTOCOL.md §6.2) needs the unexported
// sidNext/exhausted fields, and the nonzero-epoch rule (§6.1) pins the
// unexported epoch draw the whole peer_epoch scheme relies on (0 means
// "absent echo", so no incarnation may be named 0).

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestSidExhaustionLatch(t *testing.T) {
	tx := FrameHandlerFunc(func(context.Context, *Frame) error { return nil })
	// Reliable mode: no timers, no sweeper — the latch is pure bookkeeping.
	c := NewConn(tx, WithReliable(true))
	defer c.Close(nil)

	c.mu.Lock()
	c.sidNext = ^uint32(0) - 1 // next allocation yields the final sid
	c.mu.Unlock()

	// The last sid of the space is still allocatable.
	s, err := c.newStream(context.Background(), "/a.B/C", false, false)
	if err != nil {
		t.Fatalf("the final sid must still be allocatable: %v", err)
	}
	if s.sid != ^uint32(0) {
		t.Fatalf("expected the final sid %d, got %d", ^uint32(0), s.sid)
	}

	// The next allocation wraps sidNext to 0: the space is spent. sids are
	// never recycled within an epoch (§6.2) — RESOURCE_EXHAUSTED, and the
	// exhausted latch trips.
	_, err = c.newStream(context.Background(), "/a.B/C", false, false)
	if got := status.Code(err); got != codes.ResourceExhausted {
		t.Fatalf("expected ResourceExhausted at sid wrap, got %v (err=%v)", got, err)
	}
	c.mu.Lock()
	latched := c.exhausted
	c.mu.Unlock()
	if !latched {
		t.Fatal("the exhausted latch must trip at sid wrap")
	}

	// Sticky: every later attempt fails the same way; the application must
	// create a new Conn (new epoch) to keep calling.
	for i := range 3 {
		_, err := c.newStream(context.Background(), "/a.B/C", false, false)
		if got := status.Code(err); got != codes.ResourceExhausted {
			t.Fatalf("attempt %d: the latch must be sticky, got %v (err=%v)", i, got, err)
		}
	}

	s.finishLocal(status.Error(codes.Canceled, "test cleanup"))
}

func TestEpochNeverZero(t *testing.T) {
	// §6.1: an epoch is a uniformly random, NONZERO fixed32 — 0 marks an
	// absent peer_epoch echo, so an incarnation named 0 could be addressed by
	// frames that echo nothing.
	for i := range 4096 {
		if nonzeroEpoch() == 0 {
			t.Fatalf("draw %d: nonzeroEpoch returned 0", i)
		}
	}

	tx := FrameHandlerFunc(func(context.Context, *Frame) error { return nil })
	c := NewConn(tx, WithReliable(true))
	defer c.Close(nil)
	if c.epoch == 0 {
		t.Fatal("Conn epoch must be nonzero (§6.1)")
	}

	s := NewServer(tx, WithReliable(true))
	defer s.Stop()
	if s.epoch == 0 {
		t.Fatal("Server epoch must be nonzero (§6.1)")
	}
}
