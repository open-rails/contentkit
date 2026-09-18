package search

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/open-rails/contentkit/contentref"
)

// RRF (Reciprocal Rank Fusion) combines ranked lists without relying on raw
// score calibration: score(doc) = Σ weight_i / (k + rank_i), rank 1-based.
type RRFOptions struct {
	// K is the stabilizer constant; higher K flattens rank differences.
	// Defaults to 60 when <= 0.
	K int
	// Weights applied to each list. Empty => all 1.0.
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

type RRFContribution struct {
	ListIndex    int     `json:"list_index"` // zero-based source-list index
	Rank         int     `json:"rank"`       // one-based rank within the source list
	Weight       float32 `json:"weight"`
	Contribution float32 `json:"contribution"`
}

type RRFTraceHit struct {
	Hit           RRFHit            `json:"-"`
	Key           RRFTraceKey       `json:"key"`
	Score         float32           `json:"score"`
	Contributions []RRFContribution `json:"contributions"`
}

type RRFTraceKey struct {
	TenantID         string `json:"tenant_id"`
	ContentKind      string `json:"content_kind"`
	ContentID        string `json:"content_id"`
	ContentVersionID string `json:"content_version_id,omitempty"`
	Language         string `json:"language"`
}

func (k RRFKey) keyString() string {
	return strings.Join([]string{k.TenantID, k.ContentKind, k.ContentID, k.ContentVersionID, k.Language}, "\x1f")
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
	out, _ := fuseRRF(lists, opts, false)
	return out
}

// FuseRRFWithTrace returns the same ordered hits as FuseRRF plus the exact
// source-list contributions used to compute each score.
func FuseRRFWithTrace(lists [][]RRFKey, opts RRFOptions) ([]RRFTraceHit, error) {
	for i, weight := range opts.Weights {
		if !finiteFloat32(weight) {
			return nil, fmt.Errorf("RRF weight %d must be finite", i)
		}
	}
	hits, contributions := fuseRRF(lists, opts, true)
	out := make([]RRFTraceHit, 0, len(hits))
	for _, hit := range hits {
		if !finiteFloat32(hit.Score) {
			return nil, fmt.Errorf("RRF score overflow for %s", hit.ContentID)
		}
		out = append(out, RRFTraceHit{
			Hit:           hit,
			Key:           RRFTraceKey{TenantID: hit.TenantID, ContentKind: hit.ContentKind, ContentID: hit.ContentID, ContentVersionID: hit.ContentVersionID, Language: hit.Language},
			Score:         hit.Score,
			Contributions: contributions[hit.RRFKey.keyString()],
		})
	}
	return out, nil
}

func fuseRRF(lists [][]RRFKey, opts RRFOptions, includeTrace bool) ([]RRFHit, map[string][]RRFContribution) {
	k := opts.K
	if k <= 0 {
		k = 60
	}
	scores := make(map[string]float32)
	example := make(map[string]RRFKey)
	var contributions map[string][]RRFContribution
	if includeTrace {
		contributions = make(map[string][]RRFContribution)
	}
	for li, list := range lists {
		w := float32(1.0)
		if li < len(opts.Weights) && opts.Weights[li] > 0 {
			w = opts.Weights[li]
		}
		for i, item := range list {
			rank := i + 1
			ks := item.keyString()
			example[ks] = item
			contribution := w / (float32(k) + float32(rank))
			scores[ks] += contribution
			if includeTrace {
				contributions[ks] = append(contributions[ks], RRFContribution{ListIndex: li, Rank: rank, Weight: w, Contribution: contribution})
			}
		}
	}
	out := make([]RRFHit, 0, len(scores))
	for ks, sc := range scores {
		out = append(out, RRFHit{RRFKey: example[ks], Score: sc})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].RRFKey.less(out[j].RRFKey)
	})
	return out, contributions
}

func finiteFloat32(value float32) bool {
	return !math.IsNaN(float64(value)) && !math.IsInf(float64(value), 0)
}
