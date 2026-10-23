package access

import (
	"encoding/json"
	"testing"
	"time"
)

func TestAudienceReleaseBoundaryPreservesEarlierTiers(t *testing.T) {
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	gate, err := NewGate(GateConfig{Tenant: tenant, Kinds: tiered})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := gate.planOne(Item{Content: id1, Rule: Rule{Level: TierLevel, Tiers: []string{"gold"}, Releases: []Release{{At: at, Level: TierLevel, Tiers: []string{"premium"}}, {At: at.Add(time.Hour), Level: Public}}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, instant := range []time.Time{at.Add(-time.Nanosecond), at, at.Add(time.Nanosecond), at.Add(time.Hour)} {
		for _, tier := range []string{"gold", "premium"} {
			decision, _ := verdict(plan, &knowledge{claimed: map[string]bool{tier: true}}, instant, false)
			want := tier == "gold" || !instant.Before(at)
			if decision.Allowed != want {
				t.Errorf("%s tier=%s allowed=%t want%t", instant, tier, decision.Allowed, want)
			}
		}
	}
}

func TestAudienceReleaseRejectsLegacyField(t *testing.T) {
	var r Rule
	if err := json.Unmarshal([]byte(`{"level":"ppv","free_after":"2027-01-01T00:00:00Z"}`), &r); err == nil {
		t.Fatal("legacy field accepted")
	}
	for _, release := range []Release{{Level: Public}, {At: time.Now(), Level: PPV}, {At: time.Now(), Level: TierLevel, Tiers: []string{"missing"}}} {
		if err := (Rule{Level: PPV, Releases: []Release{release}}).Validate(tiered); err == nil {
			t.Fatalf("invalid release accepted: %+v", release)
		}
	}
}
