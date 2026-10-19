package e2e

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"

	contentkit "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/testauth"
	"github.com/open-rails/contentkit/media/versions"
)

// A real AuthKit JWT opens a claimed tier through the real catalog with no
// billing provider. Its download URL delegates the same narrowly scoped read;
// it must not lose the claim merely because a browser attachment has no bearer.
func TestArchivePreservesVerifiedTokenEntitlements(t *testing.T) {
	var held sync.Map
	w := newWorld(t, authtest.WithConfig(func(c *authkit.Config) {
		c.Token.EntitlementAllowlist = []string{"premium"}
	}), authtest.WithDeps(func(d *authkit.Deps) {
		d.Entitlements = func(_ context.Context, ids []string) (map[string][]string, error) {
			out := map[string][]string{}
			for _, id := range ids {
				if claims, ok := held.Load(id); ok {
					out[id] = claims.([]string)
				}
			}
			return out, nil
		}
	}))
	d := &downloads{w: w}
	_, d.staff = w.person(w.staff)
	premium, _ := w.person(iam.Role{})
	held.Store(premium.ID, []string{"premium"})
	d.premium = authtest.SignIn(t, w.ak, premium).AccessToken
	_, d.user = w.person(iam.Role{})
	d.f = serve(t, w.engine(func(c *contentkit.Config, deps *contentkit.Deps) {
		c.Content.TokenClaims = []string{"premium"}
		deps.Billing = nil
	}))
	c := d.content("book", "Token-only premium", access.Rule{Level: access.TierLevel, Tiers: []string{"premium"}})
	d.pages(c, 1)
	checkTokenArchive(t, d, c)
}

type archiveStaffOnly string

func (staff archiveStaffOnly) Can(_ context.Context, actor access.Actor, _ string) (bool, error) {
	return actor.Kind == "user" && actor.ID == string(staff), nil
}

func TestArchivePreservesLegacyApplicationAudience(t *testing.T) {
	w := newWorld(t)
	staff, _ := w.person(w.staff)
	app, plain := contentref.NewID(), contentref.NewID()
	d := &downloads{w: w, staff: "user:" + staff.ID, premium: "app:" + app, user: "user:" + plain}
	d.f = serve(t, w.engine(func(c *contentkit.Config, deps *contentkit.Deps) {
		c.Content.TokenClaims = []string{"premium"}
		deps.Billing, deps.Users = nil, nil
		deps.Auth = testauth.Auth{Authz: archiveStaffOnly(staff.ID), Claims: map[string][]string{app: {"premium"}}}
	}))
	c := d.content("book", "Application premium", access.Rule{Level: access.TierLevel, Tiers: []string{"premium"}})
	d.pages(c, 1)
	checkTokenArchive(t, d, c)
}

func checkTokenArchive(t *testing.T, d *downloads, c catalog.Content) {
	t.Helper()
	var detail catalog.Content
	d.f.want(http.StatusOK, d.premium, "GET", "/contents/"+c.ID, nil, &detail)
	if detail.Editor {
		t.Fatal("token-only reader accidentally has editor authority")
	}
	d.download(http.StatusForbidden, d.user, c)
	var grant versions.FileDownload
	path := "/contents/" + c.ID + "/versions/" + c.Versions[0].ID + "/download"
	d.f.want(http.StatusOK, d.premium, "GET", path, nil, &grant)
	if !strings.HasPrefix(grant.URL, "archive?") {
		t.Fatalf("reader did not receive an archive transfer: %s", grant.URL)
	}
	var first []byte
	for range 2 {
		response := d.f.call("", "GET", strings.TrimSuffix(path, "download")+grant.URL, nil)
		if response.Code != http.StatusOK {
			t.Fatalf("token-only attachment: status=%d body=%s", response.Code, response.Body.Bytes())
		}
		if len(entries(t, response.Body.Bytes())) != 1 {
			t.Fatal("token-only archive lost its page")
		}
		if first == nil {
			first = bytes.Clone(response.Body.Bytes())
		} else if !bytes.Equal(first, response.Body.Bytes()) {
			t.Fatal("cached token-only archive changed its bytes")
		}
	}
	q, err := url.ParseQuery(strings.TrimPrefix(grant.URL, "archive?"))
	if err != nil {
		t.Fatal(err)
	}
	q.Set("claims", `["premium","not-in-token"]`)
	response := d.f.call("", "GET", strings.TrimSuffix(path, "download")+"archive?"+q.Encode(), nil)
	if response.Code != http.StatusNotFound {
		t.Fatalf("tampered token claims: status=%d", response.Code)
	}
	q, _ = url.ParseQuery(strings.TrimPrefix(grant.URL, "archive?"))
	if q.Get("account_set") == "true" {
		q.Set("account_set", "false")
	} else {
		q.Set("account_set", "true")
	}
	response = d.f.call("", "GET", strings.TrimSuffix(path, "download")+"archive?"+q.Encode(), nil)
	if response.Code != http.StatusNotFound {
		t.Fatalf("tampered account membership presence: status=%d", response.Code)
	}
}
