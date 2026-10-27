//go:build hostperformance

package hostperformance

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

type httpCase struct {
	Name, Domain, Host, Language string
	Query                        url.Values
	Expected                     []string
	From, Until                  time.Time
}

type httpPage struct {
	Contents []struct {
		ID, Kind, Language string
		Version            struct{ ID string }
		Access             struct{ Level string }
	} `json:"contents"`
	Next       string `json:"next_cursor"`
	Popularity *struct {
		Bucket      string
		Ready       bool
		From, Until time.Time
	} `json:"popularity"`
}

type httpSample struct {
	Case                 string  `json:"case"`
	Seconds              float64 `json:"seconds"`
	Status, Bytes, Cards int
	Error                string `json:"error,omitempty"`
}

// httpCases derives the expected catalog selection independently from the
// public query. It applies the fixture's known domain/language/tag membership
// to the worker's stored rank order, then validates actual card hydration.
func (f *fixture) httpCases() []httpCase {
	t, ctx := f.t, f.t.Context()
	var out []httpCase
	for _, domain := range f.model.ContentFilters.Domains {
		if len(domain.Hosts) == 0 {
			t.Fatalf("domain %s has no request host", domain.Name)
		}
		for _, language := range append([]string{""}, f.model.Languages...) {
			for _, bucket := range f.model.Kinds[f.kind].Popularity {
				var from, until *time.Time
				var rank []string
				must(t, f.pool.QueryRow(ctx, "SELECT content_ids,covered_from,covered_until FROM "+f.schema+".content_popular_rankings WHERE tenant_id=$1 AND kind=$2 AND period=$3 AND domain=$4 AND language=$5", f.tenant, f.kind, bucket.ID, domain.Name, language).Scan(&rank, &from, &until))
				if from == nil || until == nil {
					t.Fatalf("no prepared coverage for %s/%s/%s", domain.Name, language, bucket.ID)
				}
				rows, e := f.pool.Query(ctx, "SELECT id,item,language,domain_number,level FROM "+f.schema+".benchmark_items WHERE id=ANY($1)", rank)
				must(t, e)
				admit := map[string]bool{}
				public := map[string]bool{}
				for rows.Next() {
					var id, lang, level string
					var item, dn int
					must(t, rows.Scan(&id, &item, &lang, &dn, &level))
					visible := language == "" || lang == language
					if f.app == "doujins" {
						switch domain.Name {
						case "main":
							visible = visible && dn == 0
						case "gay":
							visible = visible && dn == 1
						case "pet":
							visible = visible && dn == 2
						default:
							t.Fatalf("unmodelled D domain %s", domain.Name)
						}
					} else if domain.Name == "restricted" {
						visible = visible && item%10 != 0
					}
					admit[id], public[id] = visible, visible && level == "public"
				}
				must(t, rows.Err())
				rows.Close()
				base := httpCase{Name: domain.Name + "/" + language + "/" + bucket.ID, Domain: domain.Name, Host: domain.Hosts[0], Language: language, Query: url.Values{"kind": {f.kind}, "sort": {"popular:" + bucket.ID}, "language": {language}, "display_language": {"en"}, "summary": {"true"}, "limit": {"24"}}, From: from.UTC(), Until: until.UTC()}
				for _, id := range rank {
					if admit[id] {
						base.Expected = append(base.Expected, id)
					}
				}
				if len(base.Expected) == 0 {
					t.Fatalf("scope has no known ranked public cards: %s", base.Name)
				}
				out = append(out, base)
				// The same covered range must select the same ordered IDs through
				// the actual ClickHouse -> PostgreSQL arbitrary-range path.
				if bucket.ID == "7d" || bucket.ID == "30d" || bucket.ID == "365d" {
					ranged := base
					ranged.Name += "/range"
					ranged.Query = cloneQuery(base.Query)
					ranged.Query.Set("sort", "popular")
					ranged.Query.Set("activity_from", from.UTC().Format(time.DateOnly))
					ranged.Query.Set("activity_until", until.UTC().Format(time.DateOnly))
					out = append(out, ranged)
				}
				if bucket.ID == "7d" {
					filtered := base
					filtered.Name += "/public"
					filtered.Query = cloneQuery(base.Query)
					filtered.Query.Set("access", "public")
					filtered.Expected = nil
					for _, id := range rank {
						if public[id] {
							filtered.Expected = append(filtered.Expected, id)
						}
					}
					if len(filtered.Expected) > 0 {
						out = append(out, filtered)
					}
				}
			}
		}
	}
	return out
}

func cloneQuery(v url.Values) url.Values {
	out := url.Values{}
	for k, vs := range v {
		out[k] = slices.Clone(vs)
	}
	return out
}

