package access

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/open-rails/contentkit/kinds"
)

var tiered = kinds.Config{Tiers: []string{"premium", "gold"}}

func TestRuleJSON(t *testing.T) {
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		rule Rule
		json string
	}{
		{Rule{Level: Public}, `{"level":"public"}`},
		{Rule{Level: TierLevel, Tiers: []string{"premium"}, Preview: &Preview{Pages: 3}}, `{"level":"tier","tiers":["premium"],"preview":{"pages":3}}`},
		{Rule{Level: EntityLevel, Entity: id1}, `{"level":"group","group_id":"` + id1 + `"}`},
		{Rule{Level: PPV, Releases: []Release{{At: at, Level: Public}}}, `{"level":"ppv","releases":[{"at":"2026-11-01T00:00:00Z","level":"public"}]}`},
		{Rule{Level: EntityPPV, Entity: id1, Preview: &Preview{Segments: 1}}, `{"level":"group_ppv","group_id":"` + id1 + `","preview":{"segments":1}}`},
	} {
		b, err := json.Marshal(tc.rule)
		if err != nil || string(b) != tc.json {
			t.Fatalf("marshal %+v = %s %v", tc.rule, b, err)
		}
		var back Rule
		if err := json.Unmarshal(b, &back); err != nil || back.Validate(tiered) != nil {
			t.Fatalf("unmarshal %s: %+v %v", b, back, err)
		}
		if b2, _ := json.Marshal(back); string(b2) != tc.json {
			t.Fatalf("round trip %s -> %s", tc.json, b2)
		}
	}
	var r Rule
	if err := json.Unmarshal([]byte(`{"level":"ppv","scope":"x"}`), &r); !errors.Is(err, ErrRule) {
		t.Fatalf("unknown field: %v", err)
	}
}

func TestRuleValidate(t *testing.T) {
	future := time.Now().Add(time.Hour)
	for _, bad := range []Rule{
		{}, {Level: "vip"}, {Level: Public, Tiers: []string{"premium"}}, {Level: PPV, Entity: id1},
		{Level: TierLevel}, {Level: TierLevel, Tiers: []string{"platinum"}}, {Level: TierLevel, Tiers: []string{"gold", "gold"}},
		{Level: TierLevel, Tiers: []string{"gold"}, Entity: id1}, {Level: EntityLevel}, {Level: EntityPPV, Entity: "x"},
		{Level: EntityLevel, Entity: id1, Tiers: []string{"gold"}}, {Level: Public, Preview: &Preview{Pages: 1}},
		{Level: Public, Releases: []Release{{At: future, Level: Public}}}, {Level: PPV, Preview: &Preview{}}, {Level: PPV, Preview: &Preview{Pages: -1}},
	} {
		if err := bad.Validate(tiered); !errors.Is(err, ErrRule) {
			t.Errorf("%+v: %v", bad, err)
		}
	}
}

func TestRuleFreeEffectiveFacet(t *testing.T) {
	now := time.Now()
	past, future := now.Add(-time.Minute), now.Add(time.Minute)
	if !(Rule{Level: Public}).Free(now) || !(Rule{Level: PPV, Releases: []Release{{At: past, Level: Public}}}).Free(now) || (Rule{Level: PPV, Releases: []Release{{At: future, Level: Public}}}).Free(now) {
		t.Fatal("Free")
	}
	content, override := Rule{Level: TierLevel, Tiers: []string{"premium"}}, Rule{Level: Public}
	if Effective(content, nil).Level != TierLevel || Effective(content, &override).Level != Public {
		t.Fatal("Effective")
	}
	f := Rule{Level: EntityLevel, Entity: id1, Releases: []Release{{At: future, Level: Public}}}.Facet()
	if f.Level != EntityLevel || f.EntityID == nil || *f.EntityID != id1 || f.Tiers == nil || len(f.Tiers) != 0 || len(f.Releases) != 1 || !f.Releases[0].At.Equal(future) {
		t.Fatalf("Facet %+v", f)
	}
}
