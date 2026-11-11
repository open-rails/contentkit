package critical_test

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/importer"
)

func TestImportedPollChoices(t *testing.T) {
	w := newWorld(t, func(c *ck.Config) { c.Interactions.Limits.Disabled = true })
	a := w.apps[0]
	admin := a.engine.Client()
	created := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	record := importer.PollRecord{ExternalID: "season", Question: "Choose a season", Language: "en", CreatedAt: &created,
		Options: []importer.PollOptionRecord{{ExternalID: "summer", Label: "Summer"}, {ExternalID: "winter", Label: "Winter", Position: 1}, {ExternalID: "spring", Label: "Spring", Position: 2}}}
	write := func(record importer.PollRecord, deferred bool) importer.Result {
		t.Helper()
		result, err := admin.PostImport(t.Context(), importer.Batch{Source: "legacy-polls", DeferRecounts: deferred, Polls: []importer.PollRecord{record}})
		must(t, err)
		if len(result.Failed) != 0 || result.Written[importer.SectionPolls] != 1 {
			t.Fatalf("poll import: %+v", result)
		}
		return result
	}
	initial := write(record, false)
	record.ID = initial.IDs[importer.RecordPoll][record.ExternalID]
	for i := range record.Options {
		record.Options[i].ID = initial.IDs[importer.RecordOption][record.Options[i].ExternalID]
	}
	person, _ := w.person(t, 0)
	_, token := w.person(t, 0)
	result, err := admin.PostImport(t.Context(), importer.Batch{Source: "legacy-polls", Votes: []importer.VoteRecord{
		{Poll: importer.Ref{ID: record.ID}, Option: importer.Ref{ID: record.Options[0].ID}, UserID: person.ID},
		{Poll: importer.Ref{ID: record.ID}, Option: importer.Ref{ID: record.Options[1].ID}, IP: "203.0.113.40"},
	}})
	must(t, err)
	if len(result.Failed) != 0 {
		t.Fatalf("votes: %+v", result)
	}
	// Change a kept voted choice, remove another voted choice and add a new one.
	questionImage, choiceImage := "i-"+contentref.NewID(), "i-"+contentref.NewID()
	record.Question, record.Image = "Choose again", &questionImage
	record.Options = []importer.PollOptionRecord{record.Options[0], {ExternalID: "autumn", Label: "Autumn"}}
	record.Options[0].Label, record.Options[0].Position, record.Options[0].Image = "Hot summer", 1, &choiceImage
	added := write(record, true) // poll replacement must recount even in bulk mode
	record.Options[1].ID = added.IDs[importer.RecordOption]["autumn"]
	for range 2 {
		write(record, true)
	}
	got, err := admin.GetPoll(t.Context(), record.ID)
	must(t, err)
	if got.Question != record.Question || got.TotalVotes != 1 || len(got.Options) != 2 || got.Options[0].ID != record.Options[1].ID || got.Options[0].VoteCount != 0 || got.Options[1].ID != record.Options[0].ID || got.Options[1].Label != "Hot summer" || got.Options[1].VoteCount != 1 {
		t.Fatalf("replacement lost identity/votes/counts: %+v", got)
	}
	var votes int
	must(t, w.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+w.schema+`.content_poll_votes WHERE tenant_id=$1 AND question_id=$2`, a.cfg.Scope.AppID.String(), record.ID).Scan(&votes))
	if votes != 1 {
		t.Fatalf("removed choice retained votes: %d", votes)
	}
	images := func(wantQuestion, wantChoice string) {
		t.Helper()
		var question, choice string
		must(t, w.pool.QueryRow(t.Context(), `SELECT coalesce(q.image_name,''),coalesce(o.image_name,'') FROM `+w.schema+`.content_poll_questions q JOIN `+w.schema+`.content_poll_options o ON o.tenant_id=q.tenant_id AND o.question_id=q.id WHERE q.tenant_id=$1 AND q.id=$2 AND o.id=$3`, a.cfg.Scope.AppID.String(), record.ID, record.Options[0].ID).Scan(&question, &choice))
		if question != wantQuestion || choice != wantChoice {
			t.Fatalf("images: %q %q, want %q %q", question, choice, wantQuestion, wantChoice)
		}
	}
	images(questionImage, choiceImage)
	record.Image, record.Options[0].Image = nil, nil
	write(record, false)
	images(questionImage, choiceImage)
	questionImage, choiceImage = "i-"+contentref.NewID(), "i-"+contentref.NewID()
	record.Image, record.Options[0].Image = &questionImage, &choiceImage
	write(record, false)
	images(questionImage, choiceImage)
	label := "Staff edit"
	_, err = admin.PatchPollOption(t.Context(), record.ID, record.Options[0].ID, content.PollOptionPatch{Label: &label})
	requireError(t, err, 409, "conflict")
	_, err = admin.PutPollOptionImage(t.Context(), record.ID, record.Options[0].ID, content.ImageInput{Image: new(string)})
	requireError(t, err, 409, "conflict")
	// Invalid images roll back metadata and mappings; a sibling still commits.
	for _, optionImage := range []bool{false, true} {
		bad := record
		bad.Options = slices.Clone(record.Options)
		bad.Question = "Must roll back"
		invalid := "https://invalid.example/image.png"
		if optionImage {
			bad.Options[0].Image = &invalid
		} else {
			bad.Image = &invalid
		}
		failed, err := admin.PostImport(t.Context(), importer.Batch{Source: "legacy-polls", Polls: []importer.PollRecord{bad, {ExternalID: fmt.Sprint("sibling-", optionImage), Question: "Sibling", Language: "en"}}})
		must(t, err)
		if len(failed.Failed) != 1 || failed.Failed[0].Code != importer.CodeInvalid || failed.Written[importer.SectionPolls] != 1 {
			t.Fatalf("invalid image isolation: %+v", failed)
		}
		after, err := admin.GetPoll(t.Context(), record.ID)
		must(t, err)
		if !reflect.DeepEqual(after, got) {
			t.Fatalf("failed image import changed poll: before=%+v after=%+v", got, after)
		}
	}
	record.Image, record.Options[0].Image = new(string), new(string)
	write(record, false)
	images("", "")
	// Removing every choice also removes every vote, including a native vote.
	_, err = a.client(token).PostPollVote(t.Context(), record.ID, content.PollVote{OptionID: record.Options[1].ID})
	must(t, err)
	record.Options = nil
	write(record, true)
	got, err = admin.GetPoll(t.Context(), record.ID)
	must(t, err)
	if got.TotalVotes != 0 || len(got.Options) != 0 {
		t.Fatalf("empty replacement: %+v", got)
	}
}

