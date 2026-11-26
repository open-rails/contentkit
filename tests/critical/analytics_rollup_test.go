package critical_test

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	ck "github.com/open-rails/contentkit"
	discovery "github.com/open-rails/contentkit/discovery"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/internal/signalstore"
	"github.com/open-rails/contentkit/internal/subject"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/signal"
)

// cell is one content_daily outcome of a gallery and day.
type cell struct {
	class, reaction, reason string
	audience                bool
}

// daily reads the newest generation of a gallery's content_daily day.
func (x *analyticsWorld) daily(t *testing.T, day time.Time, content string) map[cell]uint64 {
	t.Helper()
	rows, err := x.a.deps.ClickHouse.Query(t.Context(), `SELECT toString(class), toString(reaction), toString(reason), audience, sum(people)
 FROM `+x.a.cfg.ClickHouse.Database+`.content_daily WHERE tenant = ? AND day = ? AND content_id = ? AND generation =
 (SELECT max(generation) FROM `+x.a.cfg.ClickHouse.Database+`.content_daily WHERE tenant = ? AND day = ?)
 GROUP BY class, reaction, reason, audience`, x.tenant(), day.Format(time.DateOnly), content, x.tenant(), day.Format(time.DateOnly))
	must(t, err)
	defer rows.Close()
	out := map[cell]uint64{}
	for rows.Next() {
		var c cell
		var n uint64
		must(t, rows.Scan(&c.class, &c.reaction, &c.reason, &c.audience, &n))
		out[c] = n
	}
	must(t, rows.Err())
	return out
}

// generations counts a day's content_daily generations left after a rebuild.
func (x *analyticsWorld) generations(t *testing.T, table string, day time.Time) uint64 {
	t.Helper()
	rows, err := x.a.deps.ClickHouse.Query(t.Context(), `SELECT uniqExact(generation) FROM `+x.a.cfg.ClickHouse.Database+`.`+table+` WHERE tenant = ? AND day = ?`,
		x.tenant(), day.Format(time.DateOnly))
	must(t, err)
	defer rows.Close()
	var n uint64
	for rows.Next() {
		must(t, rows.Scan(&n))
	}
	return n
}

func TestAnalyticsDailyRollups(t *testing.T) {
	x := newAnalyticsWorld(t)
	item := x.article(t, "Rolled up", 4)
	user, token := x.person(t, 0)
	member, account := x.reader(t, token, ""), accountSubject(t, user.ID)
	anonymous := uuid.NewString()
	anon, _ := subject.Anonymous(x.app(), anonymous)
	day := utcMidnight(x.clock.now())
	t.Run("CK-J32-S09_daily_rollups_rebuild_a_whole_day_by_outcome_and_access", func(t *testing.T) {
		must(t, member.PostActivity(t.Context(), discovery.Activity{Events: []discovery.ActivityEvent{open(item.ID), progress(item.ID, 10*time.Second, 4, 4, 4)}}))
		must(t, x.reader(t, "", anonymous).PostActivity(t.Context(), discovery.Activity{Events: []discovery.ActivityEvent{open(item.ID)}}))
		x.rows(t, account, 2)
		x.rows(t, anon, 1)
		x.rollupDue(day)
		x.runJob(t)
		x.runJob(t)
		want := map[cell]uint64{{class: "complete", audience: true}: 1, {class: "bounce", audience: true}: 1}
		if got := x.daily(t, day, item.ID); !equalCells(got, want) {
			t.Fatalf("content_daily = %v; want %v", got, want)
		}
		active, err := x.a.engine.Client().GetStatsActive(t.Context(), ck.GetStatsActiveQuery{Days: 3})
		must(t, err)
		var today discovery.ActiveDay
		for _, d := range active.Days {
			if d.Date == day.Format(time.DateOnly) {
				today = d
			}
		}
		if today.People != 2 || today.ByAccess["account"] != 1 || today.ByAccess["anonymous"] != 1 {
			t.Fatalf("active today = %+v", today)
		}
		// A later rebuild, and the closing one after midnight, replace the day whole.
		if utcMidnight(x.clock.now()).Equal(day) {
			x.rollupDue(day)
			x.runJob(t)
		}
		if x.clock.now().Before(day.Add(24*time.Hour + 20*time.Minute)) {
			x.clock.set(day.Add(24*time.Hour + 20*time.Minute))
			x.runJob(t)
		}
		if got := x.daily(t, day, item.ID); !equalCells(got, want) {
			t.Fatalf("content_daily after rebuilds = %v", got)
		}
		for _, table := range []string{"content_daily", "audience_daily"} {
			if n := x.generations(t, table, day); n != 1 {
				t.Fatalf("%s keeps %d generations of the day", table, n)
			}
		}
	})
}

