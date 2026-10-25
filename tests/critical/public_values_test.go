package critical_test

import (
	"encoding/json"
	"strings"
	"testing"

	ck "github.com/open-rails/contentkit"
)

// These are the approved public value contracts used in deployment files.
// Invalid input must not silently change a previously bound identity.
func TestPublicScopeValues(t *testing.T) {
	const tenantText = "af985306-d261-4499-8c67-eccdcde5612e"
	const appText = "b0b00467-0811-4d99-b790-46854551eb3e"
	tenant, err := ck.ParseTenantID(tenantText)
	must(t, err)
	app, err := ck.ParseAppID(appText)
	must(t, err)
	t.Run("canonical_config_round_trip", func(t *testing.T) {
		if tenant.IsZero() || app.IsZero() || tenant.String() != tenantText || app.String() != appText {
			t.Fatal("canonical identity changed")
		}
		tenantBytes, err := tenant.MarshalText()
		must(t, err)
		appBytes, err := app.MarshalText()
		must(t, err)
		var tenantAgain ck.TenantID
		var appAgain ck.AppID
		must(t, tenantAgain.UnmarshalText(tenantBytes))
		must(t, appAgain.UnmarshalText(appBytes))
		if tenantAgain != tenant || appAgain != app {
			t.Fatal("text round trip changed identity")
		}
		scope := ck.Scope{TenantID: tenant, AppID: app}
		must(t, scope.Validate())
		encoded, err := json.Marshal(scope)
		must(t, err)
		var wire map[string]string
		must(t, json.Unmarshal(encoded, &wire))
		if len(wire) != 2 || wire["tenant_id"] != tenantText || wire["app_id"] != appText {
			t.Fatalf("configuration encoding changed: %s", encoded)
		}
		var restored ck.Scope
		must(t, json.Unmarshal(encoded, &restored))
		must(t, restored.Validate())
		if restored != scope {
			t.Fatal("JSON round trip changed app ownership")
		}
	})
	for name, bad := range map[string]string{"blank": "", "zero": "00000000-0000-0000-0000-000000000000", "uppercase": strings.ToUpper(tenantText), "leading_space": " " + tenantText, "trailing_space": tenantText + " ", "compact": strings.ReplaceAll(tenantText, "-", ""), "braces": "{" + tenantText + "}", "malformed": "unknown-tenant"} {
		t.Run("refuse_"+name, func(t *testing.T) {
			if _, err := ck.ParseTenantID(bad); err == nil {
				t.Fatal("tenant parser accepted noncanonical identity")
			}
			if _, err := ck.ParseAppID(bad); err == nil {
				t.Fatal("app parser accepted noncanonical identity")
			}
			tenantBefore, appBefore := tenant, app
			if tenantBefore.UnmarshalText([]byte(bad)) == nil || appBefore.UnmarshalText([]byte(bad)) == nil {
				t.Fatal("text decoder accepted invalid identity")
			}
			if tenantBefore != tenant || appBefore != app {
				t.Fatal("failed decoding changed existing binding")
			}
		})
	}
	t.Run("provisioning_generates_distinct_nonzero_bindings", func(t *testing.T) {
		a, b := ck.NewTenantID(), ck.NewTenantID()
		c, d := ck.NewAppID(), ck.NewAppID()
		if a.IsZero() || b.IsZero() || c.IsZero() || d.IsZero() || a == b || c == d {
			t.Fatal("provisioning reused or omitted an identity")
		}
		must(t, (ck.Scope{TenantID: a, AppID: c}).Validate())
		var emptyTenant ck.TenantID
		var emptyApp ck.AppID
		if !emptyTenant.IsZero() || !emptyApp.IsZero() {
			t.Fatal("zero-value identity was considered provisioned")
		}
		for _, scope := range []ck.Scope{{}, {TenantID: a}, {AppID: c}} {
			if scope.Validate() == nil {
				t.Fatalf("incomplete deployment binding accepted: %+v", scope)
			}
		}
	})
}
