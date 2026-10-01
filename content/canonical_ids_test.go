package content

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
)

// foldingResolver finds content whatever the letter case of its id and keeps
// the requested reference, like a host resolving `WHERE id = $1::uuid`.
type foldingResolver map[string]bool

func (f foldingResolver) Resolve(_ context.Context, refs []contentref.ContentRef, _ access.Actor) (map[contentref.ContentKey]access.Resolution, error) {
	out := map[contentref.ContentKey]access.Resolution{}
	for _, r := range refs {
		if f[strings.ToLower(r.ContentID)] {
			out[r.Key()] = access.Resolution{Visible: true, Accessible: true}
		}
	}
	return out, nil
}

// spellings returns up to n distinct letter-case spellings of id, itself first.
func spellings(id string, n int) []string {
	var letters []int
	for i := range id {
		if id[i] >= 'a' && id[i] <= 'f' {
			letters = append(letters, i)
		}
	}
	var out []string
	for mask := 0; mask < 1<<len(letters) && len(out) < n; mask++ {
		b := []byte(id)
		for j, i := range letters {
			if mask&(1<<j) != 0 {
				b[i] -= 'a' - 'A'
			}
		}
		out = append(out, string(b))
	}
	return out
}

// Security audit 2026-10-01: liking a comment under several letter-case
// spellings of its id counted once per spelling, ranked first under sort=best,
// and EraseSubjects undid only the lower-case one.
func TestCaseSpellingsKeyOneReaction(t *testing.T) {
	const gallery = "0192abcd-ef01-7abc-8def-0123456789ab"
	rt, pool := newTestRuntime(t, Options{Resolver: foldingResolver{gallery: true}, ContentKinds: []string{"gallery"}})
	h, ctx := rt.Handler(), context.Background()
	attacker, anon := access.Actor{ID: "attacker"}, access.Actor{Anonymous: true, IP: "203.0.113.9"}
	fans := []access.Actor{{ID: "u1"}, {ID: "u2"}}

	comment := func(actor access.Actor, in createInput) Comment {
		t.Helper()
		rec := doJSON(t, h, actor, "POST", "/gallery/"+gallery+"/comments", in)
		var cm Comment
		if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &cm) != nil {
			t.Fatalf("comment: %d %s", rec.Code, rec.Body.String())
		}
		return cm
	}
	target, rival := comment(attacker, createInput{Body: "mine"}).ID, comment(fans[0], createInput{Body: "theirs"}).ID
	// Many hex letters, so the spellings are many distinct strings.
	const lettered = "0192abcd-ef01-7abc-8def-abcdefabcdef"
	if _, err := pool.Exec(ctx, `UPDATE `+rt.store.t.comments+` SET id = $1 WHERE id = $2`, lettered, target); err != nil {
		t.Fatal(err)
	}
	target = lettered

	like := func(actor access.Actor, path string) {
		t.Helper()
		if rec := doJSON(t, h, actor, "POST", path, nil); rec.Code != http.StatusOK {
			t.Fatalf("POST %s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
	for _, id := range spellings(target, 8) {
		like(attacker, "/comments/"+id+"/like")
	}
	for _, id := range spellings(rival, 8) {
		like(anon, "/comments/"+id+"/like")
	}
	for _, fan := range fans {
		like(fan, "/comments/"+rival+"/like")
	}
	like(attacker, "/gallery/"+gallery+"/like")
	if reply := comment(fans[1], createInput{Body: "re", ReplyToID: strings.ToUpper(rival)}); reply.ReplyToID != rival {
		t.Fatalf("reply_to_id = %q, want the stored %q", reply.ReplyToID, rival)
	}

	list := func(actor access.Actor) []Comment {
		t.Helper()
		rec := doJSON(t, h, actor, "GET", "/gallery/"+gallery+"/comments?sort=best", nil)
		var out []Comment
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
			t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
		}
		return out
	}
	got := list(attacker)
	if len(got) != 2 || got[0].ID != rival || got[0].Likes != 3 || got[1].ID != target || got[1].Likes != 1 || got[1].Mine != 1 {
		t.Fatalf("sort=best = %+v, want the rival (3 likes) over the target (1 like, mine)", got)
	}

	// A route id the resolver keeps is the row key, so it must be canonical.
	upper := strings.ToUpper(gallery)
	for _, route := range []string{"POST /gallery/" + upper + "/like", "GET /gallery/" + upper + "/reaction", "POST /gallery/" + upper + "/favorite",
		"DELETE /gallery/" + upper + "/favorite", "GET /gallery/" + upper + "/favorite", "POST /gallery/" + upper + "/comments", "GET /gallery/" + upper + "/comments"} {
		method, path, _ := strings.Cut(route, " ")
		rec := doJSON(t, h, attacker, method, path, createInput{Body: "x"})
		if rec.Code != http.StatusBadRequest || decodeErr(t, rec.Body.String()).Code != CodeInvalidRequest {
			t.Errorf("%s = %d %s, want 400 invalid_request", route, rec.Code, rec.Body.String())
		}
	}

	var spelled int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM `+rt.store.t.reactions+` WHERE content_id <> lower(content_id))
		+ (SELECT count(*) FROM `+rt.store.t.counts+` WHERE content_id <> lower(content_id))`).Scan(&spelled); err != nil || spelled != 0 {
		t.Fatalf("non-canonical keys stored: %d %v", spelled, err)
	}
	_, err := pool.Exec(ctx, `INSERT INTO `+rt.store.t.reactions+` (tenant_id, content_kind, content_id, user_id, value, revision) VALUES ($1, 'comment', $2, 'u9', 1, 1)`, rt.tenant, strings.ToUpper(target))
	if pgErr, ok := err.(interface{ SQLState() string }); !ok || pgErr.SQLState() != "23514" {
		t.Fatalf("upper-case content_id insert: want check violation, got %v", err)
	}

	if err := rt.EraseSubjects(ctx, []string{attacker.ID}); err != nil {
		t.Fatal(err)
	}
	got = list(fans[0])
	if len(got) != 2 || got[1].ID != target || got[1].Likes != 0 || got[0].Likes != 3 {
		t.Fatalf("after erasure = %+v, want the target at 0 likes and the rival at 3", got)
	}
	if c := countsOf(t, rt, ref(KindComment, target)); c.Likes != 0 {
		t.Fatalf("target rollup after erasure = %+v", c)
	}
	if c := countsOf(t, rt, ref("gallery", gallery)); c.Likes != 0 {
		t.Fatalf("gallery rollup after erasure = %+v", c)
	}
}
