package content

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMediaForeignTenantCannotOverwriteObjects(t *testing.T) {
	ctx := context.Background()
	media := &fakeMedia{}
	a, pool := newTestRuntime(t, Options{Tenant: "site_a", Media: media, Perms: Perms{PostWrite: "post", PollWrite: "poll"}})
	b, err := New(ctx, Options{Pool: pool, Schema: a.schema, Tenant: "site_b", Identity: &fakeIdentity{}, Authz: allowAll{}, Resolver: &fakeResolver{}, Media: media, Perms: Perms{PostWrite: "post", PollWrite: "poll"}})
	if err != nil {
		t.Fatal(err)
	}
	post := insertPost(t, a)
	poll, err := a.polls.create(ctx, Actor{ID: "admin"}, createPollInput{Question: "Q", Options: []createOptionInput{{Label: "a"}, {Label: "b"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ path, key string }{
		{"/posts/" + post + "/cover", "posts/" + post + "/cover.png"},
		{"/polls/" + poll.ID + "/image", "polls/" + poll.ID + ".png"},
		{"/polls/options/" + poll.Options[0].ID + "/image", "polls/options/" + poll.Options[0].ID + ".png"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			_, err := media.Put(ctx, tc.key, []byte("original"), "image/png")
			if err != nil {
				t.Fatal(err)
			}
			req := multipartUpload(t, "POST", tc.path, []byte("attacker"))
			req = req.WithContext(withActor(req.Context(), Actor{ID: "other-tenant-admin"}))
			rec := httptest.NewRecorder()
			b.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status=%d", rec.Code)
			}
			got, _ := media.stored(tc.key)
			if string(got) != "original" {
				t.Fatalf("foreign tenant upload overwrote object before returning 404: %q", got)
			}
		})
	}
}
