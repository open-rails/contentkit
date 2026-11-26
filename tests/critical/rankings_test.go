package critical_test

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"

	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/discovery"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/kinds"
)

// rankingsJob is the precompute the analytics job runs after a rebuild of
// content_daily.
type rankingsJob struct {
	Generation time.Time `json:"generation"`
	Final      time.Time `json:"final,omitzero"`
}

func (rankingsJob) Kind() string { return "contentkit_rankings" }

// generation is every seeded day's content_daily generation: one rebuild.
const generation = "2026-01-01 00:00:00.000"

// daily is one content_daily row of the site main/en unless it names another.
type daily struct {
	id                      string
	day                     time.Time
	class, reaction, reason string
	people                  int
	language, domain        string
	outside                 bool
}

type popularity struct {
	t     *testing.T
	w     *world
	a     *application
	today time.Time
	tags  map[string]entities.EntityID
}

func (p *popularity) tenant() string { return p.a.cfg.Scope.AppID.String() }

func (p *popularity) db() string { return p.a.cfg.ClickHouse.Database }

func (p *popularity) seed(rows ...daily) {
	p.t.Helper()
	for _, r := range rows {
		language, domain := r.language, r.domain
		if language == "" {
			language = "en"
		}
		if domain == "" {
			domain = "main"
		}
		must(p.t, p.a.deps.ClickHouse.Exec(p.t.Context(), `INSERT INTO `+p.db()+`.content_daily
 (tenant,domain,language,day,content_kind,content_id,audience,class,reaction,reason,people,generation)
 VALUES (?,?,?,toDate(?),'article',toUUID(?),?,?,?,?,?,toDateTime64(?,3,'UTC'))`,
			p.tenant(), domain, language, r.day.Format(time.DateOnly), r.id, !r.outside, r.class, r.reaction, r.reason, uint32(r.people), generation))
	}
}

// audience seeds a day's active people by access on main/en.
func (p *popularity) audience(day time.Time, people map[string]int) {
	p.t.Helper()
	for access, n := range people {
		must(p.t, p.a.deps.ClickHouse.Exec(p.t.Context(), `INSERT INTO `+p.db()+`.audience_daily (tenant,domain,language,day,access,people,generation)
 VALUES (?,'main','en',toDate(?),?,?,toDateTime64(?,3,'UTC'))`, p.tenant(), day.Format(time.DateOnly), access, uint32(n), generation))
	}
}

func (p *popularity) tag(slug string, kind ...string) entities.EntityID {
	p.t.Helper()
	k := "tag"
	if len(kind) > 0 {
		k = kind[0]
	}
	created, err := p.a.engine.Client().PostEntities(p.t.Context(), []entities.EntityInput{{Kind: k, Slug: slug,
		Names: []entities.Name{{Language: "en", Kind: "name", Name: "Ranked " + slug}}}})
	must(p.t, err)
	p.tags[slug] = created[0].ID
	return created[0].ID
}

// article publishes an article with these entities, live since live.
func (p *popularity) article(title string, live time.Time, rule *access.Rule, members ...entities.EntityID) string {
	t := p.t
	t.Helper()
	admin := p.a.engine.Client()
	startPublicationRuntime(t, p.a)
	item, err := admin.PostContents(t.Context(), catalog.ContentInput{Kind: "article", Access: rule, Versions: []catalog.VersionInput{{Languages: []string{"en"},
		Text: map[string]catalog.VersionText{"en": {Title: "Ranked " + title, Body: "Ranked body " + title}}}}})
	must(t, err)
	links := []entities.Membership{}
	for _, id := range members {
		links = append(links, entities.Membership{EntityID: id})
	}
	_, err = admin.PutContentEntitiesShared(t.Context(), item.ID, entities.ContentEntities{Memberships: links}, ck.PutContentEntitiesSharedQuery{})
	must(t, err)
	_, err = admin.PostContentPublish(t.Context(), item.ID, catalog.PublishInput{})
	must(t, err)
	waitForWithin(t, 45*time.Second, func() bool {
		wakeDuePublicationJobs(t, p.a)
		managed, err := admin.GetManage(t.Context(), ck.GetManageQuery{Ids: []string{item.ID}})
		must(t, err)
		return len(managed) == 1 && len(managed[0].Versions) == 1 && managed[0].Versions[0].Live
	})
	// The catalog's history says it has been live, at this access, since live.
	for _, q := range []string{
		`UPDATE ` + p.table("content_facets") + ` SET live_from=$3, published_at=$3, updated_at=clock_timestamp() WHERE tenant_id=$1 AND content_id=$2`,
		`UPDATE ` + p.table("content_items") + ` SET published_at=$3 WHERE tenant_id=$1 AND id=$2`,
		`UPDATE ` + p.table("content_access_history") + ` SET occurred_at=$3, snapshot=jsonb_set(snapshot, '{live_from}', to_jsonb($3::timestamptz)) WHERE tenant_id=$1 AND content_id=$2`,
	} {
		_, err := p.w.pool.Exec(t.Context(), q, p.tenant(), item.ID, live)
		must(t, err)
	}
	return item.ID
}

