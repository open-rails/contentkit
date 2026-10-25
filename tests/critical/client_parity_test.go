package critical_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/open-rails/authkit/iam"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
)

func TestClientEquivalentStaffOperations(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	persona, err := w.auth.Persona("app")
	must(t, err)
	key, err := w.tenant[0].CreateAPIKey(t.Context(), iam.SystemIdentity(), a.group, iam.NewAPIKey{Name: "Equivalent staff client", Role: persona.OwnerRole()})
	must(t, err)
	embedded, remote := a.engine.Client(), a.client(key.Secret)
	t.Run("CK-J29-S01_embedded_host_and_remote_staff_match_shared_operations", func(t *testing.T) {
		// Host and organization key are both non-human staff. Compare operations
		// for which their effective authority is identical; neither is a user.
		item, err := embedded.PostContents(t.Context(), catalog.ContentInput{Kind: "article", Text: map[string]catalog.Text{"en": {Title: "Shared client result", Body: "Draft body"}}})
		must(t, err)
		local, err := embedded.GetContent(t.Context(), item.ID, ck.GetContentQuery{})
		must(t, err)
		wire, err := remote.GetContent(t.Context(), item.ID, ck.GetContentQuery{})
		must(t, err)
		if !reflect.DeepEqual(local, wire) {
			t.Fatalf("equivalent staff reads diverged: local=%+v remote=%+v", local, wire)
		}
		title := "Remote update visible in process"
		_, err = remote.PatchContent(t.Context(), item.ID, catalog.ContentPatch{OriginalTitle: &title})
		must(t, err)
		local, err = embedded.GetContent(t.Context(), item.ID, ck.GetContentQuery{})
		must(t, err)
		if local.OriginalTitle != title {
			t.Fatal("remote mutation was not shared with embedded reader")
		}
		for _, client := range []*ck.Client{embedded, remote} {
			_, err := client.PostContents(t.Context(), catalog.ContentInput{Kind: "not-a-configured-kind"})
			requireError(t, err, 400, "invalid_request")
			_, err = client.GetMeFavorites(t.Context(), ck.GetMeFavoritesQuery{})
			requireError(t, err, 403, "forbidden")
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			_, err = client.GetKinds(ctx)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled client request changed error identity: %v", err)
			}
		}
	})
	t.Run("CK-J29-S07_mount_prefix_and_app_key_cannot_cross_authority", func(t *testing.T) {
		mux := http.NewServeMux()
		for _, mount := range []struct {
			app    *application
			prefix string
		}{{a, "/nested/first"}, {w.apps[1], "/nested/second"}} {
			routes, err := mount.app.engine.Routes(ck.Routes{Prefix: mount.prefix, Groups: mount.app.engine.RouteGroups()})
			must(t, err)
			for _, route := range routes {
				mux.Handle(route.Path, route.Handler)
			}
		}
		server := httptest.NewServer(mux)
		defer server.Close()
		transport := &http.Client{Transport: bearerTransport{key.Secret, http.DefaultTransport}}
		first := ck.NewClient(server.URL+"/nested/first/", transport)
		second := ck.NewClient(server.URL+"/nested/second", transport)
		before, err := first.GetManage(t.Context(), ck.GetManageQuery{})
		must(t, err)
		_, err = second.GetManage(t.Context(), ck.GetManageQuery{})
		requireError(t, err, 403, "forbidden")
		for _, id := range []string{"../second/v1/manage", "/nested/second/v1/manage", "https://foreign.invalid/v1/manage"} {
			_, err := first.GetContent(t.Context(), id, ck.GetContentQuery{})
			requireError(t, err, 404, "not_found")
		}
		after, err := first.GetManage(t.Context(), ck.GetManageQuery{})
		must(t, err)
		if len(before) != len(after) {
			t.Fatal("rejected mount probes changed app state")
		}
		_, err = ck.NewClient(server.URL+"/wrong", transport).GetKinds(t.Context())
		requireError(t, err, 404, "")
	})
}