// rollupDue moves the clock to when day's rollups are rebuilt next: two
// hours on, past the hourly rebuild, or just after the closing rebuild when
// that would cross midnight.
func (x *analyticsWorld) rollupDue(day time.Time) {
	next := x.clock.now().Add(2 * time.Hour)
	if !utcMidnight(next).Equal(day) {
		next = day.Add(24*time.Hour + 20*time.Minute)
	}
	x.clock.set(next)
}

func equalCells(got, want map[cell]uint64) bool {
	if len(got) != len(want) {
		return false
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

func TestAnalyticsAudience(t *testing.T) {
	x := newAnalyticsWorld(t, func(cfg *ck.Config) {
		cfg.Content.EntityKinds["tag"] = kinds.EntityKind{Route: "tags", MembershipScopes: []kinds.MembershipScope{kinds.ContentScope, kinds.VersionScope}}
		article := cfg.Content.Kinds["article"]
		article.Entities = []string{"tag"}
		cfg.Content.Kinds["article"] = article
		cfg.Content.ContentFilters = kinds.ContentFilters{AppliesTo: []string{"article"}, Domains: []kinds.Domain{
			{Name: "main", Filters: []kinds.ContentFilter{{Entity: kinds.EntityRef{Kind: "tag", Slug: "niche"}, Visible: true}}}}}
	})
	admin := x.a.engine.Client()
	tags, err := admin.PostEntities(t.Context(), []entities.EntityInput{{Kind: "tag", Slug: "niche", Names: []entities.Name{{Language: "en", Kind: "name", Name: "Niche"}}}})
	must(t, err)
	item := x.article(t, "Polarizing", 4)
	_, err = admin.PutContentEntitiesShared(t.Context(), item.ID, entities.ContentEntities{Memberships: []entities.Membership{{EntityID: tags[0].ID}}}, ck.PutContentEntitiesSharedQuery{})
	must(t, err)
	day := utcMidnight(x.clock.now())
	t.Run("CK-J32-S10_a_polarizing_gallerys_audience_is_its_readers_with_affinity", func(t *testing.T) {
		fan, fanToken := x.person(t, 0)
		outsider, outsiderToken := x.person(t, 0)
		rejecter, rejecterToken := x.person(t, 0)
		for _, token := range []string{fanToken, rejecterToken} {
			must(t, x.reader(t, token, "").PostActivity(t.Context(), discovery.Activity{Events: []discovery.ActivityEvent{open(item.ID), progress(item.ID, 10*time.Second, 4, 4, 4)}}))
		}
		must(t, x.reader(t, outsiderToken, "").PostActivity(t.Context(), discovery.Activity{Events: []discovery.ActivityEvent{open(item.ID)}}))
		// The feedback prompt logs a dislike's reason beside the reaction, on
		// the site the API stamped on the reader's rows.
		x.advance(time.Minute)
		store, err := signalstore.NewStore(x.a.deps.ClickHouse, x.a.cfg.ClickHouse.Database)
		must(t, err)
		rejecterSubject := accountSubject(t, rejecter.ID)
		x.rows(t, rejecterSubject, 2)
		var domain, language string
		site, err := x.a.deps.ClickHouse.Query(t.Context(), `SELECT toString(domain), toString(language) FROM `+x.a.cfg.ClickHouse.Database+`.activity WHERE tenant = ? AND subject = ? LIMIT 1`,
			x.tenant(), rejecterSubject.String())
		must(t, err)
		for site.Next() {
			must(t, site.Scan(&domain, &language))
		}
		site.Close()
		_, err = store.Insert(t.Context(), x.tenant(), []signalstore.Row{{Subject: rejecterSubject, At: x.clock.now(), Action: signalstore.ActionReaction,
			ContentKind: "article", ContentID: uuid.MustParse(item.ID), Domain: domain, Language: language, Access: signalstore.AccessAccount, Value: -1, Reason: "not_into"}})
		must(t, err)
		x.rows(t, accountSubject(t, fan.ID), 2)
		x.rows(t, accountSubject(t, outsider.ID), 1)
		x.rows(t, rejecterSubject, 3)
		x.rollupDue(day)
		x.runJob(t)
		want := map[cell]uint64{
			{class: "complete", audience: true}:                                           1, // the fan
			{class: "bounce", audience: false}:                                            1, // the outsider: no affinity for its tag
			{class: "complete", reaction: "dislike", reason: "not_into", audience: false}: 1,
		}
		if got := x.daily(t, day, item.ID); !equalCells(got, want) {
			t.Fatalf("content_daily = %v; want %v", got, want)
		}
	})
}

// countingConn counts the INSERTs that reach ClickHouse.
type countingConn struct {
	signal.Conn
	inserts atomic.Int64
}

func (c *countingConn) Exec(ctx context.Context, query string, args ...any) error {
	if strings.HasPrefix(query, "INSERT INTO") {
		c.inserts.Add(1)
	}
	return c.Conn.Exec(ctx, query, args...)
}

func TestAnalyticsBatchedWrites(t *testing.T) {
	x := newAnalyticsWorld(t)
	t.Run("CK-J32-S02_writes_are_batched_and_survive_an_outage", func(t *testing.T) {
		conn := &countingConn{Conn: x.a.deps.ClickHouse}
		store, err := signalstore.NewStore(conn, x.a.cfg.ClickHouse.Database)
		must(t, err)
		var down atomic.Bool
		writer := signalstore.NewWriter(x.tenant(), func() *signalstore.Store {
			if down.Load() {
				return nil
			}
			return store
		}, nil)
		writer.Start()
		defer func() { must(t, writer.Close(context.Background())) }()
		who := uuid.New()
		var wg sync.WaitGroup
		for range 20 {
			wg.Go(func() {
				rows := make([]signalstore.Row, 25)
				for i := range rows {
					rows[i] = signalstore.Row{Subject: who, At: time.Now(), Action: signalstore.ActionOpen, ContentKind: "article", ContentID: uuid.New(), Access: signalstore.AccessAnonymous}
				}
				writer.Add(rows...)
			})
		}
		wg.Wait()
		x.rows(t, who, 500)
		if n := conn.inserts.Load(); n < 1 || n > 3 {
			t.Fatalf("500 rows from 20 requests took %d INSERTs", n)
		}
		// While ClickHouse is down rows wait in the buffer, then land once.
		down.Store(true)
		writer.Add(signalstore.Row{Subject: who, At: time.Now(), Action: signalstore.ActionOpen, ContentKind: "article", ContentID: uuid.New(), Access: signalstore.AccessAnonymous})
		time.Sleep(2500 * time.Millisecond)
		down.Store(false)
		x.rows(t, who, 501)
		time.Sleep(1500 * time.Millisecond)
		rows, err := x.a.deps.ClickHouse.Query(t.Context(), `SELECT count() FROM `+x.a.cfg.ClickHouse.Database+`.activity WHERE subject = ?`, who.String())
		must(t, err)
		defer rows.Close()
		var n uint64
		for rows.Next() {
			must(t, rows.Scan(&n))
		}
		if n != 501 {
			t.Fatalf("activity holds %d rows; want each once (501)", n)
		}
	})
}
