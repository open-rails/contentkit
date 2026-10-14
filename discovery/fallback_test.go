package discovery

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/signal"
)

type fixed struct {
	out []Candidate
	err error
}

func (f fixed) Similar(context.Context, contentref.ContentRef, Query) ([]Candidate, error) {
	return f.out, f.err
}

func (f fixed) ForSubject(context.Context, signal.Subject, Query) ([]Candidate, error) {
	return f.out, f.err
}

// cid is a deterministic canonical UUIDv7 content id; cid(n) sorts in n order.
func cid(n int) string { return fmt.Sprintf("01920000-0000-7000-8000-%012d", n) }

func refs(ids ...string) []Candidate {
	out := make([]Candidate, 0, len(ids))
	for _, id := range ids {
		out = append(out, Candidate{Ref: contentref.New("t", "gallery", id)})
	}
	return out
}

func ids(cs []Candidate) []string {
	out := []string{}
	for _, c := range cs {
		out = append(out, c.Ref.ContentID)
	}
	return out
}

func TestFallback(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("boom")
	var reported []error
	onError := func(err error) { reported = append(reported, err) }
	cases := []struct {
		name               string
		primary, secondary fixed
		want               []string
		wantErr            bool
		reports            int
	}{
		{"full primary", fixed{out: refs(cid(1), cid(2))}, fixed{err: boom}, []string{cid(1), cid(2)}, false, 0},
		{"primary error", fixed{err: boom}, fixed{out: refs(cid(3))}, []string{cid(3)}, false, 1},
		{"thin primary filled", fixed{out: refs(cid(1))}, fixed{out: refs(cid(1), cid(3))}, []string{cid(1), cid(3)}, false, 0},
		{"thin primary, secondary error", fixed{out: refs(cid(1))}, fixed{err: boom}, []string{cid(1)}, false, 1},
		{"both fail", fixed{err: boom}, fixed{err: boom}, nil, true, 1},
	}
	for _, tc := range cases {
		reported = nil
		f := Fallback{Primary: tc.primary, Secondary: tc.secondary, OnError: onError}
		got, err := f.ForSubject(ctx, signal.Subject{UserID: "u"}, Query{Limit: 2})
		if (err != nil) != tc.wantErr || len(reported) != tc.reports {
			t.Fatalf("%s: err=%v reported=%v", tc.name, err, reported)
		}
		if !tc.wantErr && !reflect.DeepEqual(ids(got), tc.want) {
			t.Fatalf("%s: %v", tc.name, ids(got))
		}
	}
}
