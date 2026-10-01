package access

import "testing"

func TestResolutionFull(t *testing.T) {
	for _, tc := range []struct {
		name string
		r    Resolution
		full bool
	}{
		{"full", Resolution{Visible: true, Accessible: true}, true},
		{"hidden", Resolution{Accessible: true}, false},
		{"denied", Resolution{Visible: true}, false},
		{"editor only", Resolution{Visible: true, Editor: true}, false},
	} {
		if got := tc.r.Full(); got != tc.full {
			t.Errorf("%s: Full() = %v, want %v", tc.name, got, tc.full)
		}
	}
}
