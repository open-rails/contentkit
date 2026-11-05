package critical_test

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"math/bits"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/importer"
)

// isolation is a schema's record of imported favorite writes: every insert
// statement (rolled back or not) and each committed favorite row. Favorites
// of poison-* users fail at their insert, as only a write can tell.
type isolation struct {
	t    *testing.T
	ctx  context.Context
	pool *pgxpool.Pool
	qs   string
}

func newIsolation(t *testing.T, ctx context.Context, w *world) *isolation {
	t.Helper()
	qs := pgx.Identifier{w.schema}.Sanitize()
	_, err := w.pool.Exec(ctx, fmt.Sprintf(`CREATE SEQUENCE %[1]s.critical_favorite_writes;
CREATE FUNCTION %[1]s.critical_favorite_write() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN PERFORM nextval('%[1]s.critical_favorite_writes'); RETURN NULL; END$$;
CREATE TRIGGER critical_favorite_write BEFORE INSERT ON %[1]s.content_favorites FOR EACH STATEMENT EXECUTE FUNCTION %[1]s.critical_favorite_write();
CREATE SEQUENCE %[1]s.critical_flaky;
CREATE TABLE %[1]s.critical_favorites (content_id text, user_id text);
CREATE FUNCTION %[1]s.critical_favorite() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN
 IF NEW.user_id LIKE 'poison-%%' THEN RAISE EXCEPTION 'poisoned favorite' USING ERRCODE = 'check_violation'; END IF;
 IF NEW.user_id = 'flaky' THEN
  IF nextval('%[1]s.critical_flaky') = 1 THEN RAISE EXCEPTION 'flaky favorite' USING ERRCODE = 'serialization_failure'; END IF;
 END IF;
 INSERT INTO %[1]s.critical_favorites VALUES (NEW.content_id, NEW.user_id); RETURN NEW; END$$;
CREATE TRIGGER critical_favorite BEFORE INSERT ON %[1]s.content_favorites FOR EACH ROW EXECUTE FUNCTION %[1]s.critical_favorite();
CREATE TABLE %[1]s.critical_recounts (content_id text);
CREATE FUNCTION %[1]s.critical_recount() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN INSERT INTO %[1]s.critical_recounts VALUES (NEW.content_id); RETURN NEW; END$$;
CREATE TRIGGER critical_recount BEFORE UPDATE ON %[1]s.content_interaction_counts FOR EACH ROW
 WHEN (NEW.content_version_id IS NULL) EXECUTE FUNCTION %[1]s.critical_recount()`, qs))
	must(t, err)
	return &isolation{t: t, ctx: ctx, pool: w.pool, qs: qs}
}

// writes is how many favorite insert statements ran so far.
func (s *isolation) writes() int {
	s.t.Helper()
	var n int
	must(s.t, s.pool.QueryRow(s.ctx, `SELECT coalesce(pg_sequence_last_value('`+s.qs+`.critical_favorite_writes'), 0)`).Scan(&n))
	return n
}

// favorites is how many times each user's favorite of item was written.
func (s *isolation) favorites(item string) map[string]int {
	s.t.Helper()
	rows, err := s.pool.Query(s.ctx, `SELECT user_id, count(*) FROM `+s.qs+`.critical_favorites WHERE content_id=$1 GROUP BY user_id`, item)
	must(s.t, err)
	out := map[string]int{}
	for rows.Next() {
		var user string
		var n int
		must(s.t, rows.Scan(&user, &n))
		out[user] = n
	}
	must(s.t, rows.Err())
	return out
}

// recounts is how many times item's counts were rewritten.
func (s *isolation) recounts(item string) int {
	s.t.Helper()
	var n int
	must(s.t, s.pool.QueryRow(s.ctx, `SELECT count(*) FROM `+s.qs+`.critical_recounts WHERE content_id=$1`, item).Scan(&n))
	return n
}

type importCounts struct{ comments, likes, dislikes, favorites int }

func (s *isolation) counts(item string) importCounts {
	s.t.Helper()
	var got importCounts
	err := s.pool.QueryRow(s.ctx, `SELECT comment_count, likes, dislikes, favorites FROM `+s.qs+`.content_interaction_counts WHERE content_id=$1 AND content_version_id IS NULL`, item).
		Scan(&got.comments, &got.likes, &got.dislikes, &got.favorites)
	if err != pgx.ErrNoRows {
		must(s.t, err)
	}
	return got
}

