package uuidv7

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestNewIsV7Now(t *testing.T) {
	before := time.Now().Truncate(time.Millisecond)
	id := New()
	if id.Version() != 7 || id.Variant() != uuid.RFC4122 {
		t.Fatalf("%s is not a UUIDv7", id)
	}
	if at := At(id.String()); at.Before(before) || at.After(time.Now()) {
		t.Fatalf("%s is dated %s", id, at)
	}
}

func TestNameIsDeterministicAndDated(t *testing.T) {
	at := time.Date(2026, 11, 24, 12, 0, 0, 123_456_789, time.UTC)
	id := Name(at, uuid.NameSpaceURL, "purge:t/k/x/")
	if id != Name(at, uuid.NameSpaceURL, "purge:t/k/x/") {
		t.Fatal("same inputs derived different ids")
	}
	if id.Version() != 7 || id.Variant() != uuid.RFC4122 || !At(id.String()).Equal(at.Truncate(time.Millisecond)) {
		t.Fatalf("%s is not a UUIDv7 dated %s", id, at)
	}
	// Pinned: a changed derivation gives every in-flight operation a new id.
	if got := id.String(); got != "01a20d6d-5a7b-740a-a919-1eddece8c46a" {
		t.Fatalf("derivation changed: %s", got)
	}
	for _, other := range []uuid.UUID{
		Name(at, uuid.NameSpaceOID, "purge:t/k/x/"),
		Name(at, uuid.NameSpaceURL, "purge:t/k/y/"),
		Name(at.Add(time.Millisecond), uuid.NameSpaceURL, "purge:t/k/x/"),
	} {
		if other == id {
			t.Fatalf("distinct inputs derived %s", id)
		}
	}
}

func TestChildIsDatedAtItsParent(t *testing.T) {
	parent := New()
	child := Child(parent, "reset")
	if child == parent || child != Child(parent, "reset") || child == Child(parent, "delete") {
		t.Fatal("child is not a distinct deterministic id")
	}
	if !At(child.String()).Equal(At(parent.String())) {
		t.Fatalf("child %s is not dated at parent %s", child, parent)
	}
	// A parent without an instant dates its children at the Unix epoch.
	legacy := uuid.NewSHA1(uuid.NameSpaceURL, []byte("legacy"))
	if got := At(Child(legacy, "reset").String()); got.UnixMilli() != 0 {
		t.Fatalf("child of a v5 parent dated %s", got)
	}
}

func TestAtTakesTheFirstV7(t *testing.T) {
	v7 := Name(time.UnixMilli(1_700_000_000_000), uuid.Nil, "x")
	if got := At("", "not-a-uuid", uuid.NewString(), v7.String(), New().String()); got.UnixMilli() != 1_700_000_000_000 {
		t.Fatalf("At = %s", got)
	}
	if got := At(uuid.NewString()); got.UnixMilli() != 0 {
		t.Fatalf("At of a v4 = %s", got)
	}
}