func TestImportedPollChoiceRaces(t *testing.T) {
	w := newWorld(t, func(c *ck.Config) { c.Interactions.Limits.Disabled = true })
	a := w.apps[0]
	admin := a.engine.Client()
	tenant := a.cfg.Scope.AppID.String()
	blockedBy := func(t *testing.T, pid uint32) uint32 {
		t.Helper()
		var waiter uint32
		waitForWithin(t, 10*time.Second, func() bool {
			must(t, w.pool.QueryRow(t.Context(), `SELECT coalesce(min(pid),0) FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid))`, pid).Scan(&waiter))
			return waiter != 0
		})
		return waiter
	}
	for _, name := range []string{"public_vote_before_import", "import_delete_before_public_vote", "imported_vote_before_staff_edit", "staff_delete_before_imported_vote"} {
		t.Run(name, func(t *testing.T) {
			created := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
			record := importer.PollRecord{ExternalID: name, Question: "Choose", Language: "en", CreatedAt: &created,
				Options: []importer.PollOptionRecord{{ExternalID: name + "-one", Label: "One"}, {ExternalID: name + "-two", Label: "Two", Position: 1}, {ExternalID: name + "-three", Label: "Three", Position: 2}}}
			initial, err := admin.PostImport(t.Context(), importer.Batch{Source: "poll-choice-race", Polls: []importer.PollRecord{record}})
			must(t, err)
			if len(initial.Failed) != 0 {
				t.Fatalf("poll import fixture failed: %+v", initial)
			}
			p, err := admin.GetPoll(t.Context(), initial.IDs[importer.RecordPoll][record.ExternalID])
			must(t, err)
			holder, err := w.pool.Begin(t.Context())
			must(t, err)
			defer holder.Rollback(t.Context())
			voteBatch := importer.Batch{Source: "poll-choice-race", Votes: []importer.VoteRecord{{Poll: importer.Ref{ID: p.ID}, Option: importer.Ref{ID: p.Options[2].ID}, IP: "203.0.113.41"}}}
			if name == "import_delete_before_public_vote" {
				_, err = holder.Exec(t.Context(), `SELECT 1 FROM `+w.schema+`.content_poll_options WHERE tenant_id=$1 AND id=$2 FOR UPDATE`, tenant, p.Options[2].ID)
				must(t, err)
				record.Options = record.Options[:2]
				imported := make(chan error, 1)
				go func() {
					result, err := admin.PostImport(t.Context(), importer.Batch{Source: voteBatch.Source, Polls: []importer.PollRecord{record}})
					if err == nil && len(result.Failed) != 0 {
						t.Errorf("replacement failed: %+v", result)
					}
					imported <- err
				}()
				importerPID := blockedBy(t, holder.Conn().PgConn().PID())
				_, token := w.person(t, 0)
				voted := make(chan error, 1)
				go func() {
					_, err := a.client(token).PostPollVote(t.Context(), p.ID, content.PollVote{OptionID: p.Options[2].ID})
					voted <- err
				}()
				blockedBy(t, importerPID)
				must(t, holder.Commit(t.Context()))
				must(t, <-imported)
				requireError(t, <-voted, 400, "invalid_request")
				got, err := admin.GetPoll(t.Context(), p.ID)
				must(t, err)
				if got.TotalVotes != 0 || len(got.Options) != 2 {
					t.Fatalf("vote raced past removed choice: %+v", got)
				}
				return
			}
			if name == "staff_delete_before_imported_vote" {
				_, err = holder.Exec(t.Context(), `SELECT 1 FROM `+w.schema+`.content_poll_questions WHERE tenant_id=$1 AND id=$2 FOR NO KEY UPDATE`, tenant, p.ID)
				must(t, err)
				imported := make(chan importer.Result, 1)
				importErr := make(chan error, 1)
				go func() { result, err := admin.PostImport(t.Context(), voteBatch); importErr <- err; imported <- result }()
				blockedBy(t, holder.Conn().PgConn().PID())
				_, err = holder.Exec(t.Context(), `DELETE FROM `+w.schema+`.content_poll_options WHERE tenant_id=$1 AND id=$2`, tenant, p.Options[2].ID)
				must(t, err)
				must(t, holder.Commit(t.Context()))
				must(t, <-importErr)
				result := <-imported
				if len(result.Failed) != 1 || result.Failed[0].Code != importer.CodeNotFound || result.Written[importer.SectionVotes] != 0 {
					t.Fatalf("vote import did not recheck removed choice: %+v", result)
				}
				return
			}
			// Pause a real vote at its option FK check, after taking the poll's
			// share lock. The competing choice write must wait for that vote.
			_, err = holder.Exec(t.Context(), `SELECT 1 FROM `+w.schema+`.content_poll_options WHERE tenant_id=$1 AND id=$2 FOR UPDATE`, tenant, p.Options[2].ID)
			must(t, err)
			voted := make(chan error, 1)
			if name == "public_vote_before_import" {
				_, token := w.person(t, 0)
				go func() {
					_, err := a.client(token).PostPollVote(t.Context(), p.ID, content.PollVote{OptionID: p.Options[2].ID})
					voted <- err
				}()
			} else {
				go func() {
					result, err := admin.PostImport(t.Context(), voteBatch)
					if err == nil && len(result.Failed) != 0 {
						t.Errorf("vote import failed: %+v", result)
					}
					voted <- err
				}()
			}
			voterPID := blockedBy(t, holder.Conn().PgConn().PID())
			changed := make(chan error, 1)
			if name == "public_vote_before_import" {
				record.Options[2].Label = "Changed"
				go func() {
					result, err := admin.PostImport(t.Context(), importer.Batch{Source: voteBatch.Source, Polls: []importer.PollRecord{record}})
					if err == nil && (len(result.Failed) != 0 || result.Written[importer.SectionPolls] != 1) {
						t.Errorf("poll import failed after waiting for vote: %+v", result)
					}
					changed <- err
				}()
			} else {
				label := "Changed"
				go func() {
					_, err := admin.PatchPollOption(t.Context(), p.ID, p.Options[2].ID, content.PollOptionPatch{Label: &label})
					changed <- err
				}()
			}
			blockedBy(t, voterPID)
			must(t, holder.Commit(t.Context()))
			must(t, <-voted)
			if name == "public_vote_before_import" {
				must(t, <-changed)
			} else {
				requireError(t, <-changed, 409, "conflict")
			}
			got, err := admin.GetPoll(t.Context(), p.ID)
			must(t, err)
			wantLabel := p.Options[2].Label
			if name == "public_vote_before_import" {
				wantLabel = "Changed"
			}
			if got.TotalVotes != 1 || got.Options[2].Label != wantLabel || got.Options[2].VoteCount != 1 {
				t.Fatalf("choice race changed voted choice or counts: %+v", got)
			}
		})
	}
}

