package content

import (
	"github.com/google/uuid"
	"testing"

	"github.com/open-rails/contentkit/access"
)

func TestAccountAnonymizationPreservesPollVotes(t *testing.T) {
	rt := newPreferenceRuntime(t)
	ctx := t.Context()
	poll, err := rt.polls.create(ctx, pollAdmin, PollInput{Question: "keep these votes", Options: []PollOptionInput{{Label: "yes"}, {Label: "no"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"alice", "bob"} {
		anonymous := uuid.NewString()
		if _, err := rt.polls.vote(ctx, access.Actor{ID: id}, poll.ID, poll.Options[0].ID); err != nil {
			t.Fatal(err)
		}
		if err := rt.AnonymizeAccount(ctx, id, anonymous); err != nil {
			t.Fatal(err)
		}
		if err := rt.AnonymizeAccount(ctx, id, anonymous); err != nil {
			t.Fatal(err)
		}
	}
	var votes, total int
	if err := rt.store.pool.QueryRow(ctx, `SELECT count(*) FROM `+rt.store.t.pollVotes+` WHERE tenant_id=$1 AND question_id=$2 AND user_id IS NULL AND ip IS NULL AND anonymous_actor_id IS NOT NULL`, rt.tenant, poll.ID).Scan(&votes); err != nil || votes != 2 {
		t.Fatalf("anonymous poll votes=%d err=%v", votes, err)
	}
	if err := rt.store.pool.QueryRow(ctx, `SELECT vote_count FROM `+rt.store.t.pollOptions+` WHERE tenant_id=$1 AND id=$2`, rt.tenant, poll.Options[0].ID).Scan(&total); err != nil || total != 2 {
		t.Fatalf("poll count=%d err=%v", total, err)
	}
}
