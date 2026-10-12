package contentkit

import "testing"

func TestParseRouteGroups(t *testing.T) {
	g, err := ParseRouteGroups([]string{"content", "media"})
	if err != nil || g != ContentRoutes|MediaRoutes {
		t.Fatalf("%v %v", g, err)
	}
	if g, err := ParseRouteGroups([]string{"all"}); err != nil || g != AllRoutes {
		t.Fatalf("%v %v", g, err)
	}
	if _, err := ParseRouteGroups([]string{"nope"}); err == nil {
		t.Fatal("unknown group accepted")
	}
}
