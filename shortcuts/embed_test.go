package shortcutasset

import "testing"

func TestHelperIsEmbedded(t *testing.T) {
	t.Parallel()

	first := Helper()
	if len(first) < 1_000 {
		t.Fatalf("embedded helper is unexpectedly small: %d bytes", len(first))
	}
	first[0] ^= 0xff
	second := Helper()
	if first[0] == second[0] {
		t.Fatal("Helper returned shared mutable storage")
	}
}
