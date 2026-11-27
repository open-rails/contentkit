package critical_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"

	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contentref"
	discovery "github.com/open-rails/contentkit/discovery"
	"github.com/open-rails/contentkit/importer"
	"github.com/open-rails/contentkit/internal/readingstore"
	"github.com/open-rails/contentkit/internal/subject"
	"github.com/open-rails/contentkit/signal"
)

// analyticsWorld is an app with a signal plane and its jobs running.
type analyticsWorld struct {
	*world
	a *application
}

func newAnalyticsWorld(t *testing.T, configure ...func(*ck.Config)) *analyticsWorld {
	t.Helper()
	w := newWorld(t, append([]func(*ck.Config){func(cfg *ck.Config) {
		cfg.Interactions.Anonymous = content.Anonymous{Reactions: true}
	}}, configure...)...)
	a := w.apps[0]
	a.server.Close()
	must(t, a.engine.Close(t.Context()))
	a.cfg.Database.SkipMigrations = true
	configureDiscoverySignals(t, w, a)
	var err error
	a.engine, err = ck.New(t.Context(), a.cfg, a.deps)
	must(t, err)
	a.server = mountEngine(t, a.engine)
	startPublicationRuntime(t, a)
	return &analyticsWorld{world: w, a: a}
}

// readerTransport presents a credential, or a signed-out browser's anonymous
// id, and the browser's UTC offset, as the SDK does.
type readerTransport struct {
	test             testing.TB
	w                *world
	token, anonymous string
	offset           string
}

func (r readerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header = req.Header.Clone()
	presentToken(r.test, r.w.auth, req, r.token)
	if r.anonymous != "" {
		req.Header.Set(subject.Header, r.anonymous)
	}
	if r.offset != "" {
		req.Header.Set(subject.OffsetHeader, r.offset)
	}
	return http.DefaultTransport.RoundTrip(req)
}

func (x *analyticsWorld) reader(t *testing.T, token, anonymous string) *ck.Client {
	return ck.NewClient(x.a.server.URL+"/content", &http.Client{Transport: readerTransport{test: t, w: x.world, token: token, anonymous: anonymous, offset: "-300"}})
}

// article is a published article with pages pages, as media processing
// would record them.
func (x *analyticsWorld) article(t *testing.T, title string, pages int) catalog.Content {
	t.Helper()
	item := publishedArticle(t, x.a, title)
	_, err := x.pool.Exec(t.Context(), `UPDATE `+x.schema+`.content_versions SET pages = $1 WHERE content_id = $2`, pages, item.ID)
	must(t, err)
	return item
}

func (x *analyticsWorld) app() uuid.UUID { return uuid.UUID(x.a.cfg.Scope.AppID) }

func (x *analyticsWorld) tenant() string { return x.a.cfg.Scope.AppID.String() }

// rows counts the activity log's rows of subject, waiting for the API's
// batched writes.
func (x *analyticsWorld) rows(t *testing.T, who uuid.UUID, want int) {
	t.Helper()
	waitForWithin(t, 30*time.Second, func() bool {
		rows, err := x.a.deps.ClickHouse.Query(t.Context(), `SELECT count() FROM `+x.a.cfg.ClickHouse.Database+`.activity WHERE tenant = ? AND subject = ?`, x.tenant(), who.String())
		must(t, err)
		defer rows.Close()
		var n uint64
		for rows.Next() {
			must(t, rows.Scan(&n))
		}
		return int(n) >= want
	})
}

// runJob runs one analytics job on the engine's River and waits for it.
func (x *analyticsWorld) runJob(t *testing.T) { t.Helper(); x.runAnalytics(t) }

// runAnalytics runs one analytics job on the apps' River and waits for it.
func (w *world) runAnalytics(t *testing.T) { t.Helper(); w.runContentJob(t, "contentkit_analytics") }

// awaitStartupRun waits for the run-on-start periodic job of kind, so it
// cannot fire later against an advanced clock.
func (w *world) awaitStartupRun(t *testing.T, kind string) {
	t.Helper()
	queue, err := river.NewClient(riverpgxv5.New(w.pool), &river.Config{Schema: w.schema + "_river"})
	must(t, err)
	waitForWithin(t, time.Minute, func() bool {
		done, err := queue.JobList(t.Context(), river.NewJobListParams().Kinds(kind).States(rivertype.JobStateCompleted).First(1))
		must(t, err)
		return len(done.Jobs) > 0
	})
}

