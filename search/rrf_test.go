package search

import (
	"math"
	"testing"
)

func TestFuseRRFExplicitWeights(t *testing.T) {
	t.Parallel()
	a, b, c := key(cid(1)), key(cid(2)), key(cid(3))
	lists := [][]RRFKey{{a, b}, {b, c}}
	for _, tc := range []struct {
		name    string
		weights []float32
		keys    []RRFKey
		scores  []float32
	}{
		{"zero excludes source", []float32{1, 0}, []RRFKey{a, b}, []float32{1.0 / 2, 1.0 / 3}},
		{"missing weight defaults", []float32{0}, []RRFKey{b, c}, []float32{1.0 / 2, 1.0 / 3}},
		{"all sources disabled", []float32{0, 0}, nil, nil},
		{"negative subtracts contribution", []float32{1, -2}, []RRFKey{a, b, c}, []float32{1.0 / 2, float32(1.0/3) - 1, -2.0 / 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := RRFOptions{K: 1, Weights: tc.weights}
			hits := FuseRRF(lists, opts)
			traced, err := FuseRRFWithTrace(lists, opts)
			if err != nil || len(hits) != len(tc.keys) || len(traced) != len(hits) {
				t.Fatalf("hits=%+v, traced=%+v, error=%v", hits, traced, err)
			}
			for i, hit := range hits {
				if hit.RRFKey != tc.keys[i] || hit.Score != tc.scores[i] || traced[i].Hit != hit {
					t.Fatalf("hit %d = %+v, trace=%+v; want key=%+v score=%v", i, hit, traced[i], tc.keys[i], tc.scores[i])
				}
				var sum float32
				for _, contribution := range traced[i].Contributions {
					weight := float32(1)
					if contribution.ListIndex < len(tc.weights) {
						weight = tc.weights[contribution.ListIndex]
					}
					if weight == 0 || contribution.Weight != weight {
						t.Fatalf("disabled or altered contribution: %+v", contribution)
					}
					sum += contribution.Contribution
				}
				if sum != hit.Score {
					t.Fatalf("contributions sum=%v, score=%v", sum, hit.Score)
				}
			}
		})
	}
}

func TestFuseRRFNonfiniteCompatibility(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		weight float32
		score  float32
	}{
		{"nan", float32(math.NaN()), 0.5},
		{"negative infinity", float32(math.Inf(-1)), 0.5},
		{"positive infinity", float32(math.Inf(1)), float32(math.Inf(1))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hits := FuseRRF([][]RRFKey{{key(cid(1))}}, RRFOptions{K: 1, Weights: []float32{tc.weight}})
			if len(hits) != 1 || hits[0].Score != tc.score {
				t.Fatalf("nonfinite compatibility: hits=%+v, want score=%v", hits, tc.score)
			}
		})
	}
}
