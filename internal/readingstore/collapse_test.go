package readingstore

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestCollapse(t *testing.T) {
	who, item, other := uuid.New(), uuid.New(), uuid.New()
	day := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	p := func(v uint32) *uint32 { return &v }
	got := collapse([]StateUpdate{
		{Subject: who, ContentID: item, Day: day.AddDate(0, 0, 1), LastSeen: day.Add(30 * time.Hour), Outcome: OutcomeNeutral, Furthest: 9, Position: p(9)},
		{Subject: who, ContentID: other, Day: day, LastSeen: day.Add(time.Hour), Outcome: OutcomeBounce},
		{Subject: who, ContentID: item, Day: day, LastSeen: day.Add(2 * time.Hour), Outcome: OutcomeComplete, Furthest: 4, Position: p(4)},
	})
	if len(got) != 2 {
		t.Fatalf("collapse kept %d updates", len(got))
	}
	a := got[0]
	if !a.Day.Equal(day) || !a.LastSeen.Equal(day.Add(30*time.Hour)) || a.Outcome != OutcomeComplete || a.Furthest != 9 || a.Position == nil || *a.Position != 9 {
		t.Fatalf("collapsed = %+v (position %v)", a, *a.Position)
	}
}
