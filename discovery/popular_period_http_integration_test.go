package discovery

import (
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/engagementtest"
	"github.com/open-rails/contentkit/internal/signaltest"
	"github.com/open-rails/contentkit/signal"
)

func TestSignaledPopularPeriodHTTPValidation(t *testing.T) {
	env := signaltest.FromEnv(t)
	database := "contentkit_popular_period_http_test"
	f := seededOn(t, env.Fresh(t, database), database)
	if !f.svc.o.Hub.SignalsConfigured() {
		t.Fatal("the regression requires the signaled popularity path")
	}
	reader, at := signal.Subject{UserID: "popular-period-reader"}, f.now.Add(-time.Hour)
	seedPopulation(f, "gallery", signal.ActivityScope{Domain: "main", Language: "en"}, at.Add(-time.Hour),
		[]signal.Subject{reader, {UserID: "nonreader-1"}, {UserID: "nonreader-2"}}, nil)
	if err := f.svc.o.Hub.Signals().RecordSignals(f.ctx, tenant, engagementtest.Native([]engagementtest.Observation{{
		ContentRef: contentref.NewVersion(tenant, "gallery", f.cid(1), f.vid(1, 0)), Subject: reader,
		Type: engagementtest.TypeReading, EventID: "popular-period-reading", OccurredAt: at,
		Progress: 10, ProgressMax: 10, DurationS: 100, Access: readingAccess(at, signal.ActivityScope{Domain: "main", Language: "en"}),
	}})); err != nil {
		t.Fatal(err)
	}
	for _, period := range []string{"7d", "30d"} {
		t.Run("valid/"+period, func(t *testing.T) {
			path := q("/contents", "kind", "gallery", "sort", "popular:"+period, "groups", f.ids["romance"])
			got, page := f.list("main.test", "", path)
			if want := []string{f.cid(1)}; !reflect.DeepEqual(got, want) || page.Ranking == nil || page.Ranking.Mode != "measured_then_standing" {
				t.Errorf("valid popular:%s got %v %+v, want %v", period, got, page.Ranking, want)
			}
		})
	}
	for _, kind := range []string{"gallery", "video"} {
		for _, sort := range []string{"popular:invalid", "popular:13d", "top_rated:invalid"} {
			t.Run(fmt.Sprintf("%s/%s", kind, sort), func(t *testing.T) {
				status, code := f.call(http.MethodGet, "main.test", "", q("/contents", "kind", kind, "sort", sort), nil, nil)
				if status != http.StatusBadRequest || code != "invalid_request" {
					t.Fatalf("invalid period must be a client error: got HTTP%d %s", status, code)
				}
			})
		}
	}
}
