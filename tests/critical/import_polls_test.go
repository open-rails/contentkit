package critical_test

import (
	"reflect"
	"slices"
	"testing"
	"time"

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
	record := importer.PollRecord{ExternalID: "season", ID: contentref.NewID(), Question: "Choose a season", Language: "en", CreatedAt: &created,
		Options: []importer.PollOptionRecord{
			{ExternalID: "summer", ID: contentref.NewID(), Label: "Summer"},
			{ExternalID: "winter", ID: contentref.NewID(), Label: "Winter", Position: 1},
			{ExternalID: "spring", ID: contentref.NewID(), Label: "Spring", Position: 2},
		}}
	batch := importer.Batch{Source: "poll-choice-lock", Polls: []importer.PollRecord{record}}
	for range 2 {
		result, err := admin.PostImport(t.Context(), batch)
		must(t, err)
		if len(result.Failed) != 0 || result.Written[importer.SectionPolls] != 1 || result.IDs[importer.RecordPoll][record.ExternalID] != record.ID {
			t.Fatalf("initial or identical import failed: %+v", result)
		}
		for _, option := range record.Options {
			if result.IDs[importer.RecordOption][option.ExternalID] != option.ID {
				t.Fatalf("import changed option identity: %+v", result)
			}
		}
	}
	for _, label := range []string{"Autumn", "Spring"} {
		unvoted := record
		unvoted.Options = slices.Clone(record.Options)
		unvoted.Options[2].Label = label
		result, err := admin.PostImport(t.Context(), importer.Batch{Source: batch.Source, Polls: []importer.PollRecord{unvoted}})
		must(t, err)
		if len(result.Failed) != 0 || result.Written[importer.SectionPolls] != 1 {
			t.Fatalf("unvoted choices could not change: %+v", result)
		}
	}
	person, _ := w.person(t, 0)
	votes := importer.Batch{Source: batch.Source, Votes: []importer.VoteRecord{
		{Poll: importer.Ref{ExternalID: record.ExternalID}, Option: importer.Ref{ExternalID: record.Options[0].ExternalID}, UserID: person.ID, CreatedAt: &created},
		{Poll: importer.Ref{ExternalID: record.ExternalID}, Option: importer.Ref{ExternalID: record.Options[1].ExternalID}, IP: "203.0.113.40", CreatedAt: &created},
	}}
	for range 2 {
		result, err := admin.PostImport(t.Context(), votes)
		must(t, err)
		if len(result.Failed) != 0 || result.Written[importer.SectionVotes] != 2 {
			t.Fatalf("vote import failed: %+v", result)
		}
	}
	before, err := admin.GetPoll(t.Context(), record.ID)
	must(t, err)
	if before.TotalVotes != 2 || before.Options[0].VoteCount != 1 || before.Options[1].VoteCount != 1 {
		t.Fatalf("imported votes did not preserve counts: %+v", before)
	}
	var metadata string
	must(t, w.pool.QueryRow(t.Context(), `SELECT to_jsonb(q)::text FROM `+w.schema+`.content_poll_questions q WHERE tenant_id=$1 AND id=$2`,
		a.cfg.Scope.AppID.String(), record.ID).Scan(&metadata))
	for _, name := range []string{"add", "remove", "remove_all", "rename", "reorder", "replace_id"} {
		t.Run(name, func(t *testing.T) {
			changed := record
			changed.Options = slices.Clone(record.Options)
			changed.Question = "Must roll back"
			switch name {
			case "add":
				changed.Options = append(changed.Options, importer.PollOptionRecord{ExternalID: "autumn", Label: "Autumn", Position: 3})
			case "remove":
				changed.Options = changed.Options[:2]
			case "remove_all":
				changed.Options = nil
			case "rename":
				changed.Options[2].Label = "Autumn"
			case "reorder":
				changed.Options[0].Position, changed.Options[2].Position = 2, 0
			case "replace_id":
				changed.Options[2].ExternalID, changed.Options[2].ID = "replacement", contentref.NewID()
			}
			// A rejected record rolls back its metadata and new ID mappings;
			// another valid record in the same chunk still commits.
			sibling := importer.PollRecord{ExternalID: "sibling-" + name, Question: "Independent poll", Language: "en", Options: slices.Clone(record.Options)}
			for i := range sibling.Options {
				sibling.Options[i].ID = contentref.NewID()
				sibling.Options[i].ExternalID = sibling.ExternalID + sibling.Options[i].ExternalID
			}
			result, err := admin.PostImport(t.Context(), importer.Batch{Source: batch.Source, Polls: []importer.PollRecord{changed, sibling}})
			must(t, err)
			if len(result.Failed) != 1 || result.Failed[0].Code != importer.CodeConflict || result.Failed[0].ExternalID != record.ExternalID || result.Written[importer.SectionPolls] != 1 {
				t.Fatalf("choice change did not fail independently: %+v", result)
			}
			got, err := admin.GetPoll(t.Context(), record.ID)
			must(t, err)
			if !reflect.DeepEqual(got, before) {
				t.Fatalf("refused import changed poll or votes: before=%+v after=%+v", before, got)
			}
			var after string
			must(t, w.pool.QueryRow(t.Context(), `SELECT to_jsonb(q)::text FROM `+w.schema+`.content_poll_questions q WHERE tenant_id=$1 AND id=$2`,
				a.cfg.Scope.AppID.String(), record.ID).Scan(&after))
			if after != metadata {
				t.Fatalf("refused import changed metadata: before=%s after=%s", metadata, after)
			}
		})
	}
	identities, err := admin.PostImportExternalIds(t.Context(), importer.ExternalIDsQuery{Source: batch.Source, Lists: []importer.ExternalIDList{{Entity: importer.RecordOption}}})
	must(t, err)
	for _, identity := range identities.Lists[0].IDs {
		if identity.ExternalID == "autumn" || identity.ExternalID == "replacement" {
			t.Fatalf("refused import retained a new option identity: %+v", identity)
		}
	}
	// Array order is immaterial when option IDs, labels and positions match.
	slices.Reverse(record.Options)
	record.Question = "Updated question, unchanged choices"
	result, err := admin.PostImport(t.Context(), importer.Batch{Source: batch.Source, Polls: []importer.PollRecord{record}})
	must(t, err)
	if len(result.Failed) != 0 || result.Written[importer.SectionPolls] != 1 || result.IDs[importer.RecordPoll][record.ExternalID] != record.ID {
		t.Fatalf("identical voted choices did not reimport: %+v", result)
	}
	got, err := admin.GetPoll(t.Context(), record.ID)
	must(t, err)
	if got.Question != record.Question || got.TotalVotes != before.TotalVotes || !reflect.DeepEqual(got.Options, before.Options) {
		t.Fatalf("identical reimport changed choices or counts: %+v", got)
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
	for _, name := range []string{"public_vote_before_import", "imported_vote_before_staff_edit", "staff_delete_before_imported_vote"} {
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
					if err == nil && (len(result.Failed) != 1 || result.Failed[0].Code != importer.CodeConflict) {
						t.Errorf("poll import did not refuse after waiting for vote: %+v", result)
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
			if got.TotalVotes != 1 || got.Options[2].Label != p.Options[2].Label || got.Options[2].VoteCount != 1 {
				t.Fatalf("choice race changed voted choice or counts: %+v", got)
			}
		})
	}
}
