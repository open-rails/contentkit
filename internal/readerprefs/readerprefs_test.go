package readerprefs

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestGuestOverrides(t *testing.T) {
	allowed := map[string]map[string]bool{"gallery": {"one": true, "two": true}}
	for _, tc := range []struct {
		name   string
		header []string
		want   map[string]string
		bad    bool
	}{
		{name: "absent"},
		{name: "empty", header: []string{""}},
		{name: "choices", header: []string{`{"filters":[{"group_id":"one","visible":true},{"group_id":"two","visible":false}]}`}, want: map[string]string{Key("gallery", "one"): Show, Key("gallery", "two"): Hide}},
		{name: "defaults", header: []string{`{"filters":[]}`}, want: map[string]string{}},
		{name: "another domain entity", header: []string{`{"filters":[{"group_id":"other","visible":true}]}`}, want: map[string]string{}},
		{name: "duplicate", header: []string{`{"filters":[{"group_id":"one","visible":true},{"group_id":"one","visible":false}]}`}, bad: true},
		{name: "missing visibility", header: []string{`{"filters":[{"group_id":"one"}]}`}, bad: true},
		{name: "null visibility", header: []string{`{"filters":[{"group_id":"one","visible":null}]}`}, bad: true},
		{name: "extra field", header: []string{`{"filters":[],"include_deleted":true}`}, bad: true},
		{name: "bad JSON", header: []string{`no`}, bad: true},
		{name: "null", header: []string{`null`}, bad: true},
		{name: "trailing JSON", header: []string{`{} {}`}, bad: true},
		{name: "multiple headers", header: []string{`{}`, `{}`}, bad: true},
		{name: "too large", header: []string{strings.Repeat(" ", 8193)}, bad: true},
		{name: "too many", header: []string{`{"filters":[` + strings.TrimSuffix(strings.Repeat(`{"group_id":"one","visible":true},`, 129), ",") + `]}`}, bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Overrides(WithHeader(context.Background(), tc.header), allowed)
			if (err != nil) != tc.bad || !tc.bad && !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("overrides=%v err=%v, want %v bad=%v", got, err, tc.want, tc.bad)
			}
		})
	}
}

func TestScopedGuestOverridesAndLegacyFallback(t *testing.T) {
	allowed := map[string]map[string]bool{"gallery": {"tag": true}, "blog": {"tag": true}}
	ctx := WithHeader(context.Background(), []string{`{"legacy_filters":[{"entity_id":"tag","visible":false}],"kinds":{"gallery":[{"entity_id":"tag","visible":true}]}}`})
	got, err := Overrides(ctx, allowed)
	if err != nil || got[Key("gallery", "tag")] != Show || got[Key("blog", "tag")] != Hide {
		t.Fatalf("scoped preferences: %v %v", got, err)
	}
	values := map[string]string{"tag": Hide, Key("gallery", "tag"): Default}
	if _, overridden := Stored(values, "gallery", "tag"); overridden {
		t.Fatal("reset resurrected a legacy preference")
	}
	if visible, overridden := Stored(values, "blog", "tag"); visible || !overridden {
		t.Fatal("legacy fallback was discarded")
	}
}

func TestGuestDefaultMarkers(t *testing.T) {
	allowed := map[string]map[string]bool{"gallery": {"tag": true}, "blog": {"tag": true}}
	raw := `{"legacy_filters":[{"entity_id":"tag","visible":false}],"kinds":{"gallery":[{"entity_id":"tag","default":true}]}}`
	got, err := Overrides(WithHeader(context.Background(), []string{raw}), allowed)
	if err != nil || got[Key("gallery", "tag")] != Default || got[Key("blog", "tag")] != Hide {
		t.Fatalf("default marker: %v %v", got, err)
	}
	for _, bad := range []string{`{"kinds":{"gallery":[{"entity_id":"tag","default":false}]}}`, `{"kinds":{"gallery":[{"entity_id":"tag","default":true,"visible":false}]}}`, `{"kinds":{"gallery":[{"entity_id":"tag"}]}}`} {
		if _, err := Overrides(WithHeader(context.Background(), []string{bad}), allowed); err == nil {
			t.Fatalf("invalid marker accepted: %s", bad)
		}
	}
}
