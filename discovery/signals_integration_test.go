package discovery

import (
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/engagementtest"
	"github.com/open-rails/contentkit/internal/signaltest"
	"github.com/open-rails/contentkit/signal"
)

const signalCHDB = "contentkit_discovery_signal_test"

// TestSignalPlaneRoutes drives explicit clicks and measured readership's
// history, view_status, statistics and recommendations over real ClickHouse.
func TestSignalPlaneRoutes(t *testing.T) {
	env := signaltest.FromEnv(t)
	f := seededOn(t, env.Fresh(t, signalCHDB), signalCHDB)
	const host = "main.test"
	at := time.Now().UTC().Add(-time.Hour)
	click := func(content, version, event string) SignalInput {
		return SignalInput{ContentID: content, VersionID: version, Type: TypeClick, EventID: event, OccurredAt: at}
	}
	post := func(user string, in Signals) {
		t.Helper()
		if status, code := f.call(http.MethodPost, host, user, "/signals", in, nil); status != http.StatusNoContent {
			t.Fatalf("POST signals as %q: %d %s", user, status, code)
		}
	}
	post("u1", Signals{Signals: []SignalInput{click(f.cid(1), f.vid(1, 0), "u1-g1")}})
	observed := []engagementtest.Observation{{ContentRef: contentref.NewVersion(tenant, "gallery", f.cid(1), f.vid(1, 0)),
		Subject: signal.Subject{UserID: "u1"}, Type: engagementtest.TypeReading, EventID: "read-u1-g1", OccurredAt: at, Progress: 20, ProgressMax: 20, DurationS: 400}}
	for i := range 3 {
		session := fmt.Sprintf("s%d", i)
		post("", Signals{Session: session, Signals: []SignalInput{click(f.cid(1), "", "a-g1"), click(f.cid(3), "", "a-g3")}})
		subject, err := f.svc.subject(access.Actor{Anonymous: true}, session)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range []int{1, 3} {
			observed = append(observed, engagementtest.Observation{ContentRef: contentref.NewVersion(tenant, "gallery", f.cid(n), f.vid(n, 0)), Subject: subject,
				Type: engagementtest.TypeReading, EventID: fmt.Sprintf("read-%s-g%d", session, n), OccurredAt: at, Progress: 5, ProgressMax: 20, DurationS: 100})
		}
	}
	if err := f.svc.o.Hub.RecordSignals(f.ctx, engagementtest.Native(observed)); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []struct {
		user string
		in   Signals
		want int
	}{
		{"", Signals{Signals: []SignalInput{click(f.cid(1), "", "x")}}, http.StatusBadRequest},
		{"u1", Signals{Signals: []SignalInput{click(f.cid(9), "", "x")}}, http.StatusNotFound},
		{"u1", Signals{Signals: []SignalInput{click(f.cid(1), f.vid(3, 0), "x")}}, http.StatusNotFound},
		{"u1", Signals{Signals: []SignalInput{{ContentID: f.cid(1), Type: "rating", EventID: "x", OccurredAt: at}}}, http.StatusBadRequest},
		{"u1", Signals{Signals: []SignalInput{{ContentID: f.cid(1), Type: "view", EventID: "retired", OccurredAt: at}}}, http.StatusBadRequest},
		{"u1", Signals{Signals: []SignalInput{{ContentID: f.cid(1), Type: TypeClick, OccurredAt: at}}}, http.StatusBadRequest},
	} {
		if status, _ := f.call(http.MethodPost, host, bad.user, "/signals", bad.in, nil); status != bad.want {
			t.Errorf("POST %+v: %d, want %d", bad.in, status, bad.want)
		}
	}

	ids, page := f.list(host, "u1", "/me/history")
	if !reflect.DeepEqual(ids, []string{f.cid(1)}) {
		t.Fatalf("history = %v", ids)
	}
	if v := page.Contents[0].Viewer; v == nil || !v.Completed || v.ProgressMax == 0 || v.Progress != v.ProgressMax {
		t.Fatalf("history card state: %+v", v)
	}
	for status, want := range map[string][]string{Completed: {f.cid(1)}, InProgress: {}, Unseen: {f.cid(3)}} {
		if got, _ := f.list(host, "u1", q("/contents", "kind", "gallery", "view_status", status)); !reflect.DeepEqual(got, want) {
			t.Errorf("view_status %s = %v, want %v", status, got, want)
		}
	}

	if err := f.svc.RefreshPopularity(f.ctx); err != nil {
		t.Fatal(err)
	}
	if got, page := f.list(host, "", q("/contents", "kind", "gallery", "sort", "popular:all")); !reflect.DeepEqual(got, []string{f.cid(1), f.cid(3)}) {
		t.Fatalf("popular:all = %v %+v", got, page.Contents)
	}

	// One person per UTC day regardless of how many contents they read;
	// explicit clicks do not contribute active reading time or reader counts.
	var activity Activity
	if status, code := f.call(http.MethodGet, host, "staff", q("/stats/activity", "kind", "gallery"), nil, &activity); status != http.StatusOK ||
		len(activity.Days) != 1 || activity.Days[0] != (ActivityDay{Date: at.Format(time.DateOnly), ActiveMS: 1_000_000, Readers: 4, Users: 1}) {
		t.Fatalf("activity: %d %s %+v", status, code, activity)
	}
	for _, c := range []struct {
		host, path string
		want       []string
	}{
		{host, q("/stats/top", "kind", "gallery", "period", "7d"), []string{f.cid(1) + "=4", f.cid(3) + "=3"}},
		{"clean.test", q("/stats/top", "kind", "gallery", "period", "all"), []string{f.cid(1) + "=4"}},
		{host, q("/stats/top", "kind", "gallery", "tiers", "premium"), []string{}},
	} {
		var top TopContents
		status, code := f.call(http.MethodGet, c.host, "staff", c.path, nil, &top)
		got := []string{}
		for _, r := range top.Contents {
			got = append(got, fmt.Sprintf("%s=%d", r.ID, r.Value))
		}
		if status != http.StatusOK || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s on %s: %d %s %v, want %v", c.path, c.host, status, code, got, c.want)
		}
		if len(top.Contents) > 0 && (top.Contents[0].Title != "Alpha" || top.Contents[0].Path != "/en/g/"+f.codes[1]+"/alpha") {
			t.Errorf("ranked names %+v", top.Contents[0])
		}
	}
	var mine MyStats
	if status, code := f.call(http.MethodGet, host, "u1", "/me/stats", nil, &mine); status != http.StatusOK || mine.Completed["gallery"] != 1 {
		t.Fatalf("u1's stats: %d %s %+v", status, code, mine)
	}

	exposure := Exposures{Exposures: []ExposureInput{{RenderID: "r1", Stage: "rendered", Surface: "home", Shown: []Placement{{ContentID: f.cid(1), Position: 1}, {ContentID: f.cid(3), Position: 2}}}}}
	if status, code := f.call(http.MethodPost, host, "u1", "/exposures", exposure, nil); status != http.StatusNoContent {
		t.Fatalf("exposures: %d %s", status, code)
	}
	exposure.Exposures[0].Shown[1].Position = 1
	if status, _ := f.call(http.MethodPost, host, "u1", "/exposures", exposure, nil); status != http.StatusBadRequest {
		t.Fatalf("duplicate positions: %d", status)
	}

	if got, _ := f.list(host, "", "/contents/"+f.cid(1)+"/similar"); !reflect.DeepEqual(got, []string{f.cid(3)}) {
		t.Fatalf("similar = %v", got)
	}
	if got, _ := f.list("clean.test", "", "/contents/"+f.cid(1)+"/similar"); len(got) != 0 {
		t.Fatalf("similar leaks restricted content on a clean site: %v", got)
	}
	if status, _ := f.call(http.MethodGet, host, "", "/contents/"+f.cid(9)+"/similar", nil, nil); status != http.StatusNotFound {
		t.Fatalf("similar to nothing: %d", status)
	}
	if got, _ := f.list(host, "u1", q("/recommendations", "kind", "gallery")); !reflect.DeepEqual(got, []string{f.cid(3)}) {
		t.Fatalf("recommendations = %v", got)
	}

	if status, _ := f.call(http.MethodDelete, host, "u1", q("/me/history", "content_id", f.cid(1)), nil, nil); status != http.StatusNoContent {
		t.Fatalf("forget: %d", status)
	}
	if ids, _ := f.list(host, "u1", "/me/history"); len(ids) != 0 {
		t.Fatalf("history after forget = %v", ids)
	}
}

