package contentkit

import (
	"encoding/json"
	"testing"

	"github.com/open-rails/contentkit/media"
)

func TestScopeIDs(t *testing.T) {
	want := Scope{TenantID: NewTenantID(), AppID: NewAppID()}
	b, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got Scope
	if err := json.Unmarshal(b, &got); err != nil || got != want {
		t.Fatalf("scope JSON: %s, %+v, %v", b, got, err)
	}
	if err := got.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"", "doujins", "00000000-0000-0000-0000-000000000000", "AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA", "urn:uuid:" + want.TenantID.String()} {
		if _, err := ParseTenantID(invalid); err == nil {
			t.Errorf("accepted tenant ID %q", invalid)
		}
		if _, err := ParseAppID(invalid); err == nil {
			t.Errorf("accepted app ID %q", invalid)
		}
	}
	for _, scope := range []Scope{{}, {TenantID: want.TenantID}, {AppID: want.AppID}} {
		if scope.Validate() == nil {
			t.Errorf("accepted incomplete scope %+v", scope)
		}
	}
}

func TestScopeConfigurationPreservesCatalogKey(t *testing.T) {
	scope := Scope{TenantID: NewTenantID(), AppID: NewAppID()}
	for _, key := range []string{"", "doujins"} {
		cfg := Config{Scope: &scope, LegacyCatalogKey: key}
		got, bound, err := cfg.withScope()
		if err != nil || bound == nil || *bound != scope {
			t.Fatalf("resolve scope: %+v %+v %v", got, bound, err)
		}
		want := key
		if want == "" {
			want = scope.AppID.String()
		}
		if got.Tenant != want || got.Scope != nil || got.LegacyCatalogKey != "" {
			t.Fatalf("resolved config %+v, want catalog %s", got, want)
		}
		if cfg.Tenant != "" || cfg.Scope != &scope || cfg.LegacyCatalogKey != key {
			t.Fatal("resolution mutated caller config")
		}
		if _, _, err := got.withScope(); err != nil {
			t.Fatalf("normalization is not idempotent: %v", err)
		}
	}
	for _, cfg := range []Config{
		{Scope: &scope, Tenant: "doujins"},
		{LegacyCatalogKey: "doujins"},
		{Scope: &scope, LegacyCatalogKey: " doujins"},
		{Scope: &scope, LegacyCatalogKey: "app/catalog"},
		{Scope: &Scope{}},
	} {
		if _, _, err := cfg.withScope(); err == nil {
			t.Errorf("accepted invalid configuration %+v", cfg)
		}
	}
	legacy, bound, err := (Config{Tenant: "doujins"}).withScope()
	if err != nil || bound != nil || legacy.Tenant != "doujins" {
		t.Fatalf("legacy config changed: %+v %+v %v", legacy, bound, err)
	}
}

func TestScopeMediaConfigDerivesPaths(t *testing.T) {
	scope := Scope{TenantID: NewTenantID(), AppID: NewAppID()}
	cfg := Config{Scope: &scope, LegacyCatalogKey: "doujins", Media: &MediaConfig{Config: media.Config{Namespace: "doujins"}}}
	mc, err := cfg.MediaKinds()
	if err != nil || mc.Namespace != "" || mc.Scope.TenantID != scope.TenantID.String() || mc.Scope.AppID != scope.AppID.String() {
		t.Fatalf("scoped media owner: %q, %v", mc.Namespace, err)
	}
	if cfg.Tenant != "" || cfg.Scope != &scope {
		t.Fatal("MediaKinds mutated caller config")
	}
	bad := cfg
	badMedia := *cfg.Media
	badMedia.Namespace = "other"
	bad.Media = &badMedia
	if _, err := bad.MediaKinds(); err == nil {
		t.Fatal("cross-app legacy namespace accepted")
	}
	cfg.Media.LegacyLayout = true
	legacy, err := cfg.MediaKinds()
	if err != nil || legacy.Namespace != "doujins" || !legacy.Scope.IsZero() {
		t.Fatalf("legacy compatibility layout: %+v %v", legacy, err)
	}
}