func (f *fixture) requestPage(ctx context.Context, c httpCase) (httpPage, httpSample) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	var page httpPage
	sample := httpSample{Case: c.Name}
	start := time.Now()
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, f.server.URL+"/content/v1/contents?"+c.Query.Encode(), nil)
	if e != nil {
		sample.Error = e.Error()
		return page, sample
	}
	req.Host = c.Host
	response, e := f.server.Client().Do(req)
	if e != nil {
		sample.Error = e.Error()
		sample.Seconds = time.Since(start).Seconds()
		return page, sample
	}
	defer response.Body.Close()
	b, e := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	sample.Seconds = time.Since(start).Seconds()
	sample.Status = response.StatusCode
	sample.Bytes = len(b)
	if e != nil {
		sample.Error = e.Error()
		return page, sample
	}
	if response.StatusCode != http.StatusOK {
		sample.Error = fmt.Sprintf("HTTP%d: %.500s", response.StatusCode, b)
		return page, sample
	}
	if e = json.Unmarshal(b, &page); e != nil {
		sample.Error = e.Error()
		return page, sample
	}
	sample.Cards = len(page.Contents)
	return page, sample
}

func (f *fixture) validatePage(c httpCase, page httpPage, sample *httpSample, offset int) {
	if sample.Error != "" {
		return
	}
	expected := c.Expected[min(offset, len(c.Expected)):min(offset+24, len(c.Expected))]
	ids := make([]string, 0, len(page.Contents))
	for _, card := range page.Contents {
		ids = append(ids, card.ID)
		if card.Kind != f.kind || card.Version.ID == "" || c.Language != "" && card.Language != c.Language {
			sample.Error = fmt.Sprintf("wrong hydrated card kind/version/language: %+v", card)
			return
		}
		if c.Query.Get("access") == "public" && card.Access.Level != "public" {
			sample.Error = "non-public card in public access filter"
			return
		}
	}
	if !slices.Equal(ids, expected) {
		sample.Error = fmt.Sprintf("ordered IDs differ: got%v want%v", ids, expected)
		return
	}
	if page.Popularity == nil || !page.Popularity.Ready || !page.Popularity.From.Equal(c.From) || !page.Popularity.Until.Equal(c.Until) {
		sample.Error = "missing/wrong ready coverage metadata"
		return
	}
	if bucket, ok := strings.CutPrefix(c.Query.Get("sort"), "popular:"); ok && page.Popularity.Bucket != bucket {
		sample.Error = "wrong host bucket metadata"
	}
}

func (f *fixture) httpChecks(label string) {
	cases := f.httpCases()
	var receipts []httpSample
	for _, c := range cases {
		page, sample := f.requestPage(f.t.Context(), c)
		f.validatePage(c, page, &sample, 0)
		receipts = append(receipts, sample)
		if sample.Error != "" {
			save(f.t, filepath.Join(f.out, label+"-functional.json"), receipts)
			f.t.Fatalf("%s: %s", c.Name, sample.Error)
		}
		if len(c.Expected) > 24 && page.Next != "" {
			next := c
			next.Query = cloneQuery(c.Query)
			next.Query.Set("cursor", page.Next)
			next.Query.Del("summary")
			next.Name += "/cursor2"
			p, s := f.requestPage(f.t.Context(), next)
			f.validatePage(next, p, &s, 24)
			receipts = append(receipts, s)
			if s.Error != "" {
				save(f.t, filepath.Join(f.out, label+"-functional.json"), receipts)
				f.t.Fatalf("%s: %s", next.Name, s.Error)
			}
		}
	}
	save(f.t, filepath.Join(f.out, label+"-functional.json"), receipts)
	for _, concurrency := range []int{1, 4, 8} {
		f.httpLoad(label, concurrency, cases)
	}
}

// httpLoad is also callable by the fixture owner while a refresh runs. It
// performs no preparation and reports errors rather than timing fast failures.
func (f *fixture) httpLoad(label string, concurrency int, cases []httpCase) {
	count := number("HOST_PERF_REQUESTS", 48)
	// Every load level visits every configured case at least once. Increasing
	// HOST_PERF_REQUESTS supplies more samples without dropping smaller scopes.
	count = max(count, concurrency, len(cases))
	var samples []httpSample
	var mu sync.Mutex
	work := make(chan int)
	var wg sync.WaitGroup
	start := time.Now()
	for range concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := range work {
				c := cases[n%len(cases)]
				p, s := f.requestPage(f.t.Context(), c)
				f.validatePage(c, p, &s, 0)
				mu.Lock()
				samples = append(samples, s)
				mu.Unlock()
			}
		}()
	}
	for n := range count {
		work <- n
	}
	close(work)
	wg.Wait()
	elapsed := time.Since(start).Seconds()
	latency := make([]float64, 0, len(samples))
	failures := 0
	for _, s := range samples {
		latency = append(latency, s.Seconds)
		if s.Error != "" {
			failures++
		}
	}
	slices.Sort(latency)
	quantile := func(p float64) float64 {
		return latency[min(len(latency)-1, max(0, int(math.Ceil(p*float64(len(latency))))-1))]
	}
	save(f.t, filepath.Join(f.out, fmt.Sprintf("%s-load-%d.json", label, concurrency)), map[string]any{"concurrency": concurrency, "requests": count, "seconds": elapsed, "requests_per_second": float64(count) / elapsed, "p50_seconds": quantile(.50), "p95_seconds": quantile(.95), "p99_seconds": quantile(.99), "validation_errors": failures, "samples": samples, "boundary": "ContentKit HTTP incl PG catalog filtering/hydration; excludes host ingress/billing/browser/media bytes; first functional requests already warmed these selections"})
	if failures != 0 {
		f.t.Fatalf("%s concurrency%d: %d invalid responses, see retained samples", label, concurrency, failures)
	}
}