// runContentJob runs one job of kind on the apps' River and waits for it.
func (w *world) runContentJob(t *testing.T, kind string) {
	t.Helper()
	queue, err := river.NewClient(riverpgxv5.New(w.pool), &river.Config{Schema: w.schema + "_river"})
	must(t, err)
	job, err := queue.Insert(t.Context(), scheduledContentJob{kind: kind}, &river.InsertOpts{Queue: "contentkit", MaxAttempts: 1})
	must(t, err)
	waitForWithin(t, 2*time.Minute, func() bool {
		row, err := queue.JobGet(t.Context(), job.Job.ID)
		must(t, err)
		if row.State == rivertype.JobStateRetryable || row.State == rivertype.JobStateDiscarded {
			t.Fatalf("%s job failed: %+v", kind, row.Errors)
		}
		return row.State == rivertype.JobStateCompleted
	})
}

type readingRow struct {
	firstDay, lastSeen *time.Time
	outcome            int16
	furthest           int32
	position           *int32
	hidden             bool
}

func (x *analyticsWorld) state(t *testing.T, who uuid.UUID, content string) (readingRow, bool) {
	t.Helper()
	var r readingRow
	err := x.pool.QueryRow(t.Context(), `SELECT first_day, last_seen_at, best_outcome, furthest, "position", hidden FROM `+x.schema+`.reading_state
 WHERE app_id = $1 AND subject = $2 AND content_id = $3`, x.app(), who, content).Scan(&r.firstDay, &r.lastSeen, &r.outcome, &r.furthest, &r.position, &r.hidden)
	if err != nil && strings.Contains(err.Error(), "no rows") {
		return r, false
	}
	must(t, err)
	return r, true
}

func (x *analyticsWorld) history(t *testing.T, who uuid.UUID, day time.Time) []readingstore.Action {
	t.Helper()
	var packed []byte
	err := x.pool.QueryRow(t.Context(), `SELECT actions FROM `+x.schema+`.interaction_history WHERE app_id = $1 AND subject = $2 AND day = $3`, x.app(), who, day).Scan(&packed)
	if err != nil && strings.Contains(err.Error(), "no rows") {
		return nil
	}
	must(t, err)
	actions, err := readingstore.DecodeDay(day, packed)
	must(t, err)
	return actions
}

func (x *analyticsWorld) advance(d time.Duration) { x.clock.set(x.clock.now().Add(d)) }

func open(id string) discovery.ActivityEvent {
	return discovery.ActivityEvent{Type: discovery.ActivityOpen, ContentID: id}
}

func progress(id string, active time.Duration, furthest, current, length uint32) discovery.ActivityEvent {
	return discovery.ActivityEvent{Type: discovery.ActivityProgress, ContentID: id, ActiveMS: uint32(active.Milliseconds()), Furthest: furthest, Current: current, Length: length}
}

func accountSubject(t *testing.T, id string) uuid.UUID {
	t.Helper()
	who, ok := subject.Account(id)
	if !ok {
		t.Fatalf("account %q is not a UUID", id)
	}
	return who
}

func ids(page discovery.ContentPage) []string {
	out := []string{}
	for _, c := range page.Contents {
		out = append(out, c.ID)
	}
	return out
}

