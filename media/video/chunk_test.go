package video

import "testing"

func TestPlanChunks(t *testing.T) {
	for _, tc := range []struct {
		duration float64
		parallel int
		starts   []float64
		length   float64 // of every chunk but the last
	}{
		{31, 8, []float64{0, 4, 8, 12, 16, 20, 24, 28}, 4},
		{31, 3, []float64{0, 12, 24}, 12},
		{31, 1, []float64{0}, 0},
		{4.02, 3, []float64{0, 4}, 4},
		{3, 8, []float64{0}, 0},
		{7200, 8, []float64{0, 900, 1800, 2700, 3600, 4500, 5400, 6300}, 900},
		{7201, 8, []float64{0, 904, 1808, 2712, 3616, 4520, 5424, 6328}, 904},
	} {
		cs := planChunks(tc.duration, tc.parallel)
		if len(cs) != len(tc.starts) {
			t.Fatalf("%g s / %d: %d chunks, want %d: %+v", tc.duration, tc.parallel, len(cs), len(tc.starts), cs)
		}
		next := 0
		for i, c := range cs {
			if c.start != tc.starts[i] {
				t.Fatalf("%g s / %d: chunk %d starts %g, want %g", tc.duration, tc.parallel, i, c.start, tc.starts[i])
			}
			last := i == len(cs)-1
			if want := tc.length; last && c.length != 0 || !last && c.length != want {
				t.Fatalf("%g s / %d: chunk %d length %g", tc.duration, tc.parallel, i, c.length)
			}
			if int(c.start)%segmentSeconds != 0 {
				t.Fatalf("chunk %d starts off a segment boundary: %g", i, c.start)
			}
			// Tiles partition 0..99, each at or after its chunk's start.
			if c.first != next || c.tiles < 0 || float64(c.first)*tc.duration/100 < c.start-1e-6 {
				t.Fatalf("%g s / %d: chunk %d tiles %d+%d after %d", tc.duration, tc.parallel, i, c.first, c.tiles, next)
			}
			next += c.tiles
		}
		if next != spriteCols*spriteRows {
			t.Fatalf("%g s / %d: %d sprite tiles", tc.duration, tc.parallel, next)
		}
	}
}
