package contentref

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestValidateID(t *testing.T) {
	v7 := NewID()
	if err := ValidateID(v7); err != nil {
		t.Fatalf("%s: %v", v7, err)
	}
	for _, bad := range []string{
		"", "1", "18", "g1",
		uuid.NewString(),                         // v4
		"0192f000-0000-1000-8000-000000000001",   // v1
		"0192F000-0000-7000-8000-000000000001",   // uppercase
		"0192f000-0000-7000-c000-000000000001",   // variant
		"{0192f000-0000-7000-8000-000000000001}", // braces
		"0192f000000070008000000000000001",       // no dashes
		"0192f000-0000-7000-8000-00000000000g",
	} {
		err := ValidateID(bad)
		var e *IDError
		if !errors.Is(err, ErrInvalidID) || !errors.As(err, &e) || e.ID != bad {
			t.Fatalf("%q: %v", bad, err)
		}
		if New("t", "k", bad).Validate() == nil {
			t.Fatalf("ref with %q validated", bad)
		}
		if _, err := Parse("t", "k", bad); !errors.Is(err, ErrInvalidID) {
			t.Fatalf("Parse %q: %v", bad, err)
		}
	}
}

func TestIDAtSortsByTime(t *testing.T) {
	base := time.Date(2019, 3, 1, 12, 0, 0, 0, time.UTC)
	var ids []string
	for i := range 50 {
		at := base.Add(time.Duration(i) * time.Millisecond * 37)
		id := IDAt(at)
		if err := ValidateID(id); err != nil {
			t.Fatal(err)
		}
		u := uuid.MustParse(id)
		if sec, nsec := u.Time().UnixTime(); time.Unix(sec, nsec).UnixMilli() != at.UnixMilli() {
			t.Fatalf("%s encodes %v, want %v", id, time.Unix(sec, nsec), at)
		}
		ids = append(ids, id)
	}
	if !slices.IsSorted(ids) {
		t.Fatalf("ids do not sort by time: %v", ids)
	}
	if IDAt(base) == IDAt(base) {
		t.Fatal("ids at one instant must differ")
	}
}
