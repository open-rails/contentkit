package critical_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	ck "github.com/open-rails/contentkit"
)

func TestRecentSearchHistory(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	searches := pgx.Identifier{w.schema, "content_searches"}.Sanitize()
	user, token := w.person(t, 0)
	reader, anonymous := a.client(token), a.client("")
	search := func(c *ck.Client, session string, queries ...string) {
		t.Helper()
		for _, q := range queries {
			_, err := c.GetSearch(t.Context(), ck.GetSearchQuery{Q: q, Kind: "article", Session: session})
			must(t, err)
		}
	}
	recent := func(c *ck.Client, session string) []string {
		t.Helper()
		got, err := c.GetMeSearches(t.Context(), ck.GetMeSearchesQuery{Session: session, Limit: 100})
		must(t, err)
		out := []string{}
		for _, s := range got.Searches {
			out = append(out, s.Query)
		}
		return out
	}
	stored := func(where string, args ...any) int {
		t.Helper()
		var n int
		must(t, w.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+searches+` WHERE `+where, args...).Scan(&n))
		return n
	}
	expect := func(got []string, want ...string) {
		t.Helper()
		if want == nil {
			want = []string{}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("recent searches %q, want %q", got, want)
		}
	}

	t.Run("CK-J07-S21_clearing_one_or_all_recent_searches_deletes_them", func(t *testing.T) {
		search(reader, "", "First needle", "Second needle", "Third needle")
		search(anonymous, "browser-a", "Session needle")
		expect(recent(reader, ""), "Third needle", "Second needle", "First needle")

		must(t, reader.DeleteMeSearches(t.Context(), ck.DeleteMeSearchesQuery{Query: "  second NEEDLE "}))
		expect(recent(reader, ""), "Third needle", "First needle")
		if n := stored("subject = $1", user.ID); n != 2 {
			t.Fatalf("one-query clear left %d stored searches, want 2", n)
		}

		must(t, reader.DeleteMeSearches(t.Context(), ck.DeleteMeSearchesQuery{}))
		expect(recent(reader, ""))
		if n := stored("subject = $1", user.ID); n != 0 {
			t.Fatalf("clearing all kept %d stored searches", n)
		}
		expect(recent(anonymous, "browser-a"), "Session needle")

		search(reader, "", "Fourth needle")
		expect(recent(reader, ""), "Fourth needle")

		must(t, anonymous.DeleteMeSearches(t.Context(), ck.DeleteMeSearchesQuery{Session: "browser-a"}))
		expect(recent(anonymous, "browser-a"))
		expect(recent(reader, ""), "Fourth needle")
	})

	t.Run("CK-J07-S22_prune_deletes_expired_and_erased_peoples_searches", func(t *testing.T) {
		search(anonymous, "browser-b", "Expired needle", "Retained needle")
		_, err := w.pool.Exec(t.Context(), `UPDATE `+searches+` SET searched_at = now() - $2::interval WHERE query = $1`, "Expired needle", "181 days")
		must(t, err)
		_, err = w.pool.Exec(t.Context(), `UPDATE `+searches+` SET searched_at = now() - $2::interval WHERE query = $1`, "Retained needle", "179 days")
		must(t, err)

		erased, erasedToken := w.person(t, 0)
		search(a.client(erasedToken), "", "Erased needle")
		must(t, a.engine.AnonymizeAccount(t.Context(), erased.ID))
		if n := stored("subject = $1", erased.ID); n != 0 {
			t.Fatalf("erasure kept %d searches", n)
		}
		// A search recorded while the erasure ran commits after its delete.
		_, err = w.pool.Exec(t.Context(), `INSERT INTO `+searches+` (tenant_id, subject, normalized, query)
 SELECT tenant_id, actor_id, 'raced needle', 'Raced needle' FROM `+pgx.Identifier{w.schema, "content_erased_subjects"}.Sanitize()+` WHERE actor_id = $1`, erased.ID)
		must(t, err)
		if n := stored("subject = $1", erased.ID); n != 1 {
			t.Fatalf("raced search fixture: %d rows", n)
		}

		// The hourly prune job runs when the engine starts its jobs.
		must(t, a.engine.StartJobs(t.Context()))
		jobs := pgx.Identifier{publicationRiverSchema(a), "river_job"}.Sanitize()
		waitForWithin(t, 45*time.Second, func() bool {
			var done int
			must(t, w.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+jobs+` WHERE kind = 'contentkit_search_prune' AND state = 'completed'`).Scan(&done))
			return done > 0
		})
		if n := stored("query = $1", "Expired needle"); n != 0 {
			t.Fatal("prune kept a search unused past its retention")
		}
		if n := stored("subject = $1", erased.ID); n != 0 {
			t.Fatal("prune kept an erased person's search")
		}
		expect(recent(anonymous, "browser-b"), "Retained needle")
		expect(recent(reader, ""), "Fourth needle")
	})
}
