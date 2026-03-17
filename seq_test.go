package drpc

import (
	"testing"

	"github.com/lesomnu/grpc-dgram/internal/x"
)

func TestCheckAndSet(t *testing.T) {
	var v rx_seq

	ok := v.checkAndSet(1)
	x.True(t, ok)
	x.Equal(t, 1, int(v))

	ok = v.checkAndSet(42)
	x.True(t, ok)
	x.Equal(t, 42, int(v))

	// Distance between 42 - (-1) = 43 -> too close.
	ok = v.checkAndSet(0xFFFF_FFFF)
	x.False(t, ok)
	x.Equal(t, 42, int(v))

	ok = v.checkAndSet(seqMax + 42)
	x.False(t, ok)
	x.Equal(t, 42, int(v))

	ok = v.checkAndSet(seqMax + 41)
	x.True(t, ok)
	x.Equal(t, seqMax+41, v)
}
