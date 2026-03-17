package drpc

const (
	seqMax rx_seq = 0x7FFF_FFFF
)

type tx_seq uint32

func (v *tx_seq) next() uint32 {
	w := *v
	*v++
	return uint32(w)
}

type rx_seq uint32

func (v *rx_seq) checkAndSet(remote rx_seq) bool {
	local := *v
	if (remote != local) && ((remote - local) < seqMax) {
		*v = remote
		return true
	}
	return false
}
