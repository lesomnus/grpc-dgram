package drpc

import (
	"math"
	"testing"

	"github.com/lesomnus/grpc-dgram/internal/x"
)

func TestRxWindow(t *testing.T) {
	t.Run("accepts in-window forward steps", func(t *testing.T) {
		w := rxWindow{}
		x.Equal(t, rxAccept, w.check(1))
		x.Equal(t, rxAccept, w.check(2))
		x.Equal(t, rxAccept, w.check(2+wFwd)) // max jump
	})
	t.Run("drops duplicates and older", func(t *testing.T) {
		w := rxWindow{}
		x.Equal(t, rxAccept, w.check(42))
		x.Equal(t, rxDrop, w.check(42)) // duplicate
		x.Equal(t, rxDrop, w.check(41)) // older
		x.Equal(t, rxDrop, w.check(1))  // much older
	})
	t.Run("seq 0 is malformed", func(t *testing.T) {
		w := rxWindow{}
		x.Equal(t, rxDrop, w.check(0))
	})
	t.Run("wraps around", func(t *testing.T) {
		w := rxWindow{l: math.MaxUint32 - 1}
		x.Equal(t, rxAccept, w.check(math.MaxUint32))
		x.Equal(t, rxAccept, w.check(3)) // forward across the wrap
		x.Equal(t, rxDrop, w.check(math.MaxUint32))
	})
	t.Run("lone beyond-window frame is dropped", func(t *testing.T) {
		w := rxWindow{}
		x.Equal(t, rxAccept, w.check(1))
		x.Equal(t, rxDrop, w.check(1+wFwd+1))
		// An accepted frame resets the run.
		x.Equal(t, rxAccept, w.check(2))
	})
	t.Run("K_loud consistent beyond-window frames fail loud", func(t *testing.T) {
		w := rxWindow{}
		x.Equal(t, rxAccept, w.check(1))
		base := 1 + wFwd + 1000
		x.Equal(t, rxDrop, w.check(base))
		x.Equal(t, rxDrop, w.check(base+1))
		x.Equal(t, rxDataLoss, w.check(base+2))
	})
	t.Run("delta 0 counts toward the run", func(t *testing.T) {
		// Byte-identical replays of a beyond-window T must accumulate
		// (PROTOCOL.md §6.3).
		w := rxWindow{}
		x.Equal(t, rxAccept, w.check(1))
		base := 1 + wFwd + 1000
		x.Equal(t, rxDrop, w.check(base))
		x.Equal(t, rxDrop, w.check(base))
		x.Equal(t, rxDataLoss, w.check(base))
	})
	t.Run("inconsistent beyond-window frames reset the run", func(t *testing.T) {
		w := rxWindow{}
		x.Equal(t, rxAccept, w.check(1))
		x.Equal(t, rxDrop, w.check(1+wFwd+1000))
		x.Equal(t, rxDrop, w.check(1+3*wFwd+3000)) // not within wFwd of previous
		x.Equal(t, rxDrop, w.check(1+wFwd+1001))   // run restarts
	})
}
