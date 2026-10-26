package critical_test

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/iam"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/importer"
	"github.com/open-rails/contentkit/signal"
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
		_, err := admin.PostPolls(t.Context(), content.PollInput{Question: "Too few", Options: input.Options[:1]})
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
		_, err = other.PostPollVote(t.Context(), p.ID, content.PollVote{OptionID: p.Options[1].ID})
		must(t, err)
		for range 2 {
			report, err := a.engine.EraseAppSubjects(t.Context(), []signal.Subject{{UserID: person.ID}})
			must(t, err)
			if !report.Complete() {
				t.Fatalf("incomplete erasure: %+v", report)
			}
		}
		got, err = admin.GetPoll(t.Context(), p.ID)
		must(t, err)
		if got.TotalVotes != 1 || got.Options[0].VoteCount != 0 || got.Options[1].VoteCount != 1 {
			t.Fatalf("repeated erasure damaged another vote: %+v", got)
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
		_, err = other.PostPollVote(t.Context(), p.ID, content.PollVote{OptionID: p.Options[0].ID})
		requireError(t, err, 400, "invalid_request")
		past := time.Now().Add(-time.Minute)
		closed := input
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
}

func TestAnonymousChoicePolls(t *testing.T) {
	w := newWorld(t, func(c *ck.Config) { c.Interactions.Anonymous.Votes = true; c.Interactions.Limits.Disabled = true })
	a := w.apps[0]
	poll, err := a.engine.Client().PostPolls(t.Context(), content.PollInput{Question: "Anonymous choice", Options: []content.PollOptionInput{{Label: "Yes"}, {Label: "No", Position: 1}}})
	must(t, err)
	for range 2 {
		got, err := a.client("").PostPollVote(t.Context(), poll.ID, content.PollVote{OptionID: poll.Options[0].ID})
		must(t, err)
		if !got.Voted || got.TotalVotes != 1 || got.Options[0].VoteCount != 1 {
			t.Fatalf("anonymous vote replay changed tally: %+v", got)
		}
	}
}