type importFailure struct {
	section string
	index   int
	code    string
}

// importFailures are r's failures by section and index.
func importFailures(r importer.Result) []importFailure {
	out := []importFailure{}
	for _, f := range r.Failed {
		out = append(out, importFailure{f.Section, f.Index, f.Code})
	}
	sortImportFailures(out)
	return out
}

func sortImportFailures(fs []importFailure) {
	slices.SortFunc(fs, func(a, b importFailure) int {
		return cmp.Or(cmp.Compare(a.section, b.section), cmp.Compare(a.index, b.index))
	})
}

// A chunk's records that are bad on their own (an erased person, a value
// out of range, a missing target or poll option) fail alone before the
// chunk writes; those only a write can tell are found by writing the chunk
// in halves, halved again until each fails alone. The good records land
// once each, in a few writes, and the counts hold exactly them: recounted
// once per chunk, or queued once and drained.
func TestImportIsolatesBadRecords(t *testing.T) {
	w := newWorld(t, func(cfg *ck.Config) { cfg.Interactions.Limits.Disabled = true })
	a := w.apps[0]
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	s := newIsolation(t, ctx, w)
	const erased = "legacy-erased"
	must(t, a.engine.AnonymizeAccount(ctx, erased))

	for _, deferred := range []bool{false, true} {
		name := map[bool]string{false: "recounted", true: "deferred"}[deferred]
		t.Run(name, func(t *testing.T) {
			item := publishedArticle(t, a, "Isolated bad records "+name)
			on := importer.TargetRef{Content: &importer.Ref{ID: item.ID}}
			missing := importer.TargetRef{Content: &importer.Ref{ExternalID: "missing"}}
			want := []importFailure{}
			bad := func(section string, index int, code string) {
				want = append(want, importFailure{section, index, code})
			}
			batch := importer.Batch{Source: "isolated-" + name, DeferRecounts: deferred}

			for i := range 55 {
				c := importer.CommentRecord{ExternalID: fmt.Sprintf("c-%d", i), Target: on, UserID: fmt.Sprintf("author-%d", i%7), Body: fmt.Sprintf("Comment %d", i)}
				switch i {
				case 1:
					c.ReplyTo = &importer.Ref{ExternalID: "c-0"}
				case 9:
					c.UserID = erased
					bad(importer.SectionComments, i, importer.CodeErased)
				case 20:
					c.Body = "  "
					bad(importer.SectionComments, i, importer.CodeInvalid)
				case 41:
					c.ReplyTo = &importer.Ref{ID: contentref.NewID()}
					bad(importer.SectionComments, i, importer.CodeNotFound)
				}
				batch.Comments = append(batch.Comments, c)
			}
			var likes, dislikes int
			for i := range 40 {
				x := importer.ReactionRecord{Target: on, UserID: fmt.Sprintf("reactor-%d", i), Value: []int16{1, -1}[i%2]}
				switch i {
				case 3:
					x.Value = 7
					bad(importer.SectionReactions, i, importer.CodeInvalid)
				case 17:
					x.UserID = erased
					bad(importer.SectionReactions, i, importer.CodeErased)
				case 30:
					x.Target = missing
					bad(importer.SectionReactions, i, importer.CodeNotFound)
				default:
					if x.Value == 1 {
						likes++
					} else {
						dislikes++
					}
				}
				batch.Reactions = append(batch.Reactions, x)
			}
			// Two chunks: 997 written with two poisoned, then 200 with one.
			fans := map[string]int{}
			for i := range 1200 {
				f := importer.FavoriteRecord{Target: on, UserID: fmt.Sprintf("fan-%d", i)}
				switch i {
				case 10:
					f.UserID = erased
					bad(importer.SectionFavorites, i, importer.CodeErased)
				case 500:
					f.UserID = ""
					bad(importer.SectionFavorites, i, importer.CodeInvalid)
				case 700:
					f.Target = missing
					bad(importer.SectionFavorites, i, importer.CodeNotFound)
				case 333, 999, 1100:
					f.UserID = fmt.Sprintf("poison-%d", i)
					bad(importer.SectionFavorites, i, importer.CodeInvalid)
				default:
					fans[f.UserID] = 1
				}
				batch.Favorites = append(batch.Favorites, f)
			}
			batch.Polls = []importer.PollRecord{
				{ExternalID: "p", Question: "Which?", Language: "en", Options: []importer.PollOptionRecord{{ExternalID: "p-1", Label: "One"}, {ExternalID: "p-2", Label: "Two", Position: 1}}},
				{ExternalID: "q", Question: "Other?", Language: "en", Options: []importer.PollOptionRecord{{ExternalID: "q-1", Label: "Only"}}}}
			votes := map[string]int{"p-1": 0, "p-2": 0, "q-1": 0}
			for i := range 30 {
				v := importer.VoteRecord{Poll: importer.Ref{ExternalID: "p"}, Option: importer.Ref{ExternalID: []string{"p-1", "p-2"}[i%2]}, UserID: fmt.Sprintf("voter-%d", i)}
				switch i {
				case 5:
					v.Option = importer.Ref{ExternalID: "q-1"}
					bad(importer.SectionVotes, i, importer.CodeNotFound)
				case 12:
					v.UserID = erased
					bad(importer.SectionVotes, i, importer.CodeErased)
				case 25:
					v.UserID = ""
					bad(importer.SectionVotes, i, importer.CodeInvalid)
				default:
					votes[v.Option.ExternalID]++
				}
				batch.Votes = append(batch.Votes, v)
			}

			before := s.writes()
			result, err := a.engine.Client().PostImport(ctx, batch)
			must(t, err)
			sortImportFailures(want)
			if got := importFailures(result); !reflect.DeepEqual(got, want) {
				t.Fatalf("failed %+v; want %+v", got, want)
			}
			wantWritten := map[string]int{importer.SectionComments: 52, importer.SectionReactions: 37, importer.SectionFavorites: 1194, importer.SectionPolls: 2, importer.SectionVotes: 27}
			if !reflect.DeepEqual(result.Written, wantWritten) {
				t.Fatalf("wrote %v; want %v", result.Written, wantWritten)
			}
			ids := result.IDs[importer.RecordComment]
			if len(ids) != 52 || ids["c-9"] != "" || ids["c-20"] != "" || ids["c-41"] != "" {
				t.Fatalf("comment identities %v; want the 52 good ones only", ids)
			}

			// Every good favorite was written once, none twice, none lost.
			if got := s.favorites(item.ID); !maps.Equal(got, fans) {
				again, lost := 0, 0
				for user, n := range got {
					again += n - fans[user]
				}
				for user := range fans {
					if got[user] == 0 {
						lost++
					}
				}
				t.Fatalf("favorites written %d extra times and %d lost; want each of %d people's once", again, lost, len(fans))
			}
			bound := 0
			for _, c := range []struct{ n, bad int }{{997, 2}, {200, 1}} {
				bound += 1 + 2*c.bad*bits.Len(uint(c.n))
			}
			if n := s.writes() - before; n > bound {
				t.Fatalf("favorites took %d insert statements; halving needs at most %d, one by one took 1198", n, bound)
			}
			var stored, byErased int
			qs := s.qs
			must(t, w.pool.QueryRow(ctx, `SELECT count(*) FROM `+qs+`.content_favorites WHERE content_id=$1`, item.ID).Scan(&stored))
			must(t, w.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM `+qs+`.content_comments WHERE user_id=$1) + (SELECT count(*) FROM `+qs+`.content_reactions WHERE user_id=$1)
 + (SELECT count(*) FROM `+qs+`.content_favorites WHERE user_id=$1) + (SELECT count(*) FROM `+qs+`.content_poll_votes WHERE user_id=$1)`, erased).Scan(&byErased))
			if stored != len(fans) || byErased != 0 {
				t.Fatalf("stored %d favorites and %d rows of the erased person; want %d and 0", stored, byErased, len(fans))
			}

			wantCounts := importCounts{comments: 51, likes: likes, dislikes: dislikes, favorites: len(fans)}
			if deferred {
				if got := s.counts(item.ID); got != (importCounts{}) || s.recounts(item.ID) != 0 {
					t.Fatalf("deferred import rewrote counts %d times to %+v", s.recounts(item.ID), got)
				}
				// The article, the reply's thread and the voted poll.
				if result.RecountsQueued == nil || *result.RecountsQueued != 3 {
					t.Fatalf("queued %v targets; want 3", result.RecountsQueued)
				}
				drained, err := a.engine.Client().PostImport(ctx, importer.Batch{Source: batch.Source, Recount: importer.MaxRecount})
				must(t, err)
				if drained.Recounted != 3 || drained.RecountsQueued == nil || *drained.RecountsQueued != 0 {
					t.Fatalf("drain recounted %d with %v left; want 3 and 0", drained.Recounted, drained.RecountsQueued)
				}
			}
			// Comments, reactions and each favorites chunk recount once; a
			// drain recounts once.
			wantRecounts := map[bool]int{false: 4, true: 1}[deferred]
			if got := s.counts(item.ID); got != wantCounts || s.recounts(item.ID) != wantRecounts {
				t.Fatalf("counts %+v rewritten %d times; want %+v rewritten %d times", got, s.recounts(item.ID), wantCounts, wantRecounts)
			}
			var replies int
			must(t, w.pool.QueryRow(ctx, `SELECT reply_count FROM `+qs+`.content_comments WHERE id=$1::uuid`, ids["c-0"]).Scan(&replies))
			if replies != 1 {
				t.Fatalf("parent reply count %d; want 1", replies)
			}
			options := result.IDs[importer.RecordOption]
			got := map[string]int{}
			for external := range votes {
				var n int
				must(t, w.pool.QueryRow(ctx, `SELECT vote_count FROM `+qs+`.content_poll_options WHERE id=$1::uuid`, options[external]).Scan(&n))
				got[external] = n
			}
			if !maps.Equal(got, votes) {
				t.Fatalf("option vote counts %v; want %v", got, votes)
			}
		})
	}
}

// A batch of one record keeps the old fallback: a failed write is retried
// once alone, leaving its recount to the chunk. A record that fails for
// good fails after those two writes with its own error; one whose first
// write failed for a passing reason lands on the retry, recounted once. A
// record bad on its own fails the same way without a write.
func TestImportSingleRecordRetry(t *testing.T) {
	w := newWorld(t, func(cfg *ck.Config) { cfg.Interactions.Limits.Disabled = true })
	a := w.apps[0]
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	s := newIsolation(t, ctx, w)
	must(t, a.engine.AnonymizeAccount(ctx, "legacy-erased"))
	item := publishedArticle(t, a, "Single record retry")
	on := importer.TargetRef{Content: &importer.Ref{ID: item.ID}}
	for _, c := range []struct {
		user   string
		failed []importFailure
		writes int
		after  importCounts
	}{
		{user: "poison-1", failed: []importFailure{{importer.SectionFavorites, 0, importer.CodeInvalid}}, writes: 2},
		{user: "flaky", failed: []importFailure{}, writes: 2, after: importCounts{favorites: 1}},
		{user: "legacy-erased", failed: []importFailure{{importer.SectionFavorites, 0, importer.CodeErased}}, after: importCounts{favorites: 1}},
		{user: "", failed: []importFailure{{importer.SectionFavorites, 0, importer.CodeInvalid}}, after: importCounts{favorites: 1}},
	} {
		before, rewrites := s.writes(), s.recounts(item.ID)
		result, err := a.engine.Client().PostImport(ctx, importer.Batch{Source: "single-record", Favorites: []importer.FavoriteRecord{{Target: on, UserID: c.user}}})
		must(t, err)
		written := result.Written[importer.SectionFavorites]
		if got := importFailures(result); !reflect.DeepEqual(got, c.failed) || written != 1-len(c.failed) {
			t.Fatalf("%q: wrote %d and failed %+v; want %d and %+v", c.user, written, got, 1-len(c.failed), c.failed)
		}
		if n := s.writes() - before; n != c.writes {
			t.Fatalf("%q: %d insert statements; want %d", c.user, n, c.writes)
		}
		if got, recounted := s.counts(item.ID), s.recounts(item.ID)-rewrites; got != c.after || recounted != written {
			t.Fatalf("%q: counts %+v rewritten %d times; want %+v rewritten %d times", c.user, got, recounted, c.after, written)
		}
	}
}
