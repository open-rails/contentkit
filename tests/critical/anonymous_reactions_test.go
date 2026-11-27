package critical_test

import (
	"encoding/json"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/content"
	discovery "github.com/open-rails/contentkit/discovery"
	"github.com/open-rails/contentkit/internal/subject"
)

// loopbackProxies trusts the rig's loopback peer, so a request's
// X-Forwarded-For names the client address.
var loopbackProxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}

// forwardedBrowser is a signed-out browser presenting its anonymous id (none
// when empty) from ip, which the trusted loopback proxy forwards; an empty ip
// is the loopback peer itself.
type forwardedBrowser struct {
	anonymous, ip string
}

func (b forwardedBrowser) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header = req.Header.Clone()
	if b.anonymous != "" {
		req.Header.Set(subject.Header, b.anonymous)
	}
	if b.ip != "" {
		req.Header.Set("X-Forwarded-For", b.ip)
	}
	return http.DefaultTransport.RoundTrip(req)
}

func browserAt(a *application, anonymous, ip string) *ck.Client {
	return ck.NewClient(a.server.URL+"/content", &http.Client{Transport: forwardedBrowser{anonymous: anonymous, ip: ip}})
}

// refusedLike is a raw signed-out like's status and, when refused, its code
// and the limited action.
func refusedLike(t *testing.T, a *application, id, anonymous, ip string) (int, string, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, a.server.URL+"/content/v1/contents/"+id+"/like", nil)
	must(t, err)
	res, err := (&http.Client{Transport: forwardedBrowser{anonymous: anonymous, ip: ip}}).Do(req)
	must(t, err)
	defer res.Body.Close()
	var reply struct{ Code, Action string }
	_ = json.NewDecoder(res.Body).Decode(&reply)
	return res.StatusCode, reply.Code, reply.Action
}

// reactionRow is one stored reaction's person and value.
type reactionRow struct {
	user           *string
	subject, actor *uuid.UUID
	value          int16
}

func (x *analyticsWorld) reactionRows(t *testing.T, kind, id string) []reactionRow {
	t.Helper()
	rows, err := x.pool.Query(t.Context(), `SELECT user_id, anonymous_subject, anonymous_actor_id, value FROM `+x.schema+`.content_reactions
 WHERE tenant_id = $1 AND content_kind = $2 AND content_id = $3 ORDER BY created_at`, x.tenant(), kind, id)
	must(t, err)
	defer rows.Close()
	var out []reactionRow
	for rows.Next() {
		var r reactionRow
		must(t, rows.Scan(&r.user, &r.subject, &r.actor, &r.value))
		out = append(out, r)
	}
	must(t, rows.Err())
	return out
}

func counted(t *testing.T, got content.ReactionCounts, likes, dislikes int, mine int16) {
	t.Helper()
	if got.Likes == nil || got.Dislikes == nil || *got.Likes != likes || *got.Dislikes != dislikes || got.Mine != mine {
		t.Fatalf("counts %+v, want %d likes, %d dislikes, mine %d", got, likes, dislikes, mine)
	}
}

