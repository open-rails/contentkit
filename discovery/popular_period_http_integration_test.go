package discovery

import (
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/open-rails/contentkit/contentref"
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
	if err := f.svc.o.Hub.Signals().RecordSignals(f.ctx, tenant, []signal.Signal{{
		ContentRef: contentref.New(tenant, "gallery", f.cid(1)), Subject: signal.Subject{UserID: "popular-period-reader"},
		Type: signal.TypeView, EventID: "popular-period-view", OccurredAt: f.now.Add(-time.Hour),
		Progress: 10, ProgressMax: 10, DurationS: 100,
	}}); err != nil {
		t.Fatal(err)
	}
	for _, period := range []string{"7d", "30d"} {
		t.Run("valid/"+period, func(t *testing.T) {
			path := q("/contents", "kind", "gallery", "sort", "popular:"+period, "groups", f.ids["romance"])
			got, _ := f.list("main.test", "", path)
			if want := []string{f.cid(1)}; !reflect.DeepEqual(got, want) {
				t.Errorf("valid popular:%s got %v, want %v", period, got, want)
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
