package image_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/avatar"
	"github.com/open-rails/contentkit/media/gateway"
	"github.com/open-rails/contentkit/media/image"
	"github.com/open-rails/contentkit/media/internal/s3test"
	"github.com/open-rails/contentkit/media/layout"
)

func TestScopedAccountDefaultsUseTenantRoot(t *testing.T) {
	env := s3test.Open(t)
	scope := layout.Scope{TenantID: uuid.NewString(), AppID: uuid.NewString()}
	kind := avatar.User()
	kind.Namespace = ""
	kind.Account = true
	reg, err := media.NewRegistry(media.Config{Scope: scope, Kinds: []media.Kind{kind}, Avatars: media.Avatars{Kind: "user", Preset: "avatar"}})
	if err != nil {
		t.Fatal(err)
	}
	pub, err := image.PublishDefaults(t.Context(), env.Store, reg)
	if err != nil {
		t.Fatal(err)
	}
	account := scope
	account.AppID = ""
	if len(pub.Written) == 0 {
		t.Fatal("no default assets")
	}
	for _, key := range pub.Written {
		if !strings.HasPrefix(key, account.Root()+"user/_default/public/") {
			t.Fatal(key)
		}
	}
	if repeat, err := image.PublishDefaults(t.Context(), env.Store, reg); err != nil || len(repeat.Written) != 0 {
		t.Fatalf("default publication not idempotent: %+v %v", repeat, err)
	}
	gw, err := gateway.New(gateway.Config{Endpoint: env.Config.Endpoint, Region: env.Config.Region, AccessKeyID: env.Config.AccessKeyID, SecretAccessKey: env.Config.SecretAccessKey, Routes: []gateway.Route{{Host: "media.test", Scope: account, Bucket: env.Config.Bucket}}, Defaults: pub.Defaults})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://media.test/v1/"+account.Prefix("user", "01920000-0000-7000-8000-000000000001", "")+"public/avatar-64.webp", nil))
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "image/webp" {
		t.Fatalf("scoped default: %d %s", rec.Code, rec.Body.String())
	}
}