func TestAnonymousReactions(t *testing.T) {
	x := newAnalyticsWorld(t, func(cfg *ck.Config) {
		cfg.TrustedProxies = loopbackProxies
		cfg.Interactions.Limits.Reaction = []ck.Rate{{Count: 1000, Per: time.Minute}}
		cfg.Interactions.Limits.CommentReaction = []ck.Rate{{Count: 1000, Per: time.Minute}}
		cfg.Interactions.Limits.AnonymousReaction = []ck.Rate{{Count: 3, Per: time.Hour}}
	})
	a := x.a
	replaced, shared, limited, other := x.article(t, "Browser reaction", 4), x.article(t, "Shared address", 4), x.article(t, "Ballot limit", 4), x.article(t, "Ballot elsewhere", 4)
	folded, owned, expiring := x.article(t, "Folded reaction", 4), x.article(t, "Account reaction wins", 4), x.article(t, "Expiring reactions", 4)

	t.Run("CK-J41-S01_a_signed_out_reaction_is_its_browsers_and_replaces_its_own", func(t *testing.T) {
		id := uuid.NewString()
		who, _ := subject.Anonymous(x.app(), id)
		browser := browserAt(a, id, "")
		got, err := browser.PostContentLike(t.Context(), replaced.ID, ck.PostContentLikeQuery{})
		must(t, err)
		counted(t, got, 1, 0, 1)
		got, err = browser.PostContentDislike(t.Context(), replaced.ID, ck.PostContentDislikeQuery{})
		must(t, err)
		counted(t, got, 0, 1, -1)
		// The same browser from another address is the same person.
		got, err = browserAt(a, id, "198.51.100.7").PostContentLike(t.Context(), replaced.ID, ck.PostContentLikeQuery{})
		must(t, err)
		counted(t, got, 1, 0, 1)
		// Another browser at the address holds no reaction.
		got, err = browserAt(a, uuid.NewString(), "198.51.100.7").GetContentReaction(t.Context(), replaced.ID, ck.GetContentReactionQuery{})
		must(t, err)
		counted(t, got, 1, 0, 0)
		rows := x.reactionRows(t, "article", replaced.ID)
		if len(rows) != 1 || rows[0].user != nil || rows[0].actor != nil || rows[0].subject == nil || *rows[0].subject != who || rows[0].value != 1 {
			t.Fatalf("stored reactions %+v, want one keyed by the browser's subject", rows)
		}
		var ip bool
		must(t, x.pool.QueryRow(t.Context(), `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = $1 AND table_name = 'content_reactions' AND column_name = 'ip')`,
			x.world.schema).Scan(&ip))
		if ip {
			t.Fatal("content_reactions still stores an address")
		}
		// Without its anonymous id a signed-out reaction is refused.
		_, err = browserAt(a, "", "").PostContentLike(t.Context(), replaced.ID, ck.PostContentLikeQuery{})
		requireError(t, err, http.StatusBadRequest, "invalid_request")
	})

	t.Run("CK-J41-S02_two_browsers_behind_one_address_keep_their_own_reactions", func(t *testing.T) {
		one, two := browserAt(a, uuid.NewString(), ""), browserAt(a, uuid.NewString(), "")
		_, err := one.PostContentLike(t.Context(), shared.ID, ck.PostContentLikeQuery{})
		must(t, err)
		got, err := two.PostContentDislike(t.Context(), shared.ID, ck.PostContentDislikeQuery{})
		must(t, err)
		counted(t, got, 1, 1, -1)
		got, err = one.GetContentReaction(t.Context(), shared.ID, ck.GetContentReactionQuery{})
		must(t, err)
		counted(t, got, 1, 1, 1)
		// Comments too: each browser's like counts and reads back as its own.
		_, token := x.person(t, 0)
		cm := postComment(t, a.client(token), shared.ID, "Liked by two browsers")
		for _, b := range []*ck.Client{one, two} {
			_, err := b.PostCommentLike(t.Context(), cm.ID)
			must(t, err)
		}
		for _, b := range []*ck.Client{one, two} {
			list, err := b.GetContentComments(t.Context(), shared.ID, ck.GetContentCommentsQuery{})
			must(t, err)
			if len(list) != 1 || list[0].Likes != 2 || list[0].Mine != 1 {
				t.Fatalf("comment read by a browser: %+v", list)
			}
		}
	})

	t.Run("CK-J41-S03_the_per_address_ballot_limit_refuses_past_its_limit", func(t *testing.T) {
		browsers := []string{uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()}
		for _, id := range browsers[:3] {
			_, err := browserAt(a, id, "").PostContentLike(t.Context(), limited.ID, ck.PostContentLikeQuery{})
			must(t, err)
		}
		if status, code, action := refusedLike(t, a, limited.ID, browsers[3], ""); status != http.StatusTooManyRequests || code != "rate_limited" || action != string(content.ActionAnonymousReaction) {
			t.Fatalf("a fourth browser from the address: %d %s %s", status, code, action)
		}
		// A counted browser changing its own reaction spends nothing more.
		got, err := browserAt(a, browsers[0], "").PostContentDislike(t.Context(), limited.ID, ck.PostContentDislikeQuery{})
		must(t, err)
		counted(t, got, 2, 1, -1)
		// Another item, or another address, has its own allowance.
		_, err = browserAt(a, browsers[3], "").PostContentLike(t.Context(), other.ID, ck.PostContentLikeQuery{})
		must(t, err)
		got, err = browserAt(a, browsers[3], "198.51.100.9").PostContentLike(t.Context(), limited.ID, ck.PostContentLikeQuery{})
		must(t, err)
		counted(t, got, 3, 1, 1)
		// An account is never counted by it.
		_, token := x.person(t, 0)
		_, err = a.client(token).PostContentLike(t.Context(), limited.ID, ck.PostContentLikeQuery{})
		must(t, err)
	})

	t.Run("CK-J41-S04_sign_in_moves_the_browsers_reaction_to_the_account", func(t *testing.T) {
		mineID, neighbour := uuid.NewString(), browserAt(a, uuid.NewString(), "")
		mine := browserAt(a, mineID, "")
		anon, _ := subject.Anonymous(x.app(), mineID)
		_, err := mine.PostContentLike(t.Context(), folded.ID, ck.PostContentLikeQuery{})
		must(t, err)
		_, err = neighbour.PostContentDislike(t.Context(), folded.ID, ck.PostContentDislikeQuery{})
		must(t, err)
		_, err = mine.PostContentLike(t.Context(), owned.ID, ck.PostContentLikeQuery{})
		must(t, err)
		user, token := x.person(t, 0)
		member := x.reader(t, token, "")
		_, err = member.PostContentDislike(t.Context(), owned.ID, ck.PostContentDislikeQuery{})
		must(t, err)
		must(t, member.PostMeAnonymousLink(t.Context(), discovery.AnonymousLink{AnonymousID: mineID}))
		x.runJob(t)
		cards, err := member.GetContentCards(t.Context(), ck.GetContentCardsQuery{Ids: []string{folded.ID, owned.ID}})
		must(t, err)
		reaction := map[string]int{}
		for _, c := range cards.Contents {
			if c.Viewer != nil {
				reaction[c.ID] = c.Viewer.Reaction
			}
		}
		if reaction[folded.ID] != 1 || reaction[owned.ID] != -1 {
			t.Fatalf("the account's reactions after the fold: %v", reaction)
		}
		got, err := neighbour.GetContentReaction(t.Context(), folded.ID, ck.GetContentReactionQuery{})
		must(t, err)
		counted(t, got, 1, 1, -1)
		// The account's own reaction wins; the browser's keeps counting, unlinked.
		got, err = member.GetContentReaction(t.Context(), owned.ID, ck.GetContentReactionQuery{})
		must(t, err)
		counted(t, got, 1, 1, -1)
		for _, item := range []string{folded.ID, owned.ID} {
			for _, r := range x.reactionRows(t, "article", item) {
				if r.subject != nil && *r.subject == anon {
					t.Fatalf("a reaction stayed with the folded browser: %+v", r)
				}
				if r.user != nil && !strings.EqualFold(*r.user, user.ID) {
					t.Fatalf("a reaction moved to another account: %+v", r)
				}
			}
		}
	})

	t.Run("CK-J41-S05_expiry_keeps_public_counts", func(t *testing.T) {
		ids := []string{uuid.NewString(), uuid.NewString()}
		for _, id := range ids {
			_, err := browserAt(a, id, "").PostContentLike(t.Context(), expiring.ID, ck.PostContentLikeQuery{})
			must(t, err)
		}
		before := x.likes(t, expiring.ID)
		x.advance(31 * 24 * time.Hour)
		x.runContentJob(t, expiryJob)
		if after := x.likes(t, expiring.ID); before != 2 || after != before {
			t.Fatalf("likes %d before expiry, %d after", before, after)
		}
		rows := x.reactionRows(t, "article", expiring.ID)
		if len(rows) != 2 {
			t.Fatalf("expiry removed reactions: %+v", rows)
		}
		for _, r := range rows {
			if r.subject != nil || r.actor == nil || r.value != 1 {
				t.Fatalf("an expired browser's reaction %+v, want it unlinked and counted", r)
			}
		}
		// Counts recomputed from the rows agree with the public ones.
		var recount int
		must(t, x.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+x.schema+`.content_reactions WHERE tenant_id = $1 AND content_kind = 'article' AND content_id = $2 AND value = 1 AND NOT author_hidden`,
			x.tenant(), expiring.ID).Scan(&recount))
		if recount != before {
			t.Fatalf("a recount from the rows finds %d likes, the public count is %d", recount, before)
		}
		// A browser returning after expiry starts fresh.
		got, err := browserAt(a, ids[0], "").GetContentReaction(t.Context(), expiring.ID, ck.GetContentReactionQuery{})
		must(t, err)
		counted(t, got, 2, 0, 0)
	})
}
