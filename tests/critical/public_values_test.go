package critical_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/kinds"
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

// Content model files distinguish content types from associated entity types.
// The Go field remains Kinds; the discovery HTTP model has its own wire shape.
func TestPublicContentKindsConfig(t *testing.T) {
	model := kinds.Config{
		Languages: []string{"en"},
		Kinds: map[string]kinds.Kind{"blog": {
			Route: "blog", Presentation: "post", Versions: kinds.VersionRules{Languages: "one"},
			Channel: &kinds.ChannelPolicy{Fixed: "blog"},
		}},
		Channels: &kinds.Channels{Creation: "staff"}, EntityKinds: map[string]kinds.EntityKind{"tag": {}},
	}
	must(t, model.Validate())
	if _, routed := model.Routes()["channel"]; routed {
		t.Fatal("internal blog ownership needs no public channel route")
	}
	for name, codec := range map[string]struct {
		marshal   func(any) ([]byte, error)
		unmarshal func([]byte, any) error
	}{
		"json": {json.Marshal, json.Unmarshal},
		"yaml": {yaml.Marshal, yaml.Unmarshal},
	} {
		t.Run(name+"_round_trip", func(t *testing.T) {
			encoded, err := codec.marshal(model)
			must(t, err)
			var fields map[string]any
			must(t, codec.unmarshal(encoded, &fields))
			if fields["content_kinds"] == nil || fields["entity_kinds"] == nil {
				t.Fatalf("missing explicit kind families: %s", encoded)
			}
			var decoded kinds.Config
			must(t, codec.unmarshal(encoded, &decoded))
			must(t, decoded.Validate())
			// YAML normalizes omitted nil collections to empty collections; their
			// configured representation must remain stable, not their Go allocation.
			roundTrip, err := codec.marshal(decoded)
			must(t, err)
			if string(roundTrip) != string(encoded) {
				t.Fatal("content model changed during configuration round trip")
			}
		})
		t.Run(name+"_reject_unknown_key", func(t *testing.T) {
			encoded, err := codec.marshal(model)
			must(t, err)
			var fields map[string]any
			must(t, codec.unmarshal(encoded, &fields))
			fields["unknown_config_key"] = true
			bad, err := codec.marshal(fields)
			must(t, err)
			unchanged := model
			if err := codec.unmarshal(bad, &unchanged); err == nil {
				t.Fatal("unknown config key silently accepted")
			}
			if !reflect.DeepEqual(unchanged, model) {
				t.Fatal("rejected configuration changed the existing model")
			}
			// A permissive host wrapper must not bypass the model's strict decoder.
			wrapped, err := codec.marshal(map[string]any{"content": fields})
			must(t, err)
			var host ck.Config
			if err := codec.unmarshal(wrapped, &host); err == nil {
				t.Fatal("host wrapper accepted an unknown config key")
			}
		})
	}
}
