package discovery

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/engagementtest"
	"github.com/open-rails/contentkit/internal/signaltest"
	"github.com/open-rails/contentkit/signal"
)

func seedKnownCards(f *fixture) {
	f.group("tag", "big-breasts", "yaoi")
	en := []string{"en"}
	premium := `{"level":"tier","tiers":["premium"]}`
	f.seed(
		item{n: 1, titles: map[string]string{"en": "Known unlisted"}, age: time.Hour, listing: "unlisted", versions: []ver{{langs: en}}},
		item{n: 2, titles: map[string]string{"en": "Known filtered"}, age: time.Hour, listing: "unlisted", groups: []string{"big-breasts"}, versions: []ver{{langs: en}}},
		item{n: 3, titles: map[string]string{"en": "Known domain restricted"}, age: time.Hour, listing: "unlisted", groups: []string{"restricted"}, versions: []ver{{langs: en}}},
		item{n: 4, titles: map[string]string{"en": "Known premium"}, age: time.Hour, listing: "unlisted", rule: premium, versions: []ver{{langs: en}}},
		item{n: 5, titles: map[string]string{"en": "Future"}, age: time.Hour, listing: "unlisted", versions: []ver{{langs: en, future: true}}},
		item{n: 6, titles: map[string]string{"en": "Deleted"}, age: time.Hour, listing: "unlisted", deleted: true, versions: []ver{{langs: en}}},
		item{n: 7, titles: map[string]string{"en": "Members only"}, age: time.Hour, listing: "members", rule: premium, versions: []ver{{langs: en}}},
	)
}

func TestKnownCardsRetainUnlistedWithoutWeakeningDiscoveryOrReaderPolicy(t *testing.T) {
	f := newFixture(t, nil, "")
	seedKnownCards(f)
	selected := []string{f.cid(4), f.cid(2), f.cid(3), f.cid(1), f.cid(5), f.cid(6), f.cid(7), f.cid(99), f.cid(1)}
	path := q("/content-cards", "ids", strings.Join(selected, ","), "kind", "gallery", "summary", "true")
	ids, out := f.list("clean.test", "", path)
	if want := []string{f.cid(4), f.cid(1)}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("known IDs in caller order = %v, want %v", ids, want)
	}
	if out.Contents[0].Access.Allowed || out.Contents[0].Access.Level != access.TierLevel {
		t.Fatalf("premium known card must remain locked: %+v", out.Contents[0])
	}
	if out.Summary == nil || out.Summary.Total != 2 || out.Summary.FilteredCount != 1 || out.Summary.LockedCount != 1 {
		t.Fatalf("bounded summary = %+v", out.Summary)
	}
	if got, _ := f.list("clean.test", "", path+"&mode=hide"); !reflect.DeepEqual(got, []string{f.cid(1)}) {
		t.Fatalf("hide mode = %v", got)
	}
	if got, _ := f.list("clean.test", "fan", path); !reflect.DeepEqual(got, []string{f.cid(4), f.cid(1), f.cid(7)}) {
		t.Fatalf("member access = %v", got)
	}
	// A required domain entity and requested language remain hard constraints.
	if got, _ := f.list("gay.test", "", path); len(got) != 0 {
		t.Fatalf("domain requirements bypassed: %v", got)
	}
	if got, _ := f.list("clean.test", "", path+"&language=ja"); len(got) != 0 {
		t.Fatalf("language bypassed: %v", got)
	}
	if got, _ := f.list("clean.test", "", q("/contents", "ids", f.cid(1))); len(got) != 0 {
		t.Fatalf("global ID listing changed: %v", got)
	}
	if got, _ := f.list("clean.test", "", "/search?q=Known"); len(got) != 0 {
		t.Fatalf("unlisted content became searchable: %v", got)
	}
	for _, invalid := range []string{path + "&mode=invalid", path + "&language=xx", path + "&summary=bad"} {
		if status, _ := f.call(http.MethodGet, "clean.test", "", invalid, nil, nil); status != http.StatusBadRequest {
			t.Fatalf("invalid reader request returned %d", status)
		}
	}
	for _, bad := range []string{"", "invalid", strings.Repeat(f.cid(1)+",", 100) + f.cid(1)} {
		if status, _ := f.call(http.MethodGet, "clean.test", "", q("/content-cards", "ids", bad), nil, nil); status != http.StatusBadRequest {
			t.Fatalf("invalid known-ID request %q returned %d", bad, status)
		}
	}
}

func TestPersonalHistoryRetainsUnlistedUnderCurrentReaderPolicy(t *testing.T) {
	env := signaltest.FromEnv(t)
	database := "contentkit_known_history_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	conn := env.Fresh(t, database)
	t.Cleanup(func() { env.Drop(t, conn, database) })
	f := newFixture(t, conn, database)
	seedKnownCards(f)
	at := time.Now().UTC().Add(-time.Hour)
	var observed []engagementtest.Observation
	for n := 1; n <= 7; n++ {
		observedAt := at.Add(time.Duration(n) * time.Second)
		observed = append(observed, engagementtest.Observation{ContentRef: contentref.NewVersion(tenant, "gallery", f.cid(n), f.vid(n, 0)),
			Subject: signal.Subject{UserID: "reader"}, Type: engagementtest.TypeReading, EventID: uuid.NewString(), OccurredAt: observedAt,
			Progress: 1, ProgressMax: 2, DurationS: 20, Access: readingAccess(observedAt, signal.ActivityScope{Domain: "main", Language: "en"})})
	}
	if err := f.svc.o.Hub.RecordSignals(f.ctx, engagementtest.Native(observed)); err != nil {
		t.Fatal(err)
	}
	ids, out := f.list("clean.test", "reader", "/me/history?kind=gallery&summary=true")
	if want := []string{f.cid(4), f.cid(1)}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("personal history = %v, want %v", ids, want)
	}
	if out.Summary == nil || out.Summary.Total != 2 || out.Summary.FilteredCount != 1 || out.Summary.LockedCount != 1 {
		t.Fatalf("personal summary = %+v", out.Summary)
	}
	if got, _ := f.list("clean.test", "other", "/me/history?kind=gallery&summary=true"); len(got) != 0 {
		t.Fatalf("another account sees history: %v", got)
	}
	if got, _ := f.list("clean.test", "reader", "/me/history?kind=gallery&mode=hide"); !reflect.DeepEqual(got, []string{f.cid(1)}) {
		t.Fatalf("hidden premium history = %v", got)
	}
}
