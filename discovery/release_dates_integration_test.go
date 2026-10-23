package discovery

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/open-rails/contentkit/access"
)

// dateBilling uses the same explicit-key answers as the daily-release fixture,
// with complete and failed prefix reads to exercise all listing paths.
type dateBilling struct {
	releaseBilling
	complete    bool
	unavailable bool
}

func (b *dateBilling) Held(ctx context.Context, subject string, q access.Query) (access.Answer, error) {
	out, err := b.releaseBilling.Held(ctx, subject, q)
	if b.unavailable {
		return access.Answer{}, errors.New("billing unavailable")
	}
	if b.complete {
		for _, prefix := range q.Prefixes {
			h := access.HeldKeys{}
			for key, held := range b.held {
				if held && strings.HasPrefix(key, prefix) {
					h.Keys = append(h.Keys, key)
				}
			}
			slices.Sort(h.Keys)
			out.Held[prefix] = h
		}
	}
	return out, err
}

func TestReleaseDatesSettleAccess(t *testing.T) {
	for _, state := range []string{"incomplete", "complete", "unavailable"} {
		t.Run(state, func(t *testing.T) {
			billing := &dateBilling{releaseBilling: releaseBilling{held: map[string]bool{}}, complete: state == "complete", unavailable: state == "unavailable"}
			f := newFixtureBilling(t, nil, "", siteConfig(), billing)
			f.now = time.Date(2020, 3, 2, 12, 0, 0, 0, time.UTC)
			f.seed(
				item{n: 1, titles: map[string]string{"en": "Public"}, age: time.Hour, versions: []ver{{langs: []string{"en"}}}},
				item{n: 2, titles: map[string]string{"en": "Owned member"}, age: time.Hour, rule: `{"level":"ppv"}`, versions: []ver{{langs: []string{"en"}}}},
				item{n: 3, titles: map[string]string{"en": "Locked listed"}, age: time.Hour, rule: `{"level":"ppv"}`, versions: []ver{{langs: []string{"en"}}}},
				item{n: 4, titles: map[string]string{"en": "Denied member"}, age: 31 * 24 * time.Hour, rule: `{"level":"ppv"}`, versions: []ver{{langs: []string{"en"}}}},
			)
			billing.held[tenant+":content:"+f.cid(2)] = true
			for _, mode := range []access.Mode{access.Lock, access.Hide} {
				t.Run(string(mode), func(t *testing.T) {
					reader := Reader{Actor: access.Actor{ID: "reader"}, Host: "main.test", Mode: mode}
					wantIDs := []string{f.cid(1), f.cid(2)}
					if state == "unavailable" {
						wantIDs = wantIDs[:1]
					}
					if mode == access.Lock {
						wantIDs = []string{f.cid(1), f.cid(2), f.cid(3), f.cid(4)}
					}
					page, err := f.svc.List(f.ctx, ListOptions{Reader: reader, Filter: Filter{Kind: "gallery"}})
					if err != nil {
						t.Fatal(err)
					}
					var gotIDs []string
					for _, card := range page.Contents {
						gotIDs = append(gotIDs, card.ID)
					}
					slices.Sort(gotIDs)
					slices.Sort(wantIDs)
					if !slices.Equal(gotIDs, wantIDs) {
						t.Fatalf("List = %v, want %v", gotIDs, wantIDs)
					}
					for _, by := range []string{"day", "month"} {
						billing.queries = nil
						out, err := f.svc.ReleaseDates(f.ctx, ReleaseDatesOptions{Reader: reader, Filter: Filter{Kind: "gallery"}, By: by})
						if err != nil {
							t.Fatal(err)
						}
						date := "2020-03-02"
						if by == "month" {
							date = "2020-03"
						}
						count := len(wantIDs)
						if mode == access.Lock {
							count--
						}
						want := []DateCount{{Date: date, Count: count, Free: 1}}
						if mode == access.Lock {
							older := "2020-01-31"
							if by == "month" {
								older = "2020-01"
							}
							want = append(want, DateCount{Date: older, Count: 1})
						}
						if !reflect.DeepEqual(out.Dates, want) || out.NextCursor != "" {
							t.Errorf("%s = %+v, want %+v", by, out, want)
						}
						prefixReads := 0
						for _, query := range billing.queries {
							if len(query.Prefixes) > 0 {
								prefixReads++
							}
						}
						if prefixReads != 1 {
							t.Errorf("%s read holdings %d times, want one request memo: %+v", by, prefixReads, billing.queries)
						}
					}
				})
			}
		})
	}
}

