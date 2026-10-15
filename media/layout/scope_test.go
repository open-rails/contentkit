package layout_test

import (
	"github.com/open-rails/contentkit/media/layout"
	"strings"
	"testing"
)

func TestScopedPathsRoundTrip(t *testing.T) {
	for _, scope := range []layout.Scope{{TenantID: "11111111-1111-4111-8111-111111111111", AppID: "22222222-2222-4222-8222-222222222222"}, {TenantID: "11111111-1111-4111-8111-111111111111"}} {
		for _, version := range []string{"", "01920000-0000-7000-8000-000000000002"} {
			prefix := scope.Prefix("user", "01920000-0000-7000-8000-000000000001", version)
			k, ok := layout.Parse(prefix + "public/avatar-64.webp")
			if !ok || k.Scope != scope || k.Namespace != scope.Partition() || k.Folder() != prefix {
				t.Fatalf("%s: %+v %v", prefix, k, ok)
			}
			if strings.Contains(prefix, scope.Partition()) {
				t.Fatal("internal partition leaked into object path")
			}
		}
	}
	for _, bad := range []string{"tenant/no-uuid/app/22222222-2222-4222-8222-222222222222/user/x/public/a.webp", "tenant/11111111-1111-4111-8111-111111111111/app/../user/x/public/a.webp"} {
		if _, ok := layout.Parse(bad); ok {
			t.Fatalf("accepted %q", bad)
		}
	}
}
