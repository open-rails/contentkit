package search

import (
	"math"
	"sort"

	"github.com/open-rails/contentkit/contentref"
)

// RRF (Reciprocal Rank Fusion) combines ranked lists without relying on raw
// score calibration: score(doc) = Σ weight_i / (k + rank_i), rank 1-based.
type RRFOptions struct {
	// K is the stabilizer constant; higher K flattens rank differences.
	// Defaults to 60 when <= 0.
	K int
	// Finite weights apply to each list; missing weights default to 1.
	// Zero excludes a list. Negative weights subtract its contributions.
	Weights []float32
}

// RRFKey identifies one document across fused lists.
type RRFKey struct {
	contentref.ContentKey
	Language string
}

type RRFHit struct {
	RRFKey
	Score float32
}

func (k RRFKey) less(o RRFKey) bool {
	if k.ContentKind != o.ContentKind {
		return k.ContentKind < o.ContentKind
	}
	if k.ContentID != o.ContentID {
		return k.ContentID < o.ContentID
	}
	if k.ContentVersionID != o.ContentVersionID {
		return k.ContentVersionID < o.ContentVersionID
	}
	if k.Language != o.Language {
		return k.Language < o.Language
	}
	return k.TenantID < o.TenantID
}

// FuseRRF fuses multiple ranked lists (best-first) into one ranked list.
func FuseRRF(lists [][]RRFKey, opts RRFOptions) []RRFHit {
	k := opts.K
	if k <= 0 {
		k = 60
	}
	scores := make(map[RRFKey]float32)
	for li, list := range lists {
		w := float32(1.0)
		if li < len(opts.Weights) && (opts.Weights[li] > 0 || finiteFloat32(opts.Weights[li])) {
			w = opts.Weights[li]
		}
		if w == 0 {
			continue
		}
		for i, item := range list {
			scores[item] += w / (float32(k) + float32(i+1))
		}
	}
	out := make([]RRFHit, 0, len(scores))
	for ks, sc := range scores {
		out = append(out, RRFHit{RRFKey: ks, Score: sc})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].RRFKey.less(out[j].RRFKey)
	})
	return out
}

func finiteFloat32(value float32) bool {
	return !math.IsNaN(float64(value)) && !math.IsInf(float64(value), 0)
}