func utcMidnight(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

func TestAnalyticsReading(t *testing.T) {
	x := newAnalyticsWorld(t)
	complete, good, bounced, neutral, flipped := x.article(t, "Read complete", 10), x.article(t, "Read good", 10), x.article(t, "Read bounced", 10),
		x.article(t, "Read neutral", 10), x.article(t, "Read flipped", 10)
	user, token := x.person(t, 0)
	member, who := x.reader(t, token, ""), accountSubject(t, user.ID)
	day := utcMidnight(x.clock.now())

	t.Run("CK-J32-S01_intake_stamps_the_activity_log", func(t *testing.T) {
		search, err := member.GetSearch(t.Context(), ck.GetSearchQuery{Q: "Read", Kind: "article"})
		must(t, err)
		if uuid.Validate(search.RequestID) != nil || len(search.Contents) == 0 {
			t.Fatalf("search did not name itself: %q, %d results", search.RequestID, len(search.Contents))
		}
		x.advance(time.Minute)
		must(t, member.PostActivity(t.Context(), discovery.Activity{Language: "en", Events: []discovery.ActivityEvent{
			{Type: discovery.ActivityOpen, ContentID: complete.ID, RequestID: search.RequestID, Surface: "search", Position: 1},
			{Type: discovery.ActivityOpen, ContentID: good.ID, Surface: "home", Position: 2}, open(bounced.ID), open(neutral.ID), open(flipped.ID),
			{Type: discovery.ActivityOpen, ContentID: contentref.NewID()}}}))
		x.advance(time.Minute)
		must(t, member.PostActivity(t.Context(), discovery.Activity{Events: []discovery.ActivityEvent{
			progress(complete.ID, 20*time.Second, 10, 10, 10), progress(good.ID, 8*time.Second, 4, 4, 10),
			progress(neutral.ID, 30*time.Second, 2, 2, 10), progress(flipped.ID, 3*time.Second, 10, 10, 10)}}))
		x.rows(t, who, 10) // a search, five opens and four reports; unknown content skipped
		rows, err := x.a.deps.ClickHouse.Query(t.Context(), `SELECT toString(action), toString(content_id), toString(domain), toString(language), toString(access), utc_offset,
 toString(request_id), toString(surface), position, toString(unit), query, length(results) FROM `+x.a.cfg.ClickHouse.Database+`.activity WHERE tenant = ? AND subject = ? ORDER BY ts, action`, x.tenant(), who.String())
		must(t, err)
		defer rows.Close()
		var searched, attributed, units int
		for rows.Next() {
			var action, contentID, domain, language, access, requestID, surface, unit, query string
			var offset int16
			var position uint16
			var results uint64
			must(t, rows.Scan(&action, &contentID, &domain, &language, &access, &offset, &requestID, &surface, &position, &unit, &query, &results))
			if access != "account" || offset != -300 || language != "en" {
				t.Fatalf("row %s not stamped: access %q offset %d language %q", action, access, offset, language)
			}
			switch {
			case action == "search":
				searched++
				if query != "read" || requestID != search.RequestID || int(results) != len(search.Contents) {
					t.Fatalf("search row = %q %q %d", query, requestID, results)
				}
			case action == "open" && contentID == complete.ID:
				attributed++
				if requestID != search.RequestID || surface != "search" || position != 1 {
					t.Fatalf("open row lost its list: %q %q %d", requestID, surface, position)
				}
			case action == "progress":
				units++
				if unit != "page" {
					t.Fatalf("progress unit %q", unit)
				}
			}
		}
		must(t, rows.Err())
		if searched != 1 || attributed != 1 || units != 4 {
			t.Fatalf("rows: %d searches, %d attributed opens, %d progress", searched, attributed, units)
		}
		requireError(t, x.reader(t, "", "").PostActivity(t.Context(), discovery.Activity{Events: []discovery.ActivityEvent{open(good.ID)}}), 400, "invalid_request")
		requireError(t, member.PostActivity(t.Context(), discovery.Activity{Events: []discovery.ActivityEvent{progress(good.ID, time.Hour, 1, 1, 10)}}), 400, "invalid_request")
	})

	t.Run("CK-J32-S03_the_job_classifies_each_day_by_fraction_read", func(t *testing.T) {
		x.runJob(t)
		for _, c := range []struct {
			item    catalog.Content
			outcome int16
		}{{complete, readingstore.OutcomeComplete}, {good, readingstore.OutcomeGood}, {bounced, readingstore.OutcomeBounce},
			{neutral, readingstore.OutcomeNeutral}, {flipped, readingstore.OutcomeNeutral}} {
			r, ok := x.state(t, who, c.item.ID)
			if !ok || r.outcome != c.outcome || r.firstDay == nil || !r.firstDay.Equal(day) || r.lastSeen == nil {
				t.Fatalf("%s: state %+v (found %v), want outcome %d", c.item.Versions[0].Text["en"].Title, r, ok, c.outcome)
			}
		}
		if r, _ := x.state(t, who, good.ID); r.furthest != 4 || r.position == nil || *r.position != 4 {
			t.Fatalf("good read's points = %+v", r)
		}
		actions := x.history(t, who, day)
		if len(actions) != 6 || actions[0].Kind != readingstore.ActionSearch || actions[0].Query != "read" || !actions[0].SessionStart || actions[0].Offset != -300 {
			t.Fatalf("history = %+v", actions)
		}
		for _, a := range actions[1:] {
			if a.Kind != readingstore.ActionGallery || a.SessionStart {
				t.Fatalf("gallery action = %+v", a)
			}
			if a.ID == uuid.MustParse(complete.ID) && (a.Tenths != 10 || a.ActiveS < 16) {
				t.Fatalf("complete read's action = %+v", a)
			}
			// Each gallery keeps where its first open came from.
			want := readingstore.SurfaceOutside
			switch a.ID {
			case uuid.MustParse(complete.ID):
				want = readingstore.SurfaceSearch
			case uuid.MustParse(good.ID):
				want = readingstore.SurfaceHome
			}
			if a.Surface != want {
				t.Fatalf("gallery %s surface = %d, want %d", a.ID, a.Surface, want)
			}
		}
	})

	t.Run("CK-J32-S04_reading_state_merges_by_maximum_and_latest", func(t *testing.T) {
		x.advance(48 * time.Hour)
		must(t, member.PostActivity(t.Context(), discovery.Activity{Events: []discovery.ActivityEvent{open(complete.ID), progress(complete.ID, 2*time.Second, 2, 2, 10)}}))
		x.rows(t, who, 12)
		x.runJob(t)
		x.runJob(t) // a repeated run changes nothing
		r, _ := x.state(t, who, complete.ID)
		if r.outcome != readingstore.OutcomeComplete || r.furthest != 10 || r.position == nil || *r.position != 2 || !r.firstDay.Equal(day) ||
			!r.lastSeen.After(day.Add(24*time.Hour)) {
			t.Fatalf("merged state = %+v", r)
		}
		later := x.history(t, who, utcMidnight(x.clock.now()))
		if len(later) != 1 || !later[0].SessionStart || later[0].Tenths != 2 {
			t.Fatalf("later day's history = %+v", later)
		}
	})

	t.Run("CK-J32-S12_cards_carry_the_readers_progress", func(t *testing.T) {
		page, err := member.GetContentCards(t.Context(), ck.GetContentCardsQuery{Ids: []string{good.ID, complete.ID}})
		must(t, err)
		for _, card := range page.Contents {
			v := card.Viewer
			switch {
			case v == nil:
				t.Fatalf("card %s has no viewer state", card.ID)
			case card.ID == good.ID && (!v.Seen || v.Completed || v.Outcome != "good" || v.Progress != 4 || v.ProgressMax != 10 || v.Percent != 40 || v.Position == nil || *v.Position != 4):
				t.Fatalf("good card's viewer = %+v", *v)
			case card.ID == complete.ID && (!v.Completed || v.Percent != 100):
				t.Fatalf("complete card's viewer = %+v", *v)
			}
		}
	})

	t.Run("CK-J32-S11_continue_lists_started_unfinished_reads", func(t *testing.T) {
		_, err := member.PostContentDislike(t.Context(), neutral.ID, ck.PostContentDislikeQuery{})
		must(t, err)
		page, err := member.GetMeContinue(t.Context(), ck.GetMeContinueQuery{Kind: "article"})
		must(t, err)
		// Finished, bounced and disliked reads are left out.
		if got := ids(page); !slices.Equal(got, []string{good.ID, flipped.ID}) && !slices.Equal(got, []string{flipped.ID, good.ID}) {
			t.Fatalf("continue = %v", got)
		}
		for _, card := range page.Contents {
			if card.Viewer == nil || card.Viewer.Position == nil {
				t.Fatalf("continue card %s lacks its position", card.ID)
			}
		}
	})

	t.Run("CK-J32-S08_hide_is_immediate_everywhere_and_undoable", func(t *testing.T) {
		must(t, member.PutContentHidden(t.Context(), good.ID))
		for _, q := range []ck.GetContentsQuery{{Kind: "article"}, {Kind: "article", Sort: "oldest"}} {
			page, err := member.GetContents(t.Context(), q)
			must(t, err)
			if slices.Contains(ids(page), good.ID) {
				t.Fatalf("hidden content listed: %v", ids(page))
			}
		}
		found, err := member.GetSearch(t.Context(), ck.GetSearchQuery{Q: "Read good", Kind: "article"})
		must(t, err)
		if slices.Contains(ids(found.ContentPage), good.ID) {
			t.Fatal("hidden content found by search")
		}
		cont, err := member.GetMeContinue(t.Context(), ck.GetMeContinueQuery{Kind: "article"})
		must(t, err)
		if slices.Contains(ids(cont), good.ID) {
			t.Fatal("hidden content in continue reading")
		}
		anonymous := uuid.NewString()
		stranger := x.reader(t, "", anonymous)
		page, err := stranger.GetContents(t.Context(), ck.GetContentsQuery{Kind: "article"})
		must(t, err)
		if !slices.Contains(ids(page), good.ID) {
			t.Fatal("another reader lost a hidden content")
		}
		must(t, stranger.PutContentHidden(t.Context(), bounced.ID))
		page, err = stranger.GetContents(t.Context(), ck.GetContentsQuery{Kind: "article"})
		must(t, err)
		if slices.Contains(ids(page), bounced.ID) {
			t.Fatal("a signed-out reader's hide did not apply")
		}
		must(t, member.DeleteContentHidden(t.Context(), good.ID))
		page, err = member.GetContents(t.Context(), ck.GetContentsQuery{Kind: "article"})
		must(t, err)
		if !slices.Contains(ids(page), good.ID) {
			t.Fatal("unhide did not restore the content")
		}
		if r, ok := x.state(t, who, good.ID); !ok || r.hidden || r.outcome != readingstore.OutcomeGood {
			t.Fatalf("unhide lost the read: %+v", r)
		}
		anon, _ := subject.Anonymous(x.app(), anonymous)
		x.rows(t, anon, 1)
		requireError(t, member.PutContentHidden(t.Context(), contentref.NewID()), 404, "not_found")
	})

	t.Run("CK-J32-S13_new_to_you_leaves_out_what_was_read_or_hidden", func(t *testing.T) {
		fresh := x.article(t, "Read fresh", 3)
		page, err := member.GetContents(t.Context(), ck.GetContentsQuery{Kind: "article", ViewStatus: "unseen"})
		must(t, err)
		if got := ids(page); !slices.Equal(got, []string{fresh.ID}) {
			t.Fatalf("new to you = %v; want only %s", got, fresh.ID)
		}
		anonymous := uuid.NewString()
		stranger := x.reader(t, "", anonymous)
		must(t, stranger.PostActivity(t.Context(), discovery.Activity{Events: []discovery.ActivityEvent{open(fresh.ID)}}))
		anon, _ := subject.Anonymous(x.app(), anonymous)
		x.rows(t, anon, 1)
		x.runJob(t)
		page, err = stranger.GetContents(t.Context(), ck.GetContentsQuery{Kind: "article", ViewStatus: "unseen"})
		must(t, err)
		if slices.Contains(ids(page), fresh.ID) || len(page.Contents) == 0 {
			t.Fatalf("a signed-out reader's new to you = %v", ids(page))
		}
		cards, err := stranger.GetContentCards(t.Context(), ck.GetContentCardsQuery{Ids: []string{fresh.ID}})
		must(t, err)
		if len(cards.Contents) != 1 || cards.Contents[0].Viewer == nil || !cards.Contents[0].Viewer.Seen || cards.Contents[0].Viewer.Outcome != "bounce" {
			t.Fatalf("a signed-out reader's card = %+v", cards.Contents)
		}
	})

	t.Run("CK-J32-S15_history_and_clearing_it", func(t *testing.T) {
		history, err := member.GetMeHistory(t.Context(), ck.GetMeHistoryQuery{Kind: "article", Status: "completed"})
		must(t, err)
		if got := ids(history); !slices.Equal(got, []string{complete.ID}) {
			t.Fatalf("completed history = %v", got)
		}
		stats, err := member.GetMeStats(t.Context())
		must(t, err)
		if stats.Completed["article"] != 1 {
			t.Fatalf("completed count = %v", stats.Completed)
		}
		must(t, member.PutContentHidden(t.Context(), neutral.ID))
		must(t, member.DeleteMeHistory(t.Context(), ck.DeleteMeHistoryQuery{}))
		history, err = member.GetMeHistory(t.Context(), ck.GetMeHistoryQuery{Kind: "article"})
		must(t, err)
		if len(history.Contents) != 0 || x.history(t, who, day) != nil {
			t.Fatalf("history survived clearing: %v", ids(history))
		}
		if r, ok := x.state(t, who, neutral.ID); !ok || !r.hidden || r.lastSeen != nil {
			t.Fatalf("clearing history unhid or kept a read: %+v", r)
		}
		x.runJob(t) // the raw rows are gone too: nothing comes back
		if _, ok := x.state(t, who, complete.ID); ok {
			t.Fatal("cleared reading came back from the activity log")
		}
	})
}

func TestAnalyticsReactionsAndFavoritesAreLogged(t *testing.T) {
	x := newAnalyticsWorld(t)
	item := x.article(t, "Logged reactions", 5)
	user, token := x.person(t, 0)
	member, who := x.reader(t, token, ""), accountSubject(t, user.ID)
	t.Run("CK-J32-S06_reactions_and_favorites_are_logged_where_written", func(t *testing.T) {
		_, err := member.PostContentLike(t.Context(), item.ID, ck.PostContentLikeQuery{})
		must(t, err)
		x.advance(time.Second)
		_, err = member.PostContentFavorite(t.Context(), item.ID, ck.PostContentFavoriteQuery{})
		must(t, err)
		x.rows(t, who, 2)
		rows, err := x.a.deps.ClickHouse.Query(t.Context(), `SELECT toString(action), value FROM `+x.a.cfg.ClickHouse.Database+`.activity WHERE tenant = ? AND subject = ? ORDER BY ts`, x.tenant(), who.String())
		must(t, err)
		defer rows.Close()
		var got []string
		for rows.Next() {
			var action string
			var value int8
			must(t, rows.Scan(&action, &value))
			got = append(got, action+":"+string(rune('0'+value)))
		}
		if !slices.Equal(got, []string{"reaction:1", "favorite:1"}) {
			t.Fatalf("logged = %v", got)
		}
		// A day with only a reaction makes no reading state.
		x.runJob(t)
		if _, ok := x.state(t, who, item.ID); ok {
			t.Fatal("a reaction alone created reading state")
		}
	})
}

func TestAnalyticsSignInFold(t *testing.T) {
	x := newAnalyticsWorld(t)
	read, liked, late := x.article(t, "Fold read", 4), x.article(t, "Fold liked", 4), x.article(t, "Fold late", 4)
	anonymous := uuid.NewString()
	anon, _ := subject.Anonymous(x.app(), anonymous)
	browser := x.reader(t, "", anonymous)
	t.Run("CK-J32-S05_sign_in_folds_the_anonymous_history_into_the_account", func(t *testing.T) {
		must(t, browser.PostActivity(t.Context(), discovery.Activity{Events: []discovery.ActivityEvent{open(read.ID), progress(read.ID, 10*time.Second, 4, 4, 4)}}))
		_, err := browser.PostContentLike(t.Context(), liked.ID, ck.PostContentLikeQuery{})
		must(t, err)
		_, err = browser.GetSearch(t.Context(), ck.GetSearchQuery{Q: "Fold", Kind: "article"})
		must(t, err)
		x.rows(t, anon, 4)
		x.runJob(t)
		if r, ok := x.state(t, anon, read.ID); !ok || r.outcome != readingstore.OutcomeComplete {
			t.Fatalf("anonymous reading state = %+v", r)
		}
		user, token := x.person(t, 0)
		account := accountSubject(t, user.ID)
		member := x.reader(t, token, "")
		must(t, member.PostMeAnonymousLink(t.Context(), discovery.AnonymousLink{AnonymousID: anonymous}))
		// A late row from the anonymous id counts as the account's.
		x.advance(time.Minute)
		must(t, browser.PostActivity(t.Context(), discovery.Activity{Events: []discovery.ActivityEvent{open(late.ID)}}))
		x.rows(t, anon, 5)
		x.runJob(t)
		if r, ok := x.state(t, account, read.ID); !ok || r.outcome != readingstore.OutcomeComplete {
			t.Fatalf("folded reading state = %+v (%v)", r, ok)
		}
		if _, ok := x.state(t, account, late.ID); !ok {
			t.Fatal("a late anonymous row did not count as the account's")
		}
		if _, ok := x.state(t, anon, read.ID); ok {
			t.Fatal("the anonymous reading state was kept")
		}
		if actions := x.history(t, account, utcMidnight(x.clock.now())); len(actions) < 3 {
			t.Fatalf("folded history = %+v", actions)
		}
		cards, err := member.GetContentCards(t.Context(), ck.GetContentCardsQuery{Ids: []string{liked.ID}})
		must(t, err)
		if len(cards.Contents) != 1 || cards.Contents[0].Viewer == nil || cards.Contents[0].Viewer.Reaction != 1 {
			t.Fatalf("the anonymous like was not re-keyed: %+v", cards.Contents)
		}
		searches, err := member.GetMeSearches(t.Context(), ck.GetMeSearchesQuery{})
		must(t, err)
		if len(searches.Searches) != 1 || searches.Searches[0].Query != "Fold" {
			t.Fatalf("the anonymous search was not re-keyed: %+v", searches.Searches)
		}
		// A fresh anonymous id after sign-out is someone new.
		next := uuid.NewString()
		must(t, x.reader(t, "", next).PostActivity(t.Context(), discovery.Activity{Events: []discovery.ActivityEvent{open(liked.ID)}}))
		fresh, _ := subject.Anonymous(x.app(), next)
		x.rows(t, fresh, 1)
		x.runJob(t)
		if _, ok := x.state(t, fresh, liked.ID); !ok {
			t.Fatal("a fresh anonymous id was folded")
		}
		if _, ok := x.state(t, account, liked.ID); ok {
			t.Fatal("a fresh anonymous id's reading reached the account")
		}
	})
}

func TestAnalyticsErasure(t *testing.T) {
	x := newAnalyticsWorld(t)
	item := x.article(t, "Erased reading", 4)
	anonymous := uuid.NewString()
	anon, _ := subject.Anonymous(x.app(), anonymous)
	user, token := x.person(t, 0)
	account := accountSubject(t, user.ID)
	t.Run("CK-J32-S07_erasure_deletes_reading_and_fences_the_log", func(t *testing.T) {
		must(t, x.reader(t, "", anonymous).PostActivity(t.Context(), discovery.Activity{Events: []discovery.ActivityEvent{open(item.ID)}}))
		x.rows(t, anon, 1)
		must(t, x.reader(t, token, "").PostMeAnonymousLink(t.Context(), discovery.AnonymousLink{AnonymousID: anonymous}))
		must(t, x.reader(t, token, "").PostActivity(t.Context(), discovery.Activity{Events: []discovery.ActivityEvent{open(item.ID), progress(item.ID, 10*time.Second, 4, 4, 4)}}))
		x.rows(t, account, 2)
		x.runJob(t)
		if _, ok := x.state(t, account, item.ID); !ok {
			t.Fatal("no reading state to erase")
		}
		must(t, x.a.engine.AnonymizeAccount(t.Context(), user.ID))
		if _, ok := x.state(t, account, item.ID); ok {
			t.Fatal("erasure kept the account's reading state")
		}
		var links, days int
		must(t, x.pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM `+x.schema+`.anonymous_links WHERE app_id = $1),
 (SELECT count(*) FROM `+x.schema+`.interaction_history WHERE app_id = $1 AND subject = ANY($2))`, x.app(), []uuid.UUID{account, anon}).Scan(&links, &days))
		if links != 0 || days != 0 {
			t.Fatalf("erasure kept %d links and %d history days", links, days)
		}
		// Late rows of the folded anonymous id never count again.
		x.advance(time.Minute)
		must(t, x.reader(t, "", anonymous).PostActivity(t.Context(), discovery.Activity{Events: []discovery.ActivityEvent{open(item.ID)}}))
		time.Sleep(3 * time.Second) // past the writer's flush
		x.advance(25 * time.Hour)   // the daily delete is due
		x.runJob(t)
		if _, ok := x.state(t, anon, item.ID); ok {
			t.Fatal("an erased anonymous id's late row made reading state")
		}
		var left uint64
		rows, err := x.a.deps.ClickHouse.Query(t.Context(), `SELECT count() FROM `+x.a.cfg.ClickHouse.Database+`.activity WHERE tenant = ? AND subject IN (?, ?)`,
			x.tenant(), account.String(), anon.String())
		must(t, err)
		for rows.Next() {
			must(t, rows.Scan(&left))
		}
		rows.Close()
		if left != 0 {
			t.Fatalf("the daily delete left %d erased rows", left)
		}
		// The next day's delete starts from the last erasure it handled.
		second, secondToken := x.person(t, 0)
		later := accountSubject(t, second.ID)
		must(t, x.reader(t, secondToken, "").PostActivity(t.Context(), discovery.Activity{Events: []discovery.ActivityEvent{open(item.ID)}}))
		x.rows(t, later, 1)
		must(t, x.a.engine.AnonymizeAccount(t.Context(), second.ID))
		x.advance(25 * time.Hour)
		x.runJob(t)
		x.rows(t, later, 0)
	})
}

func TestAnalyticsLegacyReadingImport(t *testing.T) {
	x := newAnalyticsWorld(t)
	item := x.article(t, "Imported reading", 6)
	user, _ := x.person(t, 0)
	account := accountSubject(t, user.ID)
	t.Run("CK-J32-S14_legacy_reading_imports_into_reading_state", func(t *testing.T) {
		at := x.clock.now().Add(-72 * time.Hour)
		res, err := x.a.engine.Client().PostImport(t.Context(), importBatch(item.ID, user.ID, at, 6, 6, false))
		must(t, err)
		if len(res.Failed) != 0 {
			t.Fatalf("import failed: %+v", res.Failed)
		}
		r, ok := x.state(t, account, item.ID)
		if !ok || r.outcome != readingstore.OutcomeComplete || r.furthest != 6 || !r.lastSeen.Equal(at.Truncate(time.Microsecond)) {
			t.Fatalf("imported state = %+v", r)
		}
		_, err = x.a.engine.Client().PostImport(t.Context(), importBatch(item.ID, user.ID, at, 0, 0, true))
		must(t, err)
		if _, ok := x.state(t, account, item.ID); ok {
			t.Fatal("an imported removal kept the reading")
		}
	})
}

func importBatch(contentID, accountID string, at time.Time, progress, max uint32, removed bool) importer.Batch {
	return importer.Batch{Source: "critical-legacy", LegacyReading: []importer.LegacyReadingRecord{{Target: importer.TargetRef{Content: &importer.Ref{ID: contentID}},
		UserID: accountID, LastReadAt: at, Progress: progress, ProgressMax: max, Removed: removed}}}
}

// logged counts subject's activity rows in tenant that reads still see:
// those of no recorded erasure.
func logged(t *testing.T, conn signal.Conn, database, tenant string, who uuid.UUID) uint64 {
	t.Helper()
	rows, err := conn.Query(t.Context(), `SELECT count() FROM `+database+`.activity WHERE tenant = ? AND subject = ?
 AND subject NOT IN (SELECT subject FROM `+database+`.erasures WHERE tenant = ?)`, tenant, who.String(), tenant)
	must(t, err)
	defer rows.Close()
	var n uint64
	for rows.Next() {
		must(t, rows.Scan(&n))
	}
	must(t, rows.Err())
	return n
}

// opened logs an open of content for the client's reader.
func opened(t *testing.T, client *ck.Client, content string) {
	t.Helper()
	must(t, client.PostActivity(t.Context(), discovery.Activity{Events: []discovery.ActivityEvent{open(content)}}))
}
