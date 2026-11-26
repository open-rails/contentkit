package critical_test

import (
	"testing"

	"github.com/google/uuid"
	mediastore "github.com/open-rails/contentkit/internal/mediastore"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/avatar"
	"github.com/open-rails/contentkit/media/layout"
)

// The bucket is the tenancy boundary: keys name no tenant or app, account
// media sits under _accounts/, and no kind takes a reserved top-level name.
func TestBucketKeyLayout(t *testing.T) {
	gallery := media.Kind{Name: "gallery", Uploads: []media.Upload{{Path: "cover", Types: []string{"image/png"}, MaxBytes: 1 << 20}},
		Public: []media.Public{{Name: "cover", From: "cover", To: "cover-{w}.webp", Widths: []int{32}}}}
	t.Run("CK-J13-S10_items_are_kind_and_id_in_the_app_bucket", func(t *testing.T) {
		scope := newMediaScope()
		reg, err := mediastore.NewRegistry(media.Config{Scope: scope, Kinds: []media.Kind{gallery, avatar.User()}}, mediastore.Hooks{})
		must(t, err)
		id, account := uuid.Must(uuid.NewV7()).String(), uuid.NewString()
		ref, err := reg.Ref("gallery", id)
		must(t, err)
		item, err := reg.Item(ref)
		must(t, err)
		if item.Prefix() != "gallery/"+id+"/" || ref.TenantID != scope.Partition() {
			t.Fatalf("app item at %q in %q", item.Prefix(), ref.TenantID)
		}
		ref, err = reg.Ref("user", account)
		must(t, err)
		item, err = reg.Item(ref)
		must(t, err)
		if item.Prefix() != "_accounts/user/"+account+"/" || ref.TenantID != (layout.Scope{TenantID: scope.TenantID}).Partition() {
			t.Fatalf("account item at %q in %q", item.Prefix(), ref.TenantID)
		}
		roots := map[string]string{}
		for _, rule := range reg.PresetRules() {
			roots[rule.Kind] = rule.Root
		}
		if roots["gallery"] != "" || roots["user"] != layout.AccountRoot {
			t.Fatalf("preset roots %v", roots)
		}
		hash := layout.Hash(make([]byte, 32))
		for key, want := range map[string]bool{
			"gallery/" + id + "/public/" + hash:                                                       true,
			"_accounts/user/" + account + "/manifest.json":                                            true,
			"tenant/" + scope.TenantID + "/app/" + scope.AppID + "/gallery/" + id + "/public/" + hash: false,
			"tenant/" + scope.TenantID + "/account/user/" + account + "/manifest.json":                false,
			"_probe/x/manifest.json":                                                                  false,
			"_accounts/_accounts/" + id + "/manifest.json":                                            false,
			"work/" + id + "/manifest.json":                                                           false,
		} {
			if k, ok := layout.Parse(key); ok != want || ok && k.Folder()+layout.ManifestName != key && k.Folder()+k.Area+"/"+k.Name != key {
				t.Errorf("Parse(%q) = %+v, %v; want %v", key, k, ok, want)
			}
		}
	})
	t.Run("CK-J13-S10_registries_refuse_reserved_kinds_and_a_missing_scope", func(t *testing.T) {
		for _, name := range []string{"_accounts", "_probe", "work"} {
			kind := gallery
			kind.Name = name
			if _, err := mediastore.NewRegistry(media.Config{Scope: newMediaScope(), Kinds: []media.Kind{kind}}, mediastore.Hooks{}); err == nil {
				t.Errorf("kind %q was accepted", name)
			}
		}
		for _, scope := range []layout.Scope{{}, {TenantID: uuid.NewString()}} {
			if _, err := mediastore.NewRegistry(media.Config{Scope: scope, Kinds: []media.Kind{gallery}}, mediastore.Hooks{}); err == nil {
				t.Errorf("scope %+v was accepted", scope)
			}
		}
	})
}