// A reader who has seen everything popular lately still gets what they have
// not seen: popular of all time past the 30-day window, then the newest.
func TestRecommendationsFillPastRecentPopularity(t *testing.T) {
	env := signaltest.FromEnv(t)
	f := seededOn(t, env.Fresh(t, signalCHDB), signalCHDB)
	f.seed(item{n: 8, titles: map[string]string{"en": "Hotel"}, age: 6 * time.Hour, versions: []ver{{langs: []string{"en"}}}},
		item{n: 9, titles: map[string]string{"en": "India"}, age: 90 * 24 * time.Hour, versions: []ver{{langs: []string{"en"}}}})
	const host = "main.test"
	at := time.Now().UTC().Add(-time.Hour)
	var observed []engagementtest.Observation
	for _, n := range []int{1, 3} {
		observed = append(observed, engagementtest.Observation{ContentRef: contentref.NewVersion(tenant, "gallery", f.cid(n), f.vid(n, 0)), Subject: signal.Subject{UserID: "u1"},
			Type: engagementtest.TypeReading, EventID: fmt.Sprintf("u1-%d", n), OccurredAt: at, Progress: 5, ProgressMax: 20, DurationS: 60})
	}
	observed = append(observed, engagementtest.Observation{ContentRef: contentref.NewVersion(tenant, "gallery", f.cid(9), f.vid(9, 0)), Subject: signal.Subject{UserID: "u2"}, Type: engagementtest.TypeReading,
		EventID: "u2-9", OccurredAt: time.Now().UTC().AddDate(0, 0, -40), Progress: 10, ProgressMax: 10, DurationS: 80})
	if err := f.svc.o.Hub.RecordSignals(f.ctx, engagementtest.Native(observed)); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.list(host, "u1", q("/recommendations", "kind", "gallery")); !reflect.DeepEqual(got, []string{f.cid(9), f.cid(8)}) {
		t.Fatalf("recommendations = %v, want all-time popular then newest unseen", got)
	}
}
