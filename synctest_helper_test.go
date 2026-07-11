package drpc_test

import (
	"testing"
	"testing/synctest"
)

// bubble runs f inside a synctest bubble: the drpc timers (retransmission,
// liveness, probes, tombstone GC) use a fake clock that advances instantly
// when every goroutine is blocked, so timing tests are fast AND deterministic.
// A leaked goroutine (a sweeper or handler that never exits) makes synctest
// panic — which directly validates G1 (no goroutine outlives its bound).
func bubble(t *testing.T, f func(t *testing.T)) {
	t.Helper()
	synctest.Test(t, f)
}
