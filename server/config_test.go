package server

import (
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/open-rails/authkit"

	contentkit "github.com/open-rails/contentkit"
)

func TestLoadTransport(t *testing.T) {
	c, err := Load([]byte(`
engine:
  trusted_proxies: [10.0.0.0/8, "fd00::/8"]
  clickhouse: {tls: true, ca_file: /etc/clickhouse/ca.pem}
read_timeout: 30s
idle_timeout: 2m
`), func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	want := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("fd00::/8")}
	if !slices.Equal(c.Engine.TrustedProxies, want) || !c.Engine.ClickHouse.TLS || c.Engine.ClickHouse.CAFile != "/etc/clickhouse/ca.pem" ||
		c.ReadTimeout != 30*time.Second || c.IdleTimeout != 2*time.Minute {
		t.Fatalf("loaded %+v", c)
	}
	if _, err := Load([]byte("engine: {trusted_proxies: [not-a-prefix]}"), func(string) string { return "" }); err == nil {
		t.Fatal("a malformed prefix loaded")
	}
}

func TestLoadAuthAndBilling(t *testing.T) {
	const merchant = "0195f0c4-7a3e-7d2b-9c1a-3f4e5d6c7b8a"
	c, err := Load([]byte(`
auth:
  issuer: https://contentkit.example.test
  resource: {id: https://contentkit.example.test}
auth_keys: /vault/auth
openrails: {base_url: https://openrails.example, merchant_id: `+merchant+`}
`), func(v string) string {
		if v == "CONTENTKIT_OPENRAILS_API_KEY" {
			return "merchant-key"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.Auth.Issuer != "https://contentkit.example.test" || c.Auth.Resource.ID != "https://contentkit.example.test" || c.AuthKeys != "/vault/auth" ||
		c.OpenRails.MerchantID.String() != merchant || c.OpenRails.APIKey != "merchant-key" {
		t.Fatalf("loaded %+v", c)
	}
	for _, removed := range []string{"auth: {token: {issuer: https://contentkit.example.test}}", "auth: {keys: {path: /vault/auth}}",
		"openrails: {merchant_id: not-a-uuid}"} {
		if _, err := Load([]byte(removed), func(string) string { return "" }); err == nil {
			t.Errorf("%s loaded", removed)
		}
	}
}

func TestSecureEndpoint(t *testing.T) {
	for raw, ok := range map[string]bool{"https://openrails.example": true, "http://127.0.0.1:8080": true, "http://localhost": true, "http://[::1]:1": true,
		"http://openrails.example": false, "ftp://openrails.example": false, "openrails.example": false} {
		if err := secureEndpoint(raw); (err == nil) != ok {
			t.Errorf("secureEndpoint(%q) = %v", raw, err)
		}
	}
}

func TestAuthClientIP(t *testing.T) {
	proxies := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("173.245.48.0/20")}
	var h authkit.HTTPConfig
	if err := authClientIP(&h, nil); err != nil || !h.DirectPeerIP || len(h.TrustedProxies) != 0 {
		t.Fatalf("no proxy is the direct peer: %+v, %v", h, err)
	}
	h = authkit.HTTPConfig{}
	if err := authClientIP(&h, proxies); err != nil || h.DirectPeerIP || !slices.Equal(h.TrustedProxies, []string{"10.0.0.0/8", "173.245.48.0/20"}) {
		t.Fatalf("the engine's proxies are AuthKit's: %+v, %v", h, err)
	}
	h = authkit.HTTPConfig{TrustedProxies: []string{"10.0.0.0/8"}, CloudflareProxies: []string{"173.245.48.0/20"}}
	if err := authClientIP(&h, proxies); err != nil {
		t.Fatalf("a Cloudflare split of the same proxies is one posture: %v", err)
	}
	for name, c := range map[string]struct {
		h       authkit.HTTPConfig
		trusted []netip.Prefix
	}{
		"direct peer beside engine proxies":  {authkit.HTTPConfig{DirectPeerIP: true}, proxies},
		"direct peer beside its own proxies": {authkit.HTTPConfig{DirectPeerIP: true, TrustedProxies: []string{"10.0.0.0/8"}}, nil},
		"proxies the engine lacks":           {authkit.HTTPConfig{TrustedProxies: []string{"10.0.0.0/8"}}, nil},
		"other proxies":                      {authkit.HTTPConfig{TrustedProxies: []string{"192.168.0.0/16"}}, proxies},
		"a malformed proxy":                  {authkit.HTTPConfig{TrustedProxies: []string{"10.0.0.0"}}, proxies},
	} {
		if err := authClientIP(&c.h, c.trusted); err == nil {
			t.Errorf("%s: want refused", name)
		}
	}
}

func TestDistinctSchemas(t *testing.T) {
	engine := contentkit.Config{Scope: &contentkit.Scope{TenantID: contentkit.NewTenantID(), AppID: contentkit.NewAppID()}, Database: contentkit.DatabaseConfig{Schema: "site"}}
	for _, ok := range []authkit.DatabaseConfig{{Schema: "auth", RiverSchema: "auth_river"}, {Schema: "auth", RiverSchema: "auth"}} {
		if err := distinctSchemas(engine, ok); err != nil {
			t.Errorf("%+v: %v", ok, err)
		}
	}
	for _, bad := range []authkit.DatabaseConfig{{Schema: "site", RiverSchema: "auth_river"}, {Schema: "auth", RiverSchema: "site_river"},
		{Schema: "auth", RiverSchema: "site"}, {Schema: "contentkit_accounts", RiverSchema: "auth"}} {
		if err := distinctSchemas(engine, bad); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}
