package critical_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/kinds"
)

func TestPreparedPopularityBrowser(t *testing.T) {
	if _, err := os.Stat(".browser-dist/popular.html"); err != nil {
		t.Fatal("build the SDK and critical browser harness before this test")
	}
	w := newWorld(t, func(cfg *ck.Config) {
		kind := cfg.Content.Kinds["article"]
		kind.Popularity = []kinds.PopularityBucket{
			{ID: "readers-week", Label: "Reader week", Days: 7},
			{ID: "readers-month", Label: "Reader month", Days: 30},
		}
		cfg.Content.Kinds["article"] = kind
	})
	a := w.apps[0]
	a.server.Close()
	must(t, a.engine.Close(t.Context()))
	configureDiscoverySignals(t, w, a)
	var err error
	a.engine, err = ck.New(t.Context(), a.cfg, a.deps)
	must(t, err)
	a.server = mountEngine(t, a.engine)
	first := publishedArticle(t, a, "Daily Alpha")
	second := publishedArticle(t, a, "Daily Beta")
	// Publishing starts River. Stop it before installing this read-only
	// prepared snapshot so its startup refresh cannot race the fixture rows.
	a.server.Close()
	must(t, a.engine.Close(t.Context()))
	a.engine, err = ck.New(t.Context(), a.cfg, a.deps)
	must(t, err)
	a.server = mountEngine(t, a.engine)
	tenant := a.cfg.Scope.AppID.String()
	day := w.clock.now().UTC().Truncate(24 * time.Hour)
	from, until := day.AddDate(0, 0, -2), day.AddDate(0, 0, 1)
	for _, bucket := range []string{"readers-week", "readers-month"} {
		ids := []string{first.ID, second.ID}
		if bucket == "readers-month" {
			ids = []string{second.ID, first.ID}
		}
		_, err := w.pool.Exec(t.Context(), `INSERT INTO `+w.schema+`.content_popular_rankings
 (tenant_id,kind,period,domain,language,computed_at,content_ids,window_from,window_until,covered_from,covered_until,available_from)
 VALUES($1,'article',$2,'','',$3,$4,$5,$6,$5,$6,$5)
 ON CONFLICT (tenant_id,kind,period,domain,language) DO UPDATE SET computed_at=excluded.computed_at,content_ids=excluded.content_ids,
 window_from=excluded.window_from,window_until=excluded.window_until,covered_from=excluded.covered_from,covered_until=excluded.covered_until,available_from=excluded.available_from`, tenant, bucket, day, ids, from, until)
		must(t, err)
	}
	for at := from; at.Before(until); at = at.AddDate(0, 0, 1) {
		must(t, a.deps.ClickHouse.Exec(t.Context(), `INSERT INTO `+a.cfg.ClickHouse.Database+`.popularity_daily_prepared
 (tenant,content_kind,domain,language,day,computed_at) VALUES(?,'article','','',?,now64(6))`, tenant, at))
		for i, id := range []string{first.ID, second.ID} {
			must(t, a.deps.ClickHouse.Exec(t.Context(), `INSERT INTO `+a.cfg.ClickHouse.Database+`.popularity_daily
 (tenant,content_kind,domain,language,day,content_id,readers,eligible,version) VALUES(?,'article','','',?,?,?,?,1)`, tenant, at, id, uint64(2-i), uint64(10)))
		}
	}
	base, err := url.Parse(a.server.URL)
	must(t, err)
	mux := http.NewServeMux()
	mux.Handle("/content/", httputil.NewSingleHostReverseProxy(base))
	mux.Handle("/", http.FileServer(http.Dir(".browser-dist")))
	server := httptest.NewServer(mux)
	defer server.Close()
	fixture, err := json.Marshal(map[string]string{
		"url": server.URL + "/popular.html", "first": first.ID, "second": second.ID,
		"from": from.Format(time.DateOnly), "until": until.Format(time.DateOnly), "last": day.Format(time.DateOnly),
		"before": from.AddDate(0, 0, -1).Format(time.DateOnly),
	})
	must(t, err)
	command := exec.CommandContext(t.Context(), "node", "browser/popular.mjs")
	command.Env = append(os.Environ(), "CONTENTKIT_BROWSER_FIXTURE="+string(fixture))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("real popularity browser: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), `"status":"passed"`) {
		t.Fatalf("browser did not complete: %s", output)
	}
	var rows int
	must(t, w.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+w.schema+`.content_popular_rankings`).Scan(&rows))
	if rows != 2 {
		t.Fatalf("browser range reads created ranking work: %d rows", rows)
	}
}