func (p *popularity) table(name string) string { return pgx.Identifier{p.w.schema, name}.Sanitize() }

// precompute runs the engine's precompute job as the analytics job does
// after a rebuild, yesterday final; false when it failed.
func (p *popularity) precompute() bool {
	t := p.t
	t.Helper()
	queue, err := river.NewClient(riverpgxv5.New(p.w.pool), &river.Config{Schema: p.w.schema + "_river"})
	must(t, err)
	job, err := queue.Insert(t.Context(), rankingsJob{Generation: p.w.clock.now(), Final: p.today.AddDate(0, 0, -1)}, &river.InsertOpts{Queue: "contentkit", MaxAttempts: 1})
	must(t, err)
	var state rivertype.JobState
	waitForWithin(t, 2*time.Minute, func() bool {
		row, err := queue.JobGet(t.Context(), job.Job.ID)
		must(t, err)
		state = row.State
		return state == rivertype.JobStateCompleted || state == rivertype.JobStateDiscarded
	})
	return state == rivertype.JobStateCompleted
}

// reader reads as a visitor of host, signed in with token when it is set,
// sending headers.
func (p *popularity) reader(host, token string, headers ...string) *ck.Client {
	return ck.NewClient(p.a.server.URL+"/content", &http.Client{Transport: headerTransport{inner: domainTransport{test: p.t, auth: p.w.auth, host: host, token: token}, headers: headers}})
}

type headerTransport struct {
	inner   http.RoundTripper
	headers []string
}

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header = r.Header.Clone()
	for i := 0; i+1 < len(h.headers); i += 2 {
		r.Header.Set(h.headers[i], h.headers[i+1])
	}
	return h.inner.RoundTrip(r)
}

// among is the order ids take in page.
func among(page discovery.ContentPage, ids ...string) []string {
	out := []string{}
	for _, c := range page.Contents {
		if slices.Contains(ids, c.ID) {
			out = append(out, c.ID)
		}
	}
	return out
}

func (p *popularity) popular(c *ck.Client, q ck.GetContentsQuery) discovery.ContentPage {
	p.t.Helper()
	q.Kind, q.Sort = "article", "popular"
	if q.Mode == "" {
		q.Mode = "hide"
	}
	if q.Limit == 0 {
		q.Limit = 100
	}
	page, err := c.GetContents(p.t.Context(), q)
	must(p.t, err)
	return page
}

func (p *popularity) order(c *ck.Client, q ck.GetContentsQuery, want ...string) {
	p.t.Helper()
	if got := among(p.popular(c, q), want...); !slices.Equal(got, want) {
		p.t.Fatalf("popular %+v ranks %v; want %v", q, got, want)
	}
}

func (p *popularity) entities(c *ck.Client, q ck.GetEntitiesQuery) entities.EntityPage {
	p.t.Helper()
	if q.Limit == 0 {
		q.Limit = 200
	}
	page, err := c.GetEntities(p.t.Context(), q)
	must(p.t, err)
	return page
}

func entityOrder(page entities.EntityPage, ids ...entities.EntityID) []entities.EntityID {
	out := []entities.EntityID{}
	for _, g := range page.Entities {
		if slices.Contains(ids, g.ID) {
			out = append(out, g.ID)
		}
	}
	return out
}

