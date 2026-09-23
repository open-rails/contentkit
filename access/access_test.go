package access

import "testing"

func TestResolutionUnits(t *testing.T) {
	for _, tc := range []struct {
		name  string
		r     Resolution
		total int
		units int
		full  bool
	}{
		{"full", Resolution{Visible: true, Accessible: true}, 10, 10, true},
		{"hidden", Resolution{Accessible: true}, 10, 0, false},
		{"hidden preview", Resolution{PreviewLimit: 3}, 10, 0, false},
		{"denied", Resolution{Visible: true}, 10, 0, false},
		{"free preview", Resolution{Visible: true, PreviewLimit: 3}, 10, 3, false},
		{"scheduled", Resolution{Visible: true, Accessible: true, PreviewLimit: 7}, 10, 7, false},
		{"cap above total", Resolution{Visible: true, PreviewLimit: 30}, 10, 10, false},
		{"empty", Resolution{Visible: true, Accessible: true}, 0, 0, true},
	} {
		if got := tc.r.Units(tc.total); got != tc.units {
			t.Errorf("%s: Units(%d) = %d, want %d", tc.name, tc.total, got, tc.units)
		}
		if got := tc.r.Full(); got != tc.full {
			t.Errorf("%s: Full() = %v, want %v", tc.name, got, tc.full)
		}
	}
}
