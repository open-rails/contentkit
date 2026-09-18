package popularity

import (
	"context"
	"math"

	"github.com/open-rails/contentkit/signal"
)

// CompletionRatio is the covered share of the selected version at which a
// session counts as completed.
const CompletionRatio = 0.9

// SessionScorer is the v1 session engagement score, a signal.Scorer for one
// content kind: coverage of the selected version times dwell.
//
//	coverage = min(1, covered / total)
//	dwell    = min(1, active_s / (covered × SecondsPerUnit))
//	score    = round(100 × coverage × (0.6 + 0.4 × dwell))      ∈ [0, 100]
//
// covered is the distinct units exposed (pages, seconds) and total the
// selected version's units (ProgressMax), so size never enters: a 6-page
// gallery read fully scores the same as a 200-page one read fully. Completed
// at CompletionRatio of the units. Non-view signals pass through unchanged.
type SessionScorer struct {
	// SecondsPerUnit is the active time per covered unit at which a session
	// counts as consumed rather than flipped through (8 for gallery pages).
	// Zero means the units are seconds themselves: dwell is 1.
	SecondsPerUnit float64
	// CoveredKey names the Payload entry holding the covered units when the
	// host keeps Progress as its resume anchor; empty reads Progress.
	CoveredKey string
}

// Score implements signal.Scorer.
func (s SessionScorer) Score(_ context.Context, sig signal.Signal) (signal.Scored, error) {
	out := signal.Scored{Score: sig.Score, Progress: sig.Progress, ProgressMax: sig.ProgressMax, Completed: sig.Completed}
	if sig.Type != signal.TypeView {
		return out, nil
	}
	covered := sig.Progress
	if s.CoveredKey != "" {
		if v, ok := payloadUint32(sig.Payload[s.CoveredKey]); ok {
			covered = v
		}
	}
	out.Score, out.Completed = SessionScore(covered, sig.ProgressMax, sig.DurationS, s.SecondsPerUnit)
	return out, nil
}

// SessionScore is the pure form of SessionScorer for hosts that score outside
// a registered Scorer.
func SessionScore(covered, total, activeS uint32, secondsPerUnit float64) (score int16, completed bool) {
	var coverage, dwell float64
	if total > 0 && covered > 0 {
		coverage = math.Min(1, float64(covered)/float64(total))
		dwell = 1
		if secondsPerUnit > 0 {
			dwell = math.Min(1, float64(activeS)/(float64(covered)*secondsPerUnit))
		}
	}
	score = int16(math.Round(100 * coverage * (0.6 + 0.4*dwell)))
	completed = total > 0 && float64(covered) >= CompletionRatio*float64(total)
	return score, completed
}

func payloadUint32(v any) (uint32, bool) {
	switch t := v.(type) {
	case uint32:
		return t, true
	case uint64:
		return uint32(t), true
	case int:
		if t < 0 {
			return 0, false
		}
		return uint32(t), true
	case int64:
		if t < 0 {
			return 0, false
		}
		return uint32(t), true
	case float64:
		if t < 0 || math.IsNaN(t) {
			return 0, false
		}
		return uint32(t), true
	}
	return 0, false
}
