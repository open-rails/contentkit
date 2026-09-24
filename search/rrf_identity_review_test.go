package search

import (
	"github.com/open-rails/contentkit/contentref"
	"testing"
)

func TestRRFKeepsOpaqueContentKeysDistinct(t *testing.T) {
	a := RRFKey{ContentKey: contentref.NewVersion("tenant", "gallery", cid(1), "work\x1fversion").Key(), Language: "en"}
	b := RRFKey{ContentKey: contentref.NewVersion("tenant", "gallery", cid(1), "work").Key(), Language: "version\x1fen"}
	for _, key := range []RRFKey{a, b} {
		if err := key.Ref().Validate(); err != nil {
			t.Fatal(err)
		}
	}
	got, err := FuseRRFWithTrace([][]RRFKey{{a}, {b}}, RRFOptions{})
	if err != nil || len(got) != 2 {
		t.Fatalf("distinct opaque references merged: %+v, %v", got, err)
	}
	for _, hit := range got {
		if len(hit.Contributions) != 1 {
			t.Fatalf("cross-reference contributions: %+v", hit)
		}
	}
}
