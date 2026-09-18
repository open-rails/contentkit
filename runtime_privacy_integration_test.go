package contentkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/internal/signaltest"
	"github.com/open-rails/contentkit/signal"
)

type runtimeRetainingPolicy struct {
	down   bool
	fenced bool
}

func (*runtimeRetainingPolicy) Screen(context.Context, content.ModerationInput) (content.Verdict, error) {
	return content.Verdict{Decision: content.DecisionReview}, nil
}
func (*runtimeRetainingPolicy) Classify(context.Context, content.Answer) (content.GroupAssignment, error) {
	return content.GroupAssignment{GroupID: "g", Label: "Group"}, nil
}
func (p *runtimeRetainingPolicy) EraseSubjects(context.Context, string, []string) error {
	if p.down {
		return errors.New("provider unavailable")
	}
	p.fenced = true
	return nil
}

func TestRuntimeErasureIncludesPrivatePlaneIntegration(t *testing.T) {
	ctx := context.Background()
	pool := testPG(t)
	pgtest.EnsureExtensions(t, ctx, pool)
	env := signaltest.FromEnv(t)
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint("signal_enabled_", enabled), func(t *testing.T) {
			host := pgtest.EmptySchema(t, ctx, pool)
			searchSchema := pgtest.Schema(t, ctx, pool)
			db := stdlib.OpenDBFromPool(pool)
			if err := content.Migrate(ctx, db, host); err != nil {
				t.Fatal(err)
			}
			db.Close()
			var conn signal.Conn
			chDB := ""
			if enabled {
				chDB = fmt.Sprintf("ck_privacy_%d", time.Now().UnixNano())
				conn = env.Fresh(t, chDB)
			}
			provider := &runtimeRetainingPolicy{}
			rt, err := NewRuntime(ctx, RuntimeConfig{
				EmbeddedConfig: EmbeddedConfig{PG: pool, PGSchema: searchSchema, Tenant: testTenant, CH: conn, CHDatabase: chDB},
				Content:        content.Options{Schema: host, Identity: ctxIdentity{}, Authz: allowAuthz{}, Resolver: routeResolver{}, ContentKinds: []string{"gallery"}, Canonicalizer: content.ContentCanonicalizerFunc(stripLanguage), Moderator: provider, Classifier: provider, PrivateDataEraser: provider, Perms: content.Perms{PollWrite: "poll"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			actor := content.Actor{ID: "private-user"}
			pollResponse := do(t, rt.Handler(), actor, "POST", "/polls", map[string]string{"kind": "free_text", "question": "Question"})
			if pollResponse.Code != http.StatusCreated {
				t.Fatalf("poll: %d %s", pollResponse.Code, pollResponse.Body.String())
			}
			var poll struct{ ID string }
			if err := json.Unmarshal(pollResponse.Body.Bytes(), &poll); err != nil {
				t.Fatal(err)
			}
			for _, action := range []struct {
				path   string
				body   any
				status int
			}{
				{"/polls/" + poll.ID + "/answer", map[string]string{"text": "private answer"}, http.StatusOK},
				{"/gallery/42:en/comments", map[string]string{"body": "private held body"}, http.StatusAccepted},
				{"/gallery/42:en/like", nil, http.StatusOK},
			} {
				res := do(t, rt.Handler(), actor, "POST", action.path, action.body)
				if res.Code != action.status {
					t.Fatalf("%s = %d %s", action.path, res.Code, res.Body.String())
				}
			}
			if enabled {
				if _, err := rt.DeliverPreferences(ctx, content.PreferenceKey{}, 10, 0); err != nil {
					t.Fatal(err)
				}
			}
			// Validate the whole batch before touching any plane, even without CH.
			oversized := make([]signal.Subject, signal.MaxErasureSubjects+1)
			for i := range oversized {
				oversized[i] = signal.Subject{UserID: actor.ID}
			}
			for _, invalid := range [][]signal.Subject{
				{{UserID: actor.ID}, {UserID: actor.ID, AnonKey: "ambiguous"}}, oversized,
			} {
				report, err := rt.EraseSubjects(ctx, invalid)
				if err == nil || report.Complete() {
					t.Fatalf("invalid batch accepted: %+v %v", report, err)
				}
				for _, table := range []string{"social_poll_answers", "content_preference_snapshots"} {
					var n int
					if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+host+`.`+table+` WHERE actor_id=$1`, actor.ID).Scan(&n); err != nil || n != 1 {
						t.Fatalf("invalid batch mutated %s: %d %v", table, n, err)
					}
				}
				var fences int
				if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+host+`.content_private_subject_erasures`).Scan(&fences); err != nil || fences != 0 || provider.fenced {
					t.Fatalf("invalid batch crossed private/provider boundary: %d %v", fences, err)
				}
				if enabled {
					history, err := rt.History(ctx, signal.Subject{UserID: actor.ID}, signal.HistoryOptions{})
					if err != nil || len(history) != 1 {
						t.Fatalf("invalid batch mutated signal plane: %+v %v", history, err)
					}
				}
			}
			provider.down = true
			subjects := []signal.Subject{{UserID: " " + actor.ID + " "}}
			report, err := rt.EraseSubjects(ctx, subjects)
			if err == nil || report.Complete() {
				t.Fatalf("provider outage falsely completed runtime erasure: %+v %v", report, err)
			}
			for _, q := range []string{
				`SELECT count(*) FROM ` + host + `.social_poll_answers WHERE actor_id='private-user'`,
				`SELECT count(*) FROM ` + host + `.content_preference_snapshots WHERE actor_id='private-user'`,
				`SELECT count(*) FROM ` + host + `.social_comments WHERE body='private held body'`,
			} {
				var n int
				if err := pool.QueryRow(ctx, q).Scan(&n); err != nil || n != 0 {
					t.Fatalf("source erasure not durable: %d %v", n, err)
				}
			}
			res := do(t, rt.Handler(), actor, "POST", "/polls/"+poll.ID+"/answer", map[string]string{"text": "resurrection"})
			if res.Code != http.StatusForbidden {
				t.Fatalf("private source fence lost: %d", res.Code)
			}
			provider.down = false
			report, err = rt.EraseSubjects(ctx, subjects)
			if err != nil || !report.Complete() || !provider.fenced {
				t.Fatalf("runtime retry not complete: %+v %v", report, err)
			}
			if enabled {
				history, err := rt.History(ctx, subjects[0], signal.HistoryOptions{})
				if err != nil || len(history) != 0 {
					t.Fatalf("signal erasure missed: %+v %v", history, err)
				}
			}
		})
	}
}
