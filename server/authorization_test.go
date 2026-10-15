package server

import (
	"strings"
	"testing"

	"github.com/open-rails/authkit"
	contentkit "github.com/open-rails/contentkit"
)

func TestScopedAuthorizationConfig(t *testing.T) {
	scope := &contentkit.Scope{TenantID: contentkit.NewTenantID(), AppID: contentkit.NewAppID()}
	valid := Config{Engine: contentkit.Config{Scope: scope}, Authorization: &AuthorizationConfig{Issuer: "https://users.test", AppGroupID: "app"}}
	auth := authkit.Config{Token: authkit.TokenConfig{Issuer: "https://contentkit.test"}}
	if err := validateAuthorizationConfig(valid, auth); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Config, *authkit.Config){
		"missing binding":              func(c *Config, _ *authkit.Config) { c.Authorization = nil },
		"missing app group":            func(c *Config, _ *authkit.Config) { c.Authorization.AppGroupID = "" },
		"missing identity issuer":      func(c *Config, _ *authkit.Config) { c.Authorization.Issuer = "" },
		"missing permission authority": func(_ *Config, a *authkit.Config) { a.Token.Issuer = "" },
		"different directory":          func(c *Config, _ *authkit.Config) { c.UsersIssuer = "https://other.test" },
		"root remote declaration": func(_ *Config, a *authkit.Config) {
			a.RemoteApplications = []authkit.RemoteApplicationConfig{{Issuer: "https://users.test"}}
		},
		"binding without scope": func(c *Config, _ *authkit.Config) { c.Engine.Scope = nil },
	} {
		t.Run(name, func(t *testing.T) {
			cfg, ak := valid, auth
			binding := *valid.Authorization
			cfg.Authorization = &binding
			change(&cfg, &ak)
			if err := validateAuthorizationConfig(cfg, ak); err == nil || !strings.Contains(err.Error(), "server:") {
				t.Fatalf("invalid scoped authorization accepted: %v", err)
			}
		})
	}
}
