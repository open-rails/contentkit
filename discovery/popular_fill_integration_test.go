package discovery

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/engagementtest"
	"github.com/open-rails/contentkit/internal/signaltest"
	"github.com/open-rails/contentkit/signal"
)

// Recent measured coverage or sparse reading must not empty a popular window
// that holds explicit standing: measured works rank first, standing follows.
func TestPopularFillsStandingAfterMeasuredReaders(t *testing.T) {
	env := signaltest.FromEnv(t)
	const database = "contentkit_popular_fill_test"
	f := newFixture(t, env.Fresh(t, database), database)
	for n, title := range map[int]string{1: "Read", 2: "Liked", 3: "Liked before coverage", 4: "Liked outside the window"} {
		f.seed(item{n: n, titles: map[string]string{"en": title}, age: 100 * 24 * time.Hour, versions: []ver{{langs: []string{"en"}, pages: 10}}})
	}
	f.exec(`UPDATE {s}.content_access_history_start SET started_at=(SELECT min(occurred_at) FROM {s}.content_access_history WHERE tenant_id=$1) WHERE tenant_id=$1`, tenant)
	day := time.Date(f.now.Year(), f.now.Month(), f.now.Day(), 0, 0, 0, 0, time.UTC)
	covered := day.AddDate(0, 0, -5) // measured instrumentation starts here
	from, until := day.AddDate(0, 0, -8), day.AddDate(0, 0, -1)
	scope := signal.ActivityScope{Domain: "main", Language: "en"}
	var population []signal.Subject
	for i := range 40 {
		population = append(population, signal.Subject{UserID: fmt.Sprintf("cohort-%d", i)})
	}
	seedPopulation(f, "gallery", scope, covered, population, nil)
	var observations []engagementtest.Observation
	for i := range 4 {
		observations = append(observations, engagementtest.Observation{ContentRef: contentref.NewVersion(tenant, "gallery", f.cid(1), f.vid(1, 0)),
			Subject: population[i], Type: engagementtest.TypeReading, EventID: fmt.Sprintf("read-%d", i), OccurredAt: covered.Add(time.Hour),
			Progress: 10, ProgressMax: 10, DurationS: 100, Access: readingAccess(covered.Add(time.Hour), scope)})
	}
	if err := engagementtest.Record(f.ctx, f.svc.o.Hub.Signals(), tenant, observations); err != nil {
		t.Fatal(err)
	}
	// Standing: two fans in the window; one fan before measured coverage began
	// but inside the window; one vote outside it; the read work's own like.
	f.react(2, "fan-a", 1, covered.Add(2*time.Hour))
	f.react(2, "fan-b", 1, covered.Add(2*time.Hour))
	f.react(3, "early", 1, from.Add(time.Hour))
	f.react(4, "late", 1, until.Add(time.Hour))
	f.react(1, "reader-fan", 1, covered.Add(2*time.Hour))

	path := q("/contents", "kind", "gallery", "sort", "popular", "activity_from", from.Format(time.DateOnly), "activity_until", until.Format(time.DateOnly))
	got, page := f.list("main.test", "", path)
	// Measured first; then Wilson 2:0 before 1:0; nothing outside the window; no duplicate.
	if want := []string{f.cid(1), f.cid(2), f.cid(3)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("popular window = %v, want %v", got, want)
	}
	if r := page.Ranking; r == nil || r.Policy != "v2" || r.Mode != "measured_then_standing" || r.CoveredFrom == "" {
		t.Fatalf("ranking provenance: %+v", r)
	}
	if page.Contents[0].Ranking == nil || page.Contents[1].Ranking != nil || page.Contents[2].Ranking != nil {
		t.Fatalf("only measured works report participation: %+v %+v %+v", page.Contents[0].Ranking, page.Contents[1].Ranking, page.Contents[2].Ranking)
	}
	// Paging stays one ordering across the measured/standing seam.
	first, p1 := f.list("main.test", "", q("/contents", "kind", "gallery", "sort", "popular", "activity_from", from.Format(time.DateOnly), "activity_until", until.Format(time.DateOnly), "limit", "1"))
	second, _ := f.list("main.test", "", q("/contents", "kind", "gallery", "sort", "popular", "activity_from", from.Format(time.DateOnly), "activity_until", until.Format(time.DateOnly), "limit", "2", "cursor", p1.NextCursor))
	if !reflect.DeepEqual(append(first, second...), []string{f.cid(1), f.cid(2), f.cid(3)}) {
		t.Fatalf("paged popular window = %v + %v", first, second)
	}
}
