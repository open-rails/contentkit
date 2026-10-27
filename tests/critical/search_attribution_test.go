package critical_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/signal"
)

type attributedBrowserVisit struct {
	Mode, Content, Version, Origin, Visit string
	Rank                                  uint32
	Active                                uint64
}

func TestBrowserSearchEngagementAttribution(t *testing.T) {
	dist := t.TempDir()
	build := exec.CommandContext(t.Context(), "pnpm", "--dir", "../../sdk/ui", "exec", "vite", "build", "--config", "../../tests/critical/browser/search-attribution.config.mjs")
	build.Env = append(os.Environ(), "CONTENTKIT_ATTRIBUTION_DIST="+dist, "RAYON_NUM_THREADS=1")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build shipped attribution harness: %v\n%s", err, output)
	}
	w := newWorld(t)
	a := w.apps[0]
	configureDiscoverySignals(t, w, a)
	// Real browser observation and signed activity receipts use the same wall
	// clock; the media fixture supplies the existing persistent activity signer.
	a.deps.Clock = nil
	attachMedia(t, w, a)
	user, credential := w.person(t, 0)
	query := "Nebula258"
	for n := range 6 {
		publishedArticle(t, a, fmt.Sprintf("%s result %02d", query, n+1))
	}
	reader := a.client(credential)
	waitForWithin(t, 45*time.Second, func() bool {
		found, err := reader.GetSearch(t.Context(), ck.GetSearchQuery{Q: query, Kind: "article", Limit: 10})
		return err == nil && len(found.Contents) == 6
	})
	page, err := reader.GetSearch(t.Context(), ck.GetSearchQuery{Q: query, Kind: "article", Limit: 2, Page: 2})
	must(t, err)
	if len(page.Contents) != 2 {
		t.Fatalf("page two has %d results", len(page.Contents))
	}
	expected := map[string]uint32{page.Contents[0].ID: 3, page.Contents[1].ID: 4}
	upstream, err := url.Parse(a.server.URL)
	must(t, err)
	mux := http.NewServeMux()
	mux.Handle("/content/", httputil.NewSingleHostReverseProxy(upstream))
	mux.HandleFunc("/fixture.json", func(out http.ResponseWriter, _ *http.Request) {
		out.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(out).Encode(map[string]string{"query": query, "user": user.ID, "token": credential})
	})
	// This observation-only fixture endpoint lets the browser prove that no
	// false empty exposure landed while a real search response is withheld.
	mux.HandleFunc("/fixture/exposure-count", func(out http.ResponseWriter, request *http.Request) {
		rows, err := a.deps.ClickHouse.Query(request.Context(), `SELECT countIf(stage='rendered'),countIf(stage='visible')
 FROM `+a.cfg.ClickHouse.Database+`.exposures FINAL WHERE tenant=? AND search_query=?`, a.cfg.Scope.AppID.String(), strings.ToLower(request.URL.Query().Get("query")))
		if err != nil {
			http.Error(out, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		var rendered, visible uint64
		if !rows.Next() {
			http.Error(out, "missing aggregate", http.StatusInternalServerError)
			return
		}
		if err := rows.Scan(&rendered, &visible); err != nil {
			http.Error(out, err.Error(), http.StatusInternalServerError)
			return
		}
		out.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(out).Encode(map[string]uint64{"rendered": rendered, "visible": visible})
	})
	files := http.FileServer(http.Dir(dist))
	mux.HandleFunc("/", func(out http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			files.ServeHTTP(out, r)
			return
		}
		http.ServeFile(out, r, filepath.Join(dist, "search-attribution.html"))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	fixture, err := json.Marshal(map[string]string{"url": server.URL, "query": query})
	must(t, err)
	command := exec.CommandContext(t.Context(), "node", "browser/search-attribution.mjs")
	command.Env = append(os.Environ(), "CONTENTKIT_BROWSER_FIXTURE="+string(fixture))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("actual browser attribution: %v\n%s", err, output)
	}
	var report struct {
		Status                string
		Results               []attributedBrowserVisit
		ZeroQuery, ErrorQuery string
	}
	must(t, json.Unmarshal(output, &report))
	if report.Status != "passed" || len(report.Results) != 7 {
		t.Fatalf("browser did not complete every navigation case: %s", output)
	}
	store, err := signal.NewStore(a.deps.ClickHouse, a.cfg.ClickHouse.Database)
	must(t, err)
	tenant := a.cfg.Scope.AppID.String()
	// Source columns are checked separately from the export so a broken join
	// cannot manufacture a plausible visit from click timing or another tab.
	var long, short uint64
	for _, visit := range report.Results {
		rows, err := a.deps.ClickHouse.Query(t.Context(), `SELECT origin_click_id,content_id,content_version_id,subject,
 sum(active_ms),count(),uniqExact(receipt_id) FROM `+a.cfg.ClickHouse.Database+`.engagement_segments FINAL
 WHERE tenant=? AND visit_id=? GROUP BY origin_click_id,content_id,content_version_id,subject`, tenant, visit.Visit)
		must(t, err)
		if !rows.Next() {
			rows.Close()
			t.Fatalf("browser visit %s was not persisted", visit.Mode)
		}
		var origin, content, version, subject string
		var active, count, receipts uint64
		must(t, rows.Scan(&origin, &content, &version, &subject, &active, &count, &receipts))
		if rows.Next() {
			rows.Close()
			t.Fatalf("visit %s changed attribution during checkpoints", visit.Mode)
		}
		must(t, rows.Err())
		rows.Close()
		if origin != visit.Origin || content != visit.Content || version != visit.Version || subject != user.ID || active < visit.Active || count != receipts {
			t.Fatalf("persisted visit differs from exact browser observation: mode=%s origin=%s content=%s version=%s subject=%s active=%d rows=%d receipts=%d", visit.Mode, origin, content, version, subject, active, count, receipts)
		}
		if visit.Mode == "same_tab_long" {
			long = active
		}
		if visit.Mode == "modifier_tab_short" {
			short = active
		}
	}
	if long < 1000 || short == 0 || short >= long {
		t.Fatalf("observed short and long visits not distinguishable: short=%d long=%d", short, long)
	}
	for _, stage := range []signal.ExposureStage{signal.StageRendered, signal.StageVisible} {
		exported, err := store.Attribution(t.Context(), tenant, signal.AttributionOptions{Stage: stage, Window: signal.LastDays(1, time.Now())})
		must(t, err)
		emptySearches := 0
		for _, render := range exported.Renders {
			if render.SearchQuery == strings.ToLower(report.ErrorQuery) {
				t.Fatal("failed search became an exposure export")
			}
			if render.SearchQuery != strings.ToLower(report.ZeroQuery) {
				continue
			}
			emptySearches++
			if render.Surface != signal.SurfaceSearch || render.QueryID == "" || len(render.Shown) != 0 || len(render.Clicks) != 0 {
				t.Fatalf("zero-result search fabricated placements or lost query context: %+v", render)
			}
		}
		wantEmpty := 0
		if stage == signal.StageRendered {
			wantEmpty = 1
		}
		if emptySearches != wantEmpty {
			t.Fatalf("zero-result %s exports=%d want=%d", stage, emptySearches, wantEmpty)
		}
		for _, visit := range report.Results {
			if visit.Origin == "" {
				continue
			}
			matched := 0
			for _, render := range exported.Renders {
				for _, click := range render.Clicks {
					if click.EventID != visit.Origin {
						continue
					}
					matched++
					isSearch := visit.Mode != "ordinary_grid"
					if isSearch && (render.SearchQuery != strings.ToLower(query) || render.QueryID == "") || !isSearch && render.SearchQuery != "" || render.Language != "en" || !click.Exposed || click.ContentID != visit.Content || click.Subject.UserID != user.ID {
						t.Fatalf("query/result click join lost context: stage=%s render=%+v click=%+v", stage, render, click)
					}
					if click.Position != visit.Rank {
						t.Fatalf("click rank=%d differs from observed DOM rank=%d", click.Position, visit.Rank)
					}
					if visit.Mode == "same_tab_long" && visit.Rank != expected[visit.Content] {
						t.Fatalf("initial page-two rank=%d want=%d", visit.Rank, expected[visit.Content])
					}
					if len(click.Visits) != 1 || click.Visits[0].VisitID != visit.Visit || !slices.Contains(click.Visits[0].VersionIDs, visit.Version) || click.Visits[0].ActiveMS < visit.Active {
						t.Fatalf("exact click did not select its own measured visit: mode=%s visits=%+v", visit.Mode, click.Visits)
					}
				}
			}
			if matched != 1 {
				t.Fatalf("stage%s click%s exported %d times", stage, visit.Origin, matched)
			}
		}
	}
	rows, err := a.deps.ClickHouse.Query(t.Context(), `SELECT countIf(signal_type!='click'),countIf(value<0),count(),uniqExact(event_id)
 FROM `+a.cfg.ClickHouse.Database+`.signals FINAL WHERE tenant=? AND subject=?`, tenant, user.ID)
	must(t, err)
	if !rows.Next() {
		rows.Close()
		t.Fatal("no signal aggregate")
	}
	var other, negative, clicks, identities uint64
	must(t, rows.Scan(&other, &negative, &clicks, &identities))
	must(t, rows.Err())
	rows.Close()
	if other != 0 || negative != 0 || clicks != 6 || identities != 6 {
		t.Fatalf("observed navigation invented dislike or duplicate clicks: other=%d negative=%d clicks=%d ids=%d", other, negative, clicks, identities)
	}
	foreign, err := store.Attribution(t.Context(), w.apps[1].cfg.Scope.AppID.String(), signal.AttributionOptions{Stage: signal.StageRendered})
	must(t, err)
	if len(foreign.Renders) != 0 || len(foreign.Unattributed) != 0 {
		t.Fatal("another app exported this reader's query/visit")
	}
	// Clearing the autocomplete history leaves event attribution intact under
	// its separately approved retention; account deletion detaches both.
	must(t, reader.DeleteMeSearches(t.Context(), ck.DeleteMeSearchesQuery{}))
	kept, err := store.Attribution(t.Context(), tenant, signal.AttributionOptions{Stage: signal.StageRendered})
	must(t, err)
	if len(kept.Renders) == 0 {
		t.Fatal("clearing recent search unexpectedly erased event attribution")
	}
	must(t, a.engine.AnonymizeAccount(t.Context(), user.ID))
	after, err := store.Attribution(t.Context(), tenant, signal.AttributionOptions{Stage: signal.StageRendered})
	must(t, err)
	for _, render := range after.Renders {
		if render.Subject.UserID == user.ID {
			t.Fatal("deleted account still owns an exported render")
		}
		for _, click := range render.Clicks {
			if click.Subject.UserID == user.ID {
				t.Fatal("deleted account still owns an exported click")
			}
		}
	}
	for _, click := range after.Unattributed {
		if click.Subject.UserID == user.ID {
			t.Fatal("deleted account still owns an unattributed click")
		}
	}
}
