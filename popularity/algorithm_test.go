package popularity

import (
	"math"
	"testing"

	"github.com/open-rails/contentkit/signal"
)

func TestPolicyAlgorithmIndependentOfName(t *testing.T) {
	n := &signal.NormalizationMetrics{Rate: .1, Engagement: .05}
	p := PolicyV2
	p.Name = "future-tuned-policy"
	if err := p.Validate(); err != nil || !p.Normalized() || math.Abs(p.Score(signal.ContentMetrics{Normalization: n})-.1175) > 1e-12 {
		t.Fatalf("renamed normalized policy lost algorithm: %+v %v", p, err)
	}
	p.Algorithm = "unsupported"
	if err := p.Validate(); err == nil {
		t.Fatal("unknown algorithm accepted")
	}
	if PolicyV1.Normalized() {
		t.Fatal("legacy default changed")
	}
}
