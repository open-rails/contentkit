package discovery

import (
	"fmt"
	"time"

	"github.com/open-rails/contentkit/signal"
)

// seedPopulation is explicit independent site activity, including visitors who
// never consume a work. Callers choose the cohort; it is never derived from
// qualified readers, exposure counters or whichever ranking happens to return.
func seedPopulation(f *fixture, kind string, scope signal.ActivityScope, at time.Time, subjects []signal.Subject, eligibleTiers []string) {
	f.t.Helper()
	rows := make([]signal.PopulationObservation, len(subjects))
	for i, subject := range subjects {
		rows[i] = signal.PopulationObservation{ID: fmt.Sprintf("population-%s-%s-%s-%d-%d", kind, scope.Domain, scope.Language, at.Unix(), i),
			ContentKind: kind, Subject: subject, ObservedAt: at, Domain: scope.Domain, Language: scope.Language,
			EligibleTiers: eligibleTiers, EligibilityKnown: true}
	}
	for start := 0; start < len(rows); start += signal.MaxSignalsPerBatch {
		if err := f.svc.o.Hub.Signals().RecordPopulation(f.ctx, tenant, rows[start:min(start+signal.MaxSignalsPerBatch, len(rows))]); err != nil {
			f.t.Fatal(err)
		}
	}
}

func readingAccess(at time.Time, scope signal.ActivityScope) *signal.AccessContext {
	return &signal.AccessContext{ObservedAt: at, ContentAccess: "public", ViewerAccess: "full", Audience: "public",
		EligibilityKnown: true, Domain: scope.Domain, Language: scope.Language}
}