func TestPopularity(t *testing.T) {
	w := newWorld(t, func(cfg *ck.Config) {
		cfg.Content.EntityKinds["tag"] = kinds.EntityKind{Route: "tags"}
		cfg.Content.EntityKinds["artist"] = kinds.EntityKind{Route: "artists", Artist: true}
		article := cfg.Content.Kinds["article"]
		article.Entities = []string{"tag", "artist"}
		cfg.Content.Kinds["article"] = article
		cfg.Content.ContentFilters = kinds.ContentFilters{AppliesTo: []string{"article"}, Domains: []kinds.Domain{
			{Name: "main", Hosts: []string{"popular.invalid"}, Filters: []kinds.ContentFilter{{Entity: kinds.EntityRef{Kind: "tag", Slug: "spoilers"}, Visible: false}}},
			{Name: "second", Hosts: []string{"second.invalid"}},
		}}
	})
	a := w.apps[0]
	a.server.Close()
	must(t, a.engine.Close(t.Context()))
	configureDiscoverySignals(t, w, a)
	var err error
	a.engine, err = ck.New(t.Context(), a.cfg, a.deps)
	must(t, err)
	a.server = mountEngine(t, a.engine)
	now := w.clock.now()
	p := &popularity{t: t, w: w, a: a, today: time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC), tags: map[string]entities.EntityID{}}
	day := func(n int) time.Time { return p.today.AddDate(0, 0, -n) }
	// run is t.Run with the helpers reporting to the subtest while it runs.
	run := func(name string, f func(t *testing.T)) {
		t.Run(name, func(st *testing.T) {
			p.t = st
			defer func() { p.t = t }()
			f(st)
		})
	}
	main := p.reader("popular.invalid", "")
	old := day(60)

	// Quality fixtures: A many good reads, B few complete ones, E three good,
	// C a 50/50 split of favourites and "not into", D never read.
	heavy, light, split, unread := p.tag("heavy"), p.tag("light"), p.tag("split"), p.tag("unread")
	A := p.article("A many good", old, nil, heavy)
	B := p.article("B few complete", old, nil, heavy)
	E := p.article("E three good", old, nil, light)
	C := p.article("C polarizing", old, nil, split)
	C2 := p.article("C2 liked by a few", old, nil, split)
	D := p.article("D unread", old, nil, unread)

	run("CK-J07-S50_pages_never_query_clickhouse_for_featured_windows", func(t *testing.T) {
		// With content_daily unreachable, featured lists still answer from
		// the cache: no page reads ClickHouse. Nothing was read yet, so every
		// article lists, newest first.
		ctx := t.Context()
		must(t, a.deps.ClickHouse.Exec(ctx, "RENAME TABLE "+p.db()+".content_daily TO "+p.db()+".content_daily_away"))
		restore := func() {
			_ = a.deps.ClickHouse.Exec(context.Background(), "RENAME TABLE "+p.db()+".content_daily_away TO "+p.db()+".content_daily")
		}
		defer restore()
		page := p.popular(main, ck.GetContentsQuery{})
		if page.Popularity == nil || page.Popularity.Window != "week" {
			t.Fatalf("the week served %+v; want the week", page.Popularity)
		}
		if got := among(page, A, B, C, D, E); len(got) != 5 {
			t.Fatalf("popular list shows %v; want every article", got)
		}
		if tags := p.entities(main, ck.GetEntitiesQuery{Kind: "tag"}); tags.Popularity == nil || len(entityOrder(tags, heavy, light, split, unread)) != 4 {
			t.Fatalf("tag listing %+v; want every tag, popular by default", tags.Popularity)
		}
		restore()
		for _, q := range []ck.GetContentsQuery{{Window: "fortnight"}, {Window: "month:2026-13"}, {Window: "2026-02-10..2026-02-01"}, {Page: 2, Cursor: "x"}} {
			q.Kind, q.Sort = "article", "popular"
			_, err := main.GetContents(t.Context(), q)
			requireError(t, err, 400, "invalid_request")
		}
		_, err := main.GetContents(t.Context(), ck.GetContentsQuery{Sort: "popular"})
		requireError(t, err, 400, "invalid_request")
		_, err = main.GetContents(t.Context(), ck.GetContentsQuery{Kind: "article", Sort: "newest", Window: "week"})
		requireError(t, err, 400, "invalid_request")
	})

	p.seed(daily{id: A, day: day(3), class: "good", people: 40},
		daily{id: B, day: day(3), class: "complete", people: 5},
		daily{id: E, day: day(3), class: "good", people: 3},
		daily{id: C, day: day(3), class: "complete", reaction: "favorite", people: 30},
		daily{id: C, day: day(3), class: "bounce", reaction: "dislike", reason: "not_into", people: 30, outside: true},
		daily{id: C2, day: day(3), class: "good", people: 5})
	if !p.precompute() {
		t.Fatal("precompute failed")
	}

	run("CK-J07-S51_quality_weighted_person_days_rank_galleries", func(t *testing.T) {
		// 40 good reads beat 5 complete ones; three good reads beat sixty
		// split between favourites and "not into", which sum below zero and
		// follow the unread article.
		page := p.popular(main, ck.GetContentsQuery{Window: "30d"})
		if got := among(page, A, B, E, D, C); !slices.Equal(got, []string{A, B, E, D, C}) {
			t.Fatalf("quality order %v; want A B E D C", got)
		}
		if page.Popularity == nil || !page.Popularity.Ready || page.Popularity.Window != "30d" || page.Popularity.ComputedAt == nil {
			t.Fatalf("snapshot %+v", page.Popularity)
		}
		p.order(main, ck.GetContentsQuery{Window: "30d"}, C2, C)
		// The tag's own list ranks by its audience: the "not into" readers are
		// outside it, so the split article leads there.
		p.order(main, ck.GetContentsQuery{Window: "30d", Entities: []string{string(split)}}, C, C2)
	})

	run("CK-J07-S52_entities_sum_their_galleries_and_show_polarization", func(t *testing.T) {
		listing := p.entities(main, ck.GetEntitiesQuery{Kind: "tag", Window: "30d"})
		if listing.Popularity == nil || !listing.Popularity.Ready {
			t.Fatalf("tag listing did not default to a ready popular ranking: %+v", listing.Popularity)
		}
		if got := entityOrder(listing, heavy, light, unread, split); !slices.Equal(got, []entities.EntityID{heavy, light, unread, split}) {
			t.Fatalf("tag order %v; want heavy light unread split", got)
		}
		for _, g := range listing.Entities {
			switch g.ID {
			case heavy:
				// 40 good (q 0.2) and 5 complete (q 0.4667).
				if g.Popularity == nil || math.Abs(g.Popularity.Score-(40*0.2+5*(0.6-0.25)/0.75)) > 1e-6 {
					t.Fatalf("heavy scores %+v; want the sum of its articles", g.Popularity)
				}
			case split:
				if g.Popularity == nil || g.Popularity.Polarization < 0.7 {
					t.Fatalf("split polarization %+v; want 2·30/(60+20)", g.Popularity)
				}
			case light:
				if g.Popularity == nil || g.Popularity.Polarization != 0 {
					t.Fatalf("light polarization %+v; want 0", g.Popularity)
				}
			}
		}
	})

	run("CK-J07-S53_windows_closed_ranges_and_reports", func(t *testing.T) {
		monthAgo := time.Date(p.today.Year(), p.today.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -2, 0)
		F := p.article("F read 20 days ago", old, nil)
		G := p.article("G read two days ago", old, nil)
		H := p.article("H read two months ago", monthAgo.AddDate(0, 0, -10), nil)
		I := p.article("I read later that month", monthAgo.AddDate(0, 0, -10), nil)
		p.seed(daily{id: F, day: day(20), class: "good", people: 10},
			daily{id: G, day: day(2), class: "good", people: 3},
			daily{id: H, day: monthAgo.AddDate(0, 0, 2), class: "good", people: 4},
			daily{id: I, day: monthAgo.AddDate(0, 0, 3), class: "good", people: 2})
		must(t, boolError(p.precompute()))
		// The week leaves F's reads out: it follows the read articles.
		week := p.popular(main, ck.GetContentsQuery{Window: "week"})
		if got := among(week, F, G); !slices.Equal(got, []string{G, F}) {
			t.Fatalf("week ranks %v; want G before F", got)
		}
		p.order(main, ck.GetContentsQuery{Window: "30d"}, F, G)
		p.order(main, ck.GetContentsQuery{Window: "all"}, A, F, G)
		month := "month:" + monthAgo.Format("2006-01")
		p.order(main, ck.GetContentsQuery{Window: month}, H, I)
		// A closed month is computed once: rows the rebuild adds later
		// (none ever do) leave its cached ranking as it was.
		p.seed(daily{id: I, day: monthAgo.AddDate(0, 0, 4), class: "complete", people: 30})
		p.order(main, ck.GetContentsQuery{Window: month}, H, I)
		// An open range is a report, computed per request.
		through := monthAgo.Format(time.DateOnly) + ".." + p.today.Format(time.DateOnly)
		p.order(main, ck.GetContentsQuery{Window: through}, I, H)
		page := p.popular(main, ck.GetContentsQuery{Window: through})
		if page.Popularity == nil || page.Popularity.Window != through || !page.Popularity.From.Equal(monthAgo) || !page.Popularity.Until.Equal(p.today.AddDate(0, 0, 1)) {
			t.Fatalf("report snapshot %+v", page.Popularity)
		}
	})

	run("CK-J07-S54_each_site_ranks_its_own_reads", func(t *testing.T) {
		J, K := p.article("J read in English", old, nil), p.article("K read in German", old, nil)
		p.seed(daily{id: J, day: day(2), class: "good", people: 8},
			daily{id: K, day: day(2), class: "good", people: 8, language: "de"},
			daily{id: K, day: day(2), class: "good", people: 20, domain: "second"})
		must(t, boolError(p.precompute()))
		p.order(main, ck.GetContentsQuery{Window: "30d"}, J, K)
		// Without a language the request's Accept-Language picks the site.
		p.order(p.reader("popular.invalid", "", "Accept-Language", "de-DE,de;q=0.9"), ck.GetContentsQuery{Window: "30d"}, K, J)
		p.order(p.reader("second.invalid", ""), ck.GetContentsQuery{Window: "30d"}, K, J)
	})

	run("CK-J07-S55_gated_days_scale_to_the_people_who_could_open_them", func(t *testing.T) {
		L := p.article("L public", old, nil)
		M := p.article("M gold only", old, &access.Rule{Level: access.TierLevel, Tiers: []string{"gold"}})
		for _, n := range []int{7, 8, 9} {
			// Ten of a hundred active people hold gold: five gold readers stand
			// for fifty of everyone, beating ten public readers.
			p.audience(day(n), map[string]int{"anonymous": 70, "account": 20, "gold": 10})
			p.seed(daily{id: L, day: day(n), class: "good", people: 10}, daily{id: M, day: day(n), class: "good", people: 5})
		}
		must(t, boolError(p.precompute()))
		p.order(main, ck.GetContentsQuery{Window: "30d", Mode: "lock"}, M, L)
	})

	run("CK-J07-S56_trending_ranks_per_live_day", func(t *testing.T) {
		N := p.article("N live all week", day(20), nil)
		O := p.article("O live for two days", day(2), nil)
		for n := 2; n <= 6; n++ {
			p.seed(daily{id: N, day: day(n), class: "good", people: 2})
		}
		p.seed(daily{id: O, day: day(2), class: "good", people: 8})
		must(t, boolError(p.precompute()))
		// 10 good reads over six-plus live days trail 8 over two-plus.
		p.order(main, ck.GetContentsQuery{Window: "week"}, O, N)
		p.order(main, ck.GetContentsQuery{Window: "30d"}, N, O)
	})

	run("CK-J07-S57_listings_drop_only_what_the_filters_block", func(t *testing.T) {
		spoilers := p.tag("spoilers")
		S := p.article("S spoilers", old, nil, spoilers)
		Q := p.article("Q unread neighbour", old, nil)
		p.seed(daily{id: S, day: day(2), class: "complete", people: 100})
		must(t, boolError(p.precompute()))
		// The site hides spoilers by default: its galleries and the tag drop
		// out, everything else stays, unread articles included.
		page := p.popular(main, ck.GetContentsQuery{Window: "30d"})
		if got := among(page, S, Q, A, D); !slices.Equal(got, []string{A, Q, D}) && !slices.Equal(got, []string{A, D, Q}) {
			t.Fatalf("signed-out list %v; want A and both unread articles, without S", got)
		}
		tags := p.entities(main, ck.GetEntitiesQuery{Kind: "tag", Window: "30d"})
		if got := entityOrder(tags, spoilers, heavy, unread); !slices.Equal(got, []entities.EntityID{heavy, unread}) {
			t.Fatalf("signed-out tags %v; want spoilers left out", got)
		}
		// A visitor's browser choice and an account's override show them.
		shown := p.reader("popular.invalid", "", "ContentKit-Reader-Preferences", fmt.Sprintf(`{"filters":[{"entity_id":%q,"visible":true}]}`, spoilers))
		p.order(shown, ck.GetContentsQuery{Window: "30d"}, S, A)
		_, token := w.person(t, 0)
		account := p.reader("popular.invalid", token)
		yes := true
		_, err := account.PatchMeContentFilters(t.Context(), discovery.ContentFiltersUpdate{Filters: []discovery.GuestFilterSetting{{EntityID: string(spoilers), Visible: &yes}}}, ck.PatchMeContentFiltersQuery{})
		must(t, err)
		p.order(account, ck.GetContentsQuery{Window: "30d"}, S, A)
		if got := entityOrder(p.entities(account, ck.GetEntitiesQuery{Kind: "tag", Window: "30d"}), spoilers, heavy); !slices.Equal(got, []entities.EntityID{spoilers, heavy}) {
			t.Fatalf("account tags %v; want spoilers first", got)
		}
	})

	run("CK-J07-S58_artists_show_their_read_through_rate", func(t *testing.T) {
		artist := p.tag("ranked-artist", "artist")
		first := p.article("P1 first work", day(100), nil, artist)
		next := p.article("P2 next work", day(40), nil, artist)
		state := p.table("reading_state")
		readers := []uuid.UUID{uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()}
		for i, s := range readers {
			lastSeen := day(35)
			if i == 4 {
				lastSeen = day(95) // never seen after the next work's release
			}
			_, err := w.pool.Exec(t.Context(), `INSERT INTO `+state+` (app_id,subject,content_id,first_day,last_seen_at,best_outcome) VALUES ($1,$2,$3,$4,$5,3)`,
				p.tenant(), s, first, day(90), lastSeen)
			must(t, err)
			if i < 2 {
				_, err = w.pool.Exec(t.Context(), `INSERT INTO `+state+` (app_id,subject,content_id,first_day,last_seen_at,best_outcome) VALUES ($1,$2,$3,$4,$4,2)`,
					p.tenant(), s, next, day(38))
				must(t, err)
			}
		}
		listing := p.entities(main, ck.GetEntitiesQuery{Kind: "artist"})
		for _, g := range listing.Entities {
			if g.ID == artist {
				if g.Popularity == nil || g.Popularity.ReadThrough == nil || g.Popularity.ReadThrough.Readers != 4 || g.Popularity.ReadThrough.Rate != 0.5 {
					t.Fatalf("artist read-through %+v; want 2 of 4", g.Popularity)
				}
				return
			}
		}
		t.Fatalf("artist listing %+v lacks the artist", listing.Entities)
	})

	run("CK-J07-S59_similar_weighs_rare_tags_and_the_artist", func(t *testing.T) {
		rare, common, creator := p.tag("sim-rare"), p.tag("sim-common"), p.tag("sim-artist", "artist")
		X := p.article("X anchor", old, nil, rare, common, creator)
		Y := p.article("Y shares the rare tag", old, nil, rare)
		Z := p.article("Z shares the common tag", old, nil, common)
		V := p.article("V shares the artist", old, nil, creator)
		W := p.article("W common but spoilers", old, nil, common, p.tags["spoilers"])
		U := p.article("U common too", old, nil, common)
		similar, err := main.GetContentSimilar(t.Context(), X, ck.GetContentSimilarQuery{Kind: "article", Mode: "hide"})
		must(t, err)
		if got := among(similar, V, Y, Z, U, W); len(got) != 4 || got[0] != V || got[1] != Y || slices.Contains(got, W) {
			t.Fatalf("similar to X %v; want V, Y, then the common ones, never W", got)
		}
	})

	run("CK-J07-S62_new_to_you_leaves_out_what_the_reader_opened", func(t *testing.T) {
		user, token := w.person(t, 0)
		reader := p.reader("popular.invalid", token)
		// The reader bounced from B and hid E; A they never opened.
		state := p.table("reading_state")
		_, err := w.pool.Exec(t.Context(), `INSERT INTO `+state+` (app_id,subject,content_id,first_day,last_seen_at,best_outcome) VALUES ($1,$2,$3,$4::timestamptz::date,$4::timestamptz,1)`, p.tenant(), user.ID, B, day(3))
		must(t, err)
		_, err = w.pool.Exec(t.Context(), `INSERT INTO `+state+` (app_id,subject,content_id,hidden) VALUES ($1,$2,$3,true)`, p.tenant(), user.ID, E)
		must(t, err)
		p.order(reader, ck.GetContentsQuery{Window: "30d"}, A, B)
		if got := among(p.popular(reader, ck.GetContentsQuery{Window: "30d", ViewStatus: "unseen"}), A, B, E, D); !slices.Equal(got, []string{A, D}) {
			t.Fatalf("new to you lists %v; want A then the unread D, without B and E", got)
		}
	})
	run("CK-J07-S60_cursors_keep_their_ranking_across_a_refresh", func(t *testing.T) {
		first := p.popular(main, ck.GetContentsQuery{Window: "30d", Limit: 2})
		full := p.popular(main, ck.GetContentsQuery{Window: "30d"})
		if first.NextCursor == "" || len(full.Contents) < 5 {
			t.Fatal("paging fixture too small")
		}
		ids := func(page discovery.ContentPage) []string {
			out := []string{}
			for _, c := range page.Contents {
				out = append(out, c.ID)
			}
			return out
		}
		before := ids(full)
		// A refresh puts D first.
		p.seed(daily{id: D, day: day(2), class: "complete", reaction: "favorite", people: 500})
		must(t, boolError(p.precompute()))
		if got := ids(p.popular(main, ck.GetContentsQuery{Window: "30d", Limit: 1})); !slices.Equal(got, []string{D}) {
			t.Fatalf("refreshed ranking starts %v; want D", got)
		}
		paged := ids(first)
		cursor := first.NextCursor
		for cursor != "" {
			page := p.popular(main, ck.GetContentsQuery{Window: "30d", Limit: 2, Cursor: cursor})
			paged = append(paged, ids(page)...)
			cursor = page.NextCursor
		}
		if !slices.Equal(paged, before) {
			t.Fatalf("paging across the refresh read %v; want the ranking it started %v", paged, before)
		}
	})

	run("CK-J07-S61_rankings_are_served_stale_while_clickhouse_fails", func(t *testing.T) {
		want := among(p.popular(main, ck.GetContentsQuery{Window: "30d"}), D, A, B)
		ctx := t.Context()
		must(t, a.deps.ClickHouse.Exec(ctx, "RENAME TABLE "+p.db()+".content_daily TO "+p.db()+".content_daily_away"))
		t.Cleanup(func() {
			_ = a.deps.ClickHouse.Exec(context.Background(), "RENAME TABLE "+p.db()+".content_daily_away TO "+p.db()+".content_daily")
		})
		if p.precompute() {
			t.Fatal("precompute succeeded without content_daily")
		}
		page := p.popular(main, ck.GetContentsQuery{Window: "30d"})
		if got := among(page, D, A, B); !slices.Equal(got, want) || page.Popularity == nil || !page.Popularity.Ready || page.Popularity.Stale {
			t.Fatalf("during the outage %v %+v; want the last ranking %v", got, page.Popularity, want)
		}
		before := w.clock.now()
		w.clock.set(before.Add(4 * time.Hour))
		t.Cleanup(func() { w.clock.set(before) })
		if page := p.popular(main, ck.GetContentsQuery{Window: "30d"}); page.Popularity == nil || !page.Popularity.Stale || !slices.Equal(among(page, D, A, B), want) {
			t.Fatalf("hours into the outage %+v; want the last ranking, marked stale", page.Popularity)
		}
		w.clock.set(before)
		must(t, a.deps.ClickHouse.Exec(ctx, "RENAME TABLE "+p.db()+".content_daily_away TO "+p.db()+".content_daily"))
		if !p.precompute() {
			t.Fatal("precompute failed after ClickHouse returned")
		}
	})
}

func boolError(ok bool) error {
	if !ok {
		return fmt.Errorf("precompute failed")
	}
	return nil
}
