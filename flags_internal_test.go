package drpc

import "testing"

// PROTOCOL.md §10.6 reserves modifier bit 64 as the marker a breaking
// generation sets on the first frame of every call, and §7.1 turns that into
// a refusal — but only for as long as this implementation does NOT know the
// bit. Someone "fixing" the resulting INTERNAL by adding 64 to flagKnown would
// silently disarm the one signal an old peer has, so this pins it.
func TestGenerationBitStaysUnknown(t *testing.T) {
	const gen = 64
	if flagKnown&gen != 0 {
		t.Fatalf("bit 64 is in flagKnown; §10.6 reserves it precisely so that it is not")
	}
	for _, tc := range []struct {
		name  string
		flags uint32
		want  bool
	}{
		{"bare generation bit", gen, true},
		{"on an OPEN, as a client sets it", FlagOpen | gen, true},
		{"on a creation-ack H, as a server sets it", gen, true},
		{"on a unary T, as a server sets it", FlagClose | gen, true},
		{"the successor bit", 128, true},
		{"everything this text defines", flagKnown, false},
		{"a compressed data frame", FlagCompressed, false},
	} {
		f := &Frame{}
		f.SetFlags(tc.flags)
		if got := f.hasUnknownFlags(); got != tc.want {
			t.Errorf("%s: flags=%#x hasUnknownFlags=%v, want %v", tc.name, tc.flags, got, tc.want)
		}
	}
}
