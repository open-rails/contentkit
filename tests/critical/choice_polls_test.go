package critical_test

import (
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/iam"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/importer"
)

func TestChoicePolls(t *testing.T) {
	w := newWorld(t, func(c *ck.Config) { c.Interactions.Limits.Disabled = true })
	a := w.apps[0]
	admin := a.engine.Client()
	person, token := w.person(t, 0)
	_, otherToken := w.person(t, 0)
	voter, other := a.client(token), a.client(otherToken)
	input := content.PollInput{Question: "Choose a season", Language: "en", Options: []content.PollOptionInput{{Label: "Summer"}, {Label: "Winter", Position: 1}}}
	create := func(in content.PollInput) content.Poll {
		t.Helper()
		p, err := admin.PostPolls(t.Context(), in)
		must(t, err)
		return p
	}
	persona, err := w.auth.Persona("app")
	must(t, err)
	key, err := w.tenant[0].CreateAPIKey(t.Context(), iam.SystemIdentity(), a.group, iam.NewAPIKey{Name: "Poll contract staff", Role: persona.OwnerRole()})
	must(t, err)
	raw := func(path, body string, status int) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, a.server.URL+"/content/v1"+path, strings.NewReader(body))
		must(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+key.Secret)
		response, err := http.DefaultClient.Do(req)
		must(t, err)
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		must(t, err)
		if response.StatusCode != status {
			t.Fatalf("POST %s returned %d, want %d: %s", path, response.StatusCode, status, data)
		}
	}
	t.Run("CK-J30-S01_predefined_choices_reject_retired_writes", func(t *testing.T) {
		raw("/polls", `{"kind":"free_text","question":"Unsupported","language":"en"}`, 400)
		raw("/import", `{"source":"removed-answers","answers":[]}`, 400)
		_, err := admin.PostPolls(t.Context(), content.PollInput{Question: "Too few", Language: "en", Options: input.Options[:1]})
		requireError(t, err, 400, "invalid_request")
		_, err = voter.PostPolls(t.Context(), input)
		requireError(t, err, 403, "forbidden")
		p := create(input)
		if p.Kind != content.PollMultipleChoice || len(p.Options) != 2 || p.TotalVotes != 0 {
			t.Fatalf("invalid choice-poll creation: %+v", p)
		}
		raw("/polls/"+p.ID+"/answer", `{"text":"No longer supported"}`, 404)
		result, err := admin.PostImport(t.Context(), importer.Batch{Source: "choice-contract", Polls: []importer.PollRecord{{ExternalID: "unsupported", Kind: "free_text", Question: "Unsupported"}}})
		must(t, err)
		if len(result.Failed) != 1 || result.Written[importer.SectionPolls] != 0 {
			t.Fatalf("free-text import was not refused: %+v", result)
		}
	})
	t.Run("CK-J30-S01_duplicate_votes_scope_and_erasure_keep_counts", func(t *testing.T) {
		p, different := create(input), create(input)
		_, err := voter.PostPollVote(t.Context(), p.ID, content.PollVote{OptionID: different.Options[0].ID})
		requireError(t, err, 400, "invalid_request")
		_, err = w.apps[1].client(token).GetPoll(t.Context(), p.ID)
		requireError(t, err, 404, "not_found")
		_, err = w.apps[1].client(token).PostPollVote(t.Context(), p.ID, content.PollVote{OptionID: p.Options[0].ID})
		requireError(t, err, 404, "not_found")
		_, err = a.client("").PostPollVote(t.Context(), p.ID, content.PollVote{OptionID: p.Options[0].ID})
		requireError(t, err, 401, "unauthorized")
		var wg sync.WaitGroup
		errs := make(chan error, 8)
		for range 8 {
			wg.Go(func() {
				_, err := voter.PostPollVote(t.Context(), p.ID, content.PollVote{OptionID: p.Options[0].ID})
				errs <- err
			})
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			must(t, err)
		}
		got, err := voter.PostPollVote(t.Context(), p.ID, content.PollVote{OptionID: p.Options[1].ID})
		must(t, err)
		if got.TotalVotes != 1 || got.MyOption != p.Options[0].ID || got.Options[0].VoteCount != 1 || got.Options[1].VoteCount != 0 {
			t.Fatalf("duplicate/final vote changed counts: %+v", got)
		}
		var ip *string
		must(t, w.pool.QueryRow(t.Context(), `SELECT ip::text FROM `+w.schema+`.content_poll_votes WHERE tenant_id=$1 AND question_id=$2 AND user_id=$3`, a.cfg.Scope.AppID.String(), p.ID, person.ID).Scan(&ip))
		if ip != nil {
			t.Fatalf("signed-in vote stored an IP: %q", *ip)
		}
		_, err = other.PostPollVote(t.Context(), p.ID, content.PollVote{OptionID: p.Options[1].ID})
		must(t, err)
		for range 2 {
			must(t, a.engine.AnonymizeAccount(t.Context(), person.ID))
		}
		got, err = admin.GetPoll(t.Context(), p.ID)
		must(t, err)
		if got.TotalVotes != 2 || got.Options[0].VoteCount != 1 || got.Options[1].VoteCount != 1 {
			t.Fatalf("repeated account deletion lost or duplicated a vote: %+v", got)
		}
	})
	t.Run("CK-J30-S01_closed_scheduled_and_historical_polls_stay_inert", func(t *testing.T) {
		future := time.Now().Add(time.Hour)
		scheduled := input
		scheduled.LiveAt = &future
		p := create(scheduled)
		_, err := other.GetPoll(t.Context(), p.ID)
		requireError(t, err, 404, "not_found")
		_, err = other.PostPollVote(t.Context(), p.ID, content.PollVote{OptionID: p.Options[0].ID})
		requireError(t, err, 404, "not_found")
		p = create(input)
		inactive := false
		_, err = admin.PatchPoll(t.Context(), p.ID, content.PollUpdate{IsActive: &inactive})
		must(t, err)
		got, err := a.client("").GetPoll(t.Context(), p.ID)
		must(t, err)
		if !got.Closed {
			t.Fatalf("inactive poll is not closed: %+v", got)
		}
		_, err = other.PostPollVote(t.Context(), p.ID, content.PollVote{OptionID: p.Options[0].ID})
		requireError(t, err, 400, "invalid_request")
		past := time.Now().Add(-time.Minute)
		live := past.Add(-time.Hour)
		closed := input
		closed.LiveAt = &live
		closed.ClosesAt = &past
		p = create(closed)
		_, err = other.PostPollVote(t.Context(), p.ID, content.PollVote{OptionID: p.Options[0].ID})
		requireError(t, err, 400, "invalid_request")
		historical := uuid.NewString()
		_, err = w.pool.Exec(t.Context(), `INSERT INTO `+w.schema+`.content_poll_questions(id,tenant_id,kind,question,language) VALUES($1,$2,'free_text','Preserved history','en')`, historical, a.cfg.Scope.AppID.String())
		must(t, err)
		_, err = admin.GetPoll(t.Context(), historical)
		requireError(t, err, 404, "not_found")
		_, err = other.PostPollVote(t.Context(), historical, content.PollVote{OptionID: p.Options[0].ID})
		requireError(t, err, 404, "not_found")
		polls, err := admin.GetPollsAdmin(t.Context(), ck.GetPollsAdminQuery{})
		must(t, err)
		for _, poll := range polls {
			if poll.Kind != content.PollMultipleChoice || poll.ID == historical {
				t.Fatalf("historical unsupported poll escaped: %+v", poll)
			}
		}
		var kind string
		must(t, w.pool.QueryRow(t.Context(), `SELECT kind FROM `+w.schema+`.content_poll_questions WHERE id=$1`, historical).Scan(&kind))
		if kind != "free_text" {
			t.Fatal("historical poll was converted")
		}
	})
	t.Run("CK-J30-S01_close_must_follow_resulting_live_time", func(t *testing.T) {
		live := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
		closes := live.Add(time.Hour)
		scheduled := input
		scheduled.LiveAt, scheduled.ClosesAt = &live, &closes
		p := create(scheduled)
		later, earlier := closes.Add(time.Hour), live.Add(-time.Hour)
		for _, update := range []content.PollUpdate{
			{LiveAt: &later},
			{ClosesAt: content.Nullable[time.Time]{Set: true, Value: &earlier}},
			{LiveAt: &closes},
			{ClosesAt: content.Nullable[time.Time]{Set: true, Value: &live}},
		} {
			_, err := admin.PatchPoll(t.Context(), p.ID, update)
			requireError(t, err, 400, "invalid_request")
			if !strings.Contains(err.Error(), "closes_at must be after live_at") {
				t.Fatal(err)
			}
		}
		got, err := admin.GetPoll(t.Context(), p.ID)
		must(t, err)
		if !got.LiveAt.Equal(live) || got.ClosesAt == nil || !got.ClosesAt.Equal(closes) {
			t.Fatalf("invalid patch changed poll dates: %+v", got)
		}
		got, err = admin.PatchPoll(t.Context(), p.ID, content.PollUpdate{ClosesAt: content.Nullable[time.Time]{Set: true, Value: &later}})
		must(t, err)
		if got.ClosesAt == nil || !got.ClosesAt.Equal(later) {
			t.Fatalf("valid patch did not change closes_at: %+v", got)
		}
		got, err = admin.PatchPoll(t.Context(), p.ID, content.PollUpdate{LiveAt: &later, ClosesAt: content.Nullable[time.Time]{Set: true}})
		must(t, err)
		if !got.LiveAt.Equal(later) || got.ClosesAt != nil {
			t.Fatalf("clearing closes_at did not allow later live_at: %+v", got)
		}
		_, err = admin.PatchPoll(t.Context(), uuid.NewString(), content.PollUpdate{LiveAt: &later})
		requireError(t, err, 404, "not_found")
		past := time.Now().Add(-time.Minute)
		closed := input
		closed.ClosesAt = &past
		_, err = admin.PostPolls(t.Context(), closed)
		requireError(t, err, 400, "invalid_request")
	})
	t.Run("CK-J30-S01_poll_editor_limits", func(t *testing.T) {
		options := make([]content.PollOptionInput, 11)
		for i := range options {
			options[i] = content.PollOptionInput{Label: "Choice", Position: i}
		}
		for _, in := range []content.PollInput{
			{Question: input.Question, Language: "en", Options: options},
			{Question: strings.Repeat("界", 301), Language: "en", Options: input.Options},
			{Question: input.Question, Language: "unknown", Options: input.Options},
			{Question: input.Question, Options: input.Options},
			{Question: input.Question, Language: "en", Options: []content.PollOptionInput{{Label: strings.Repeat("界", 201)}, {Label: "No"}}},
		} {
			_, err := admin.PostPolls(t.Context(), in)
			requireError(t, err, 400, "invalid_request")
		}
		question, label := strings.Repeat("界", 300), strings.Repeat("界", 200)
		options[0].Label = " " + label + " "
		p := create(content.PollInput{Question: " " + question + " ", Language: "en", Options: options[:9]})
		if p.Question != question || p.Options[0].Label != label {
			t.Fatalf("poll text was not trimmed: %+v", p)
		}
		long := label + "界"
		_, err := admin.PostPollOptions(t.Context(), p.ID, content.PollOptionPatch{Label: &long})
		requireError(t, err, 400, "invalid_request")
		_, err = admin.PatchPollOption(t.Context(), p.ID, p.Options[0].ID, content.PollOptionPatch{Label: &long})
		requireError(t, err, 400, "invalid_request")
		_, err = admin.PostPollOptions(t.Context(), p.ID, content.PollOptionPatch{Label: &label})
		must(t, err)
		_, err = admin.PostPollOptions(t.Context(), p.ID, content.PollOptionPatch{Label: &label})
		requireError(t, err, 400, "invalid_request")
		question += "界"
		_, err = admin.PatchPoll(t.Context(), p.ID, content.PollUpdate{Question: &question})
		requireError(t, err, 400, "invalid_request")
	})
	t.Run("CK-J30-S01_vote_and_option_delete_serialize", func(t *testing.T) {
		three := input
		three.Options = append(slices.Clone(input.Options), content.PollOptionInput{Label: "Spring", Position: 2})
		tenant := a.cfg.Scope.AppID.String()
		racer, racerToken := w.person(t, 0)
		racing := a.client(racerToken)
		blocked := func(holder pgx.Tx) {
			t.Helper()
			waitForWithin(t, 10*time.Second, func() bool {
				var waiting bool
				must(t, w.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, holder.Conn().PgConn().PID()).Scan(&waiting))
				return waiting
			})
		}

		// Delete first: a vote waiting on it finds the option gone.
		p := create(three)
		holder, err := w.pool.Begin(t.Context())
		must(t, err)
		defer holder.Rollback(t.Context())
		_, err = holder.Exec(t.Context(), `SELECT 1 FROM `+w.schema+`.content_poll_questions WHERE tenant_id=$1 AND id=$2 FOR NO KEY UPDATE`, tenant, p.ID)
		must(t, err)
		_, err = holder.Exec(t.Context(), `DELETE FROM `+w.schema+`.content_poll_options WHERE tenant_id=$1 AND id=$2`, tenant, p.Options[2].ID)
		must(t, err)
		voted := make(chan error, 1)
		go func() {
			_, err := racing.PostPollVote(t.Context(), p.ID, content.PollVote{OptionID: p.Options[2].ID})
			voted <- err
		}()
		blocked(holder)
		must(t, holder.Commit(t.Context()))
		requireError(t, <-voted, 400, "invalid_request")

		// Vote first: the delete waits for it, then removes the option and its vote.
		p = create(three)
		holder, err = w.pool.Begin(t.Context())
		must(t, err)
		defer holder.Rollback(t.Context())
		_, err = holder.Exec(t.Context(), `SELECT 1 FROM `+w.schema+`.content_poll_questions WHERE tenant_id=$1 AND id=$2 FOR SHARE`, tenant, p.ID)
		must(t, err)
		deleted := make(chan error, 1)
		go func() { deleted <- admin.DeletePollOption(t.Context(), p.ID, p.Options[2].ID) }()
		blocked(holder)
		_, err = holder.Exec(t.Context(), `INSERT INTO `+w.schema+`.content_poll_votes(tenant_id, question_id, option_id, user_id) VALUES ($1, $2, $3, $4)`, tenant, p.ID, p.Options[2].ID, racer.ID)
		must(t, err)
		must(t, holder.Commit(t.Context()))
		must(t, <-deleted)
		var left int
		must(t, w.pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM `+w.schema+`.content_poll_options WHERE tenant_id=$1 AND id=$2) + (SELECT count(*) FROM `+w.schema+`.content_poll_votes WHERE tenant_id=$1 AND option_id=$2)`, tenant, p.Options[2].ID).Scan(&left))
		if left != 0 {
			t.Fatalf("deleted option or its vote survived: %d rows", left)
		}
	})
}

func TestAnonymousChoicePolls(t *testing.T) {
	w := newWorld(t, func(c *ck.Config) { c.Interactions.Anonymous.Votes = true; c.Interactions.Limits.Disabled = true })
	a := w.apps[0]
	poll, err := a.engine.Client().PostPolls(t.Context(), content.PollInput{Question: "Anonymous choice", Language: "en", Options: []content.PollOptionInput{{Label: "Yes"}, {Label: "No", Position: 1}}})
	must(t, err)
	for range 2 {
		got, err := a.client("").PostPollVote(t.Context(), poll.ID, content.PollVote{OptionID: poll.Options[0].ID})
		must(t, err)
		if !got.Voted || got.TotalVotes != 1 || got.Options[0].VoteCount != 1 {
			t.Fatalf("anonymous vote replay changed tally: %+v", got)
		}
	}
}