// A live vote on the option an import recounts, cast just before the recount
// or while it is paused after its snapshot, must survive the recount.
func TestImportedVoteRecountKeepsLiveVote(t *testing.T) {
	w := newWorld(t, func(c *ck.Config) { c.Interactions.Limits.Disabled = true })
	a := w.apps[0]
	admin := a.engine.Client()
	qs := pgx.Identifier{w.schema}.Sanitize()
	blockedBy := func(t *testing.T, pid uint32) uint32 {
		t.Helper()
		var waiter uint32
		waitForWithin(t, 10*time.Second, func() bool {
			must(t, w.pool.QueryRow(t.Context(), `SELECT coalesce(min(pid),0) FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid))`, pid).Scan(&waiter))
			return waiter != 0
		})
		return waiter
	}
	queryOf := func(t *testing.T, pid uint32) string {
		t.Helper()
		var query string
		must(t, w.pool.QueryRow(t.Context(), "SELECT query FROM pg_stat_activity WHERE pid=$1", pid).Scan(&query))
		return query
	}
	for _, mode := range []string{"vote_during_recount", "vote_before_recount"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			_, token := w.person(t, 0)
			created := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
			record := importer.PollRecord{ExternalID: mode, Question: "Choose", Language: "en", CreatedAt: &created,
				Options: []importer.PollOptionRecord{{ExternalID: mode + "-one", Label: "One"}, {ExternalID: mode + "-two", Label: "Two", Position: 1}}}
			initial, err := admin.PostImport(ctx, importer.Batch{Source: "vote-recount-race", Polls: []importer.PollRecord{record}})
			must(t, err)
			if len(initial.Failed) != 0 {
				t.Fatalf("poll import fixture failed: %+v", initial)
			}
			p, err := admin.GetPoll(ctx, initial.IDs[importer.RecordPoll][record.ExternalID])
			must(t, err)
			option := p.Options[0].ID
			gate := w.schema + ":" + mode
			holder, err := w.pool.Acquire(ctx)
			must(t, err)
			defer holder.Release()
			_, err = holder.Exec(ctx, "SELECT pg_advisory_lock(hashtextextended($1,0))", gate)
			must(t, err)
			locked := true
			defer func() {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if locked {
					_, _ = holder.Exec(cleanupCtx, "SELECT pg_advisory_unlock(hashtextextended($1,0))", gate)
				}
				_, err := holder.Exec(cleanupCtx, `DROP TRIGGER IF EXISTS critical_vote_recount ON `+qs+`.content_poll_options; DROP FUNCTION IF EXISTS `+qs+`.critical_vote_recount()`)
				must(t, err)
			}()
			// The gate holds the transaction that sees its own vote on the poll:
			// the import votes by IP, the live voter by account.
			pause := `CREATE FUNCTION %[1]s.critical_vote_recount() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN
 IF EXISTS (SELECT 1 FROM %[1]s.content_poll_votes WHERE question_id = TG_ARGV[0]::uuid AND CASE TG_ARGV[2] WHEN 'ip' THEN ip IS NOT NULL ELSE user_id IS NOT NULL END) THEN
  PERFORM pg_advisory_xact_lock(hashtextextended(TG_ARGV[1],0));
 END IF;
 RETURN NULL; END$$;
`
			if mode == "vote_during_recount" {
				// A statement trigger fires after the recount UPDATE takes its
				// snapshot and before it touches a row.
				pause += `CREATE TRIGGER critical_vote_recount BEFORE UPDATE ON %[1]s.content_poll_options FOR EACH STATEMENT EXECUTE FUNCTION %[1]s.critical_vote_recount('%[2]s','%[3]s','ip')`
			} else {
				pause += `CREATE TRIGGER critical_vote_recount AFTER UPDATE ON %[1]s.content_poll_options FOR EACH ROW EXECUTE FUNCTION %[1]s.critical_vote_recount('%[2]s','%[3]s','user')`
			}
			_, err = w.pool.Exec(ctx, fmt.Sprintf(pause, qs, p.ID, gate))
			must(t, err)
			imported, live := make(chan error, 1), make(chan error, 1)
			runImport := func() {
				result, err := admin.PostImport(ctx, importer.Batch{Source: "vote-recount-race",
					Votes: []importer.VoteRecord{{Poll: importer.Ref{ID: p.ID}, Option: importer.Ref{ID: option}, IP: "198.51.100.70"}}})
				if err == nil && (len(result.Failed) != 0 || result.Written[importer.SectionVotes] != 1) {
					err = fmt.Errorf("vote import failed: %+v", result)
				}
				imported <- err
			}
			vote := func() {
				_, err := a.client(token).PostPollVote(ctx, p.ID, content.PollVote{OptionID: option})
				live <- err
			}
			if mode == "vote_during_recount" {
				go runImport()
				importPID := blockedBy(t, holder.Conn().PgConn().PID())
				if query := queryOf(t, importPID); !strings.Contains(query, "SET vote_count = n.votes") {
					t.Fatalf("import paused outside its vote recount: %s", query)
				}
				go vote()
				// The live vote either commits now or waits for the import's option lock.
				waitForWithin(t, 10*time.Second, func() bool {
					var waiting bool
					must(t, w.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, importPID).Scan(&waiting))
					return waiting || len(live) > 0
				})
			} else {
				go vote()
				livePID := blockedBy(t, holder.Conn().PgConn().PID())
				if query := queryOf(t, livePID); !strings.Contains(query, "vote_count = vote_count + 1") {
					t.Fatalf("live vote paused outside its count: %s", query)
				}
				go runImport()
				if query := queryOf(t, blockedBy(t, livePID)); !strings.Contains(query, "content_poll_options") {
					t.Fatalf("import waited outside its vote recount: %s", query)
				}
			}
			_, err = holder.Exec(ctx, "SELECT pg_advisory_unlock(hashtextextended($1,0))", gate)
			must(t, err)
			locked = false
			must(t, <-imported)
			must(t, <-live)
			got, err := admin.GetPoll(ctx, p.ID)
			must(t, err)
			var votes int
			must(t, w.pool.QueryRow(ctx, `SELECT count(*) FROM `+qs+`.content_poll_votes WHERE tenant_id=$1 AND option_id=$2`, a.cfg.Scope.AppID.String(), option).Scan(&votes))
			if votes != 2 || got.Options[0].VoteCount != 2 || got.TotalVotes != 2 {
				t.Fatalf("import recount lost a live vote: count=%d total=%d votes=%d; want 2", got.Options[0].VoteCount, got.TotalVotes, votes)
			}
		})
	}
}