func TestReleaseDatesUsePreferredEdition(t *testing.T) {
	for _, state := range []string{"incomplete", "complete"} {
		t.Run(state, func(t *testing.T) {
			billing := &dateBilling{releaseBilling: releaseBilling{held: map[string]bool{}}, complete: state == "complete"}
			f := newFixtureBilling(t, nil, "", siteConfig(), billing)
			f.now = time.Date(2020, 3, 2, 12, 0, 0, 0, time.UTC)
			f.seed(item{n: 1, titles: map[string]string{"en": "Denied preferred", "ja": "Allowed sibling"}, age: time.Hour, rule: `{"level":"ppv"}`,
				versions: []ver{{langs: []string{"en"}}, {langs: []string{"ja"}, rule: strp(`{"level":"public"}`)}}})
			// A later sibling must not move the preferred edition's bucket.
			f.exec(`UPDATE {s}.content_facets SET published_at = $2 WHERE version_id = $1`, f.vid(1, 1), time.Date(2020, 4, 1, 12, 0, 0, 0, time.UTC))
			englishHidden := []DateCount{}
			if billing.complete {
				englishHidden = []DateCount{{Date: "2020-04-01", Count: 1, Free: 1}}
			}
			for _, tc := range []struct {
				language string
				mode     access.Mode
				want     []DateCount
			}{
				{"en", access.Hide, englishHidden},
				{"ja", access.Hide, []DateCount{{Date: "2020-04-01", Count: 1, Free: 1}}},
				{"en", access.Lock, []DateCount{{Date: "2020-03-02", Count: 1, Free: 0}}},
			} {
				reader := Reader{Actor: access.Actor{ID: "reader"}, Host: "main.test", DisplayLanguage: tc.language, Mode: tc.mode}
				page, err := f.svc.List(f.ctx, ListOptions{Reader: reader})
				if err != nil || len(page.Contents) != len(tc.want) {
					t.Fatalf("%s %s List = %+v: %v", tc.language, tc.mode, page, err)
				}
				if len(tc.want) > 0 && page.Contents[0].PublishedAt.Format(time.DateOnly) != tc.want[0].Date {
					t.Fatalf("%s %s List selected publication: %+v", tc.language, tc.mode, page.Contents[0])
				}
				for _, by := range []string{"day", "month"} {
					want := slices.Clone(tc.want)
					if by == "month" {
						for i := range want {
							want[i].Date = want[i].Date[:7]
						}
					}
					out, err := f.svc.ReleaseDates(f.ctx, ReleaseDatesOptions{Reader: reader, By: by})
					if err != nil || !reflect.DeepEqual(out.Dates, want) {
						t.Errorf("%s %s %s dates = %+v, want %+v: %v", tc.language, tc.mode, by, out, want, err)
					}
				}
			}
		})
	}
}

func TestReleaseDatesKeepFiltersTimeZoneAndCursor(t *testing.T) {
	billing := &dateBilling{releaseBilling: releaseBilling{held: map[string]bool{}}}
	f := newFixtureBilling(t, nil, "", siteConfig(), billing)
	f.now = time.Date(2020, 3, 2, 1, 0, 0, 0, time.UTC)
	f.entity("tag", "big-breasts", "yaoi")
	f.seed(
		item{n: 1, titles: map[string]string{"en": "Visible"}, versions: []ver{{langs: []string{"en"}}}, entities: []string{"yaoi"}},
		item{n: 2, titles: map[string]string{"en": "Earlier"}, age: 48 * time.Hour, versions: []ver{{langs: []string{"en"}}}, entities: []string{"yaoi"}},
		item{n: 3, titles: map[string]string{"en": "Optional hidden"}, versions: []ver{{langs: []string{"en"}}}, entities: []string{"big-breasts", "yaoi"}},
		item{n: 4, titles: map[string]string{"en": "Outside domain"}, versions: []ver{{langs: []string{"en"}}}},
		item{n: 5, titles: map[string]string{"en": "Internal fixed hidden"}, versions: []ver{{langs: []string{"en"}}}, restrictions: []string{"restricted"}, entities: []string{"yaoi"}},
		item{n: 6, titles: map[string]string{"en": "Denied newest bucket"}, age: -24 * time.Hour, rule: `{"level":"ppv"}`, versions: []ver{{langs: []string{"en"}}}, entities: []string{"yaoi"}},
	)
	for _, tc := range []struct {
		host     string
		entities []string
		want     []DateCount
	}{
		{"gay.test", nil, []DateCount{{Date: "2020-03-01", Count: 2, Free: 2}, {Date: "2020-02-28", Count: 1, Free: 1}}},
		{"gay.test", []string{f.ids["big-breasts"]}, []DateCount{{Date: "2020-03-01", Count: 1, Free: 1}}},
		{"clean.test", []string{f.ids["yaoi"]}, []DateCount{{Date: "2020-03-01", Count: 1, Free: 1}, {Date: "2020-02-28", Count: 1, Free: 1}}},
	} {
		o := ReleaseDatesOptions{Reader: Reader{Actor: access.Actor{ID: "reader"}, Host: tc.host, Mode: access.Hide},
			Filter: Filter{Kind: "gallery", Entities: tc.entities, TimeZone: "America/Los_Angeles"}, By: "day", Limit: 1}
		var dates []DateCount
		for range 4 {
			page, err := f.svc.ReleaseDates(f.ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			dates = append(dates, page.Dates...)
			if page.NextCursor == "" {
				break
			}
			if len(page.Dates) != 1 || page.NextCursor != page.Dates[0].Date {
				t.Fatalf("invalid date cursor: %+v", page)
			}
			o.Cursor = page.NextCursor
		}
		if !reflect.DeepEqual(dates, tc.want) {
			t.Errorf("%s groups %v: %+v, want %+v", tc.host, tc.entities, dates, tc.want)
		}
	}
	_, err := f.svc.ReleaseDates(f.ctx, ReleaseDatesOptions{Reader: Reader{Host: "main.test"}, Filter: Filter{Entities: []string{f.ids["restricted"]}}})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("internal entity filter: %v", err)
	}
}
