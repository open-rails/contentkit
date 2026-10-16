package readerprefs

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestGuestOverrides(t *testing.T) {
	allowed := map[string]bool{"one": true, "two": true}
	for _, tc := range []struct {
		name   string
		header []string
		want   map[string]bool
		bad    bool
	}{
		{name: "absent"},
		{name: "empty", header: []string{""}},
		{name: "choices", header: []string{`{"filters":[{"group_id":"one","visible":true},{"group_id":"two","visible":false}]}`}, want: map[string]bool{"one": true, "two": false}},
		{name: "defaults", header: []string{`{"filters":[]}`}, want: map[string]bool{}},
		{name: "unknown entity", header: []string{`{"filters":[{"group_id":"other","visible":true}]}`}, bad: true},
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
