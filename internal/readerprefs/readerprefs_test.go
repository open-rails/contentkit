package readerprefs

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/open-rails/contentkit/contentref"
)

func TestGuestOverrides(t *testing.T) {
	allowed := map[string]bool{"one": true, "two": true}
	for _, tc := range []struct {
		name   string
		header []string
		want   map[string]string
		bad    bool
	}{
		{name: "absent"}, {name: "empty", header: []string{""}},
		{name: "shared choices", header: []string{`{"filters":[{"entity_id":"one","visible":true},{"entity_id":"two","visible":false}]}`}, want: map[string]string{Key("one"): Show, Key("two"): Hide}},
		{name: "default marker", header: []string{`{"filters":[{"entity_id":"one","default":true}]}`}, want: map[string]string{Key("one"): Default}},
		{name: "empty choices", header: []string{`{"filters":[]}`}, want: map[string]string{}},
		{name: "another domain or fixed entity", header: []string{`{"filters":[{"entity_id":"other","visible":true}]}`}, want: map[string]string{}},
		{name: "duplicate", header: []string{`{"filters":[{"entity_id":"one","visible":true},{"entity_id":"one","visible":false}]}`}, bad: true},
		{name: "missing choice", header: []string{`{"filters":[{"entity_id":"one"}]}`}, bad: true},
		{name: "null choice", header: []string{`{"filters":[{"entity_id":"one","visible":null}]}`}, bad: true},
		{name: "false default", header: []string{`{"filters":[{"entity_id":"one","default":false}]}`}, bad: true},
		{name: "two choices", header: []string{`{"filters":[{"entity_id":"one","visible":false,"default":true}]}`}, bad: true},
		{name: "extra field", header: []string{`{"filters":[],"include_deleted":true}`}, bad: true},
		{name: "retired per-kind shape", header: []string{`{"kinds":{"gallery":[]}}`}, bad: true},
		{name: "bad JSON", header: []string{`no`}, bad: true}, {name: "null", header: []string{`null`}, bad: true},
		{name: "trailing JSON", header: []string{`{} {}`}, bad: true}, {name: "multiple headers", header: []string{`{}`, `{}`}, bad: true},
		{name: "too large", header: []string{strings.Repeat(" ", 8193)}, bad: true},
		{name: "too many", header: []string{`{"filters":[` + strings.TrimSuffix(strings.Repeat(`{"entity_id":"one","visible":true},`, 129), ",") + `]}`}, bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Overrides(WithHeader(context.Background(), tc.header), allowed)
			if (err != nil) != tc.bad || !tc.bad && !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got%v err%v want%v", got, err, tc.want)
			}
		})
	}
}

func TestCanonicalKeyCannotAliasValidLegacyEntityIDs(t *testing.T) {
	id := "01900000-0000-7000-8000-000000000001"
	if err := contentref.ValidateID(id); err != nil {
		t.Fatal(err)
	}
	if contentref.ValidateID(Key(id)) == nil || strings.Contains(Key(id), "/") {
		t.Fatal("new key can be mistaken for a legacy raw ID or kind/ID")
	}
	values := map[string]string{"entity/" + id: Show, "gallery/" + id: Hide}
	if visible, overridden := Stored(values, id); visible || !overridden {
		t.Fatal("valid legacy entity kind bypassed hide-wins conflict resolution")
	}
}

func TestSharedPreferenceLegacyConflicts(t *testing.T) {
	for _, tc := range []struct {
		name                string
		values              map[string]string
		visible, overridden bool
	}{
		{name: "absent", values: map[string]string{}},
		{name: "old global hide", values: map[string]string{"tag": Hide}, overridden: true},
		{name: "old global show", values: map[string]string{"tag": Show}, visible: true, overridden: true},
		{name: "scoped hide wins show", values: map[string]string{"gallery/tag": Show, "blog/tag": Hide}, overridden: true},
		{name: "entity is a valid legacy kind", values: map[string]string{"entity/tag": Show, "gallery/tag": Hide}, overridden: true},
		{name: "scoped show beats old global", values: map[string]string{"tag": Hide, "gallery/tag": Show}, visible: true, overridden: true},
		{name: "scoped reset suppresses global", values: map[string]string{"tag": Hide, "gallery/tag": Default}},
		{name: "explicit choice beats another scope reset", values: map[string]string{"gallery/tag": Default, "blog/tag": Hide}, overridden: true},
		{name: "new show beats stale hide", values: map[string]string{Key("tag"): Show, "gallery/tag": Hide, "tag": Hide}, visible: true, overridden: true},
		{name: "new hide beats stale show", values: map[string]string{Key("tag"): Hide, "gallery/tag": Show}, overridden: true},
		{name: "new reset suppresses every legacy", values: map[string]string{Key("tag"): Default, "gallery/tag": Hide, "blog/tag": Hide, "tag": Hide}},
		{name: "other entity is separate", values: map[string]string{"gallery/other": Hide, "tag": Show}, visible: true, overridden: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := map[string]string{}
			for k, v := range tc.values {
				before[k] = v
			}
			visible, overridden := Stored(tc.values, "tag")
			if visible != tc.visible || overridden != tc.overridden {
				t.Fatalf("got%v %v", visible, overridden)
			}
			if !reflect.DeepEqual(before, tc.values) {
				t.Fatal("read mutated original choices")
			}
		})
	}
}
