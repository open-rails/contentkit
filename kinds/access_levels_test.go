package kinds

import (
	"errors"
	"testing"
)

func TestKindAccessLevels(t *testing.T) {
	for _, levels := range [][]string{nil, {"public", "tier"}, {"ppv"}, {"group_ppv"}} {
		c := doujins()
		k := c.Kinds["gallery"]
		k.AccessLevels = levels
		c.Kinds["gallery"] = k
		if err := c.Validate(); err != nil {
			t.Fatalf("valid policy %v: %v", levels, err)
		}
	}
	for _, levels := range [][]string{{"unknown"}, {"public", "public"}} {
		c := doujins()
		k := c.Kinds["gallery"]
		k.AccessLevels = levels
		c.Kinds["gallery"] = k
		if !errors.Is(c.Validate(), ErrInvalid) {
			t.Fatalf("accepted invalid policy %v", levels)
		}
	}
	k := Kind{AccessLevels: []string{"public", "tier"}}
	if !k.AllowsAccess("public") || !k.AllowsAccess("tier") || k.AllowsAccess("ppv") || k.AllowsAccess("group") || k.AllowsAccess("group_ppv") {
		t.Fatal("free/premium policy accepted another access level")
	}
}

func TestKindCommentModes(t *testing.T) {
	c := doujins()
	k := c.Kinds["gallery"]
	k.CommentModes = []string{"open"}
	c.Kinds["gallery"] = k
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if !k.AllowsComments("open") || k.AllowsComments("closed") || k.AllowsComments("hidden") {
		t.Fatal("open-only policy permits another comment mode")
	}
	for _, modes := range [][]string{{"bad"}, {"open", "open"}} {
		k.CommentModes = modes
		c.Kinds["gallery"] = k
		if !errors.Is(c.Validate(), ErrInvalid) {
			t.Fatalf("invalid modes %v accepted", modes)
		}
	}
}
