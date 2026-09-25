package contentref

import (
	"errors"
	"slices"
	"strings"
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

func TestLegacyIDGolden(t *testing.T) {
	// Vectors from Doujins' LegacyV7 (pkg/utils/uid.go), whose migrated ids
	// must not change.
	for _, c := range []struct {
		kind string
		id   int64
		at   time.Time
		want string
	}{
		{"gallery", 1, time.Time{}, "0125e72e-7801-76fb-a616-e214610f18dc"},
		{"gallery", 123456, time.Date(2019, 3, 1, 12, 30, 45, 123456789, time.UTC), "0169393c-4583-7f7a-9380-8e0cf81d93d3"},
		{"tag", 42, time.Time{}, "0125e72e-782a-7dc6-b914-a655b9adef4d"},
		{"artist", 0, time.Time{}, "0125e72e-7800-79e6-8f2f-e4e682858b6f"},
		{"post", 7, time.Date(2021, 11, 5, 8, 0, 0, 0, time.FixedZone("x", 9*3600)), "017ced2e-3180-77e8-bc77-437e4e5d5326"},
		{"voice_actor", 31337, time.Unix(1600000000, 999999999), "0174876e-83e7-7cea-bac1-438e33bd7f7d"},
		{"video", 88, time.Date(2015, 6, 30, 23, 59, 59, 0, time.UTC), "014e46e9-b818-733b-832b-d0890a550cfe"},
		{"", 5, time.Unix(0, 0), "00000000-0000-751e-ac31-9a97655261f1"},
	} {
		got := LegacyID("doujins", c.kind, c.id, c.at)
		if got != c.want {
			t.Errorf("LegacyID(%q, %d, %v) = %s, want %s", c.kind, c.id, c.at, got, c.want)
		}
		if err := ValidateID(got); err != nil {
			t.Error(err)
		}
	}
	if LegacyID("doujins", "gallery", 1, time.Time{}) == LegacyID("doujins", "tag", 1, time.Time{}) {
		t.Fatal("kinds must not collide")
	}
	if !(LegacyID("doujins", "tag", 1, time.Time{}) < LegacyID("doujins", "tag", 2, time.Time{})) {
		t.Fatal("undated rows must sort by legacy id")
	}
	// Another namespace: same timestamp, other bits (vector computed independently).
	h := LegacyID("hentai0", "gallery", 1, time.Time{})
	if want := "0125e72e-7801-77a9-a112-6d50ba4ef7e8"; h != want {
		t.Fatalf("hentai0 gallery 1 = %s, want %s", h, want)
	}
	if h == LegacyID("doujins", "gallery", 1, time.Time{}) || h[:13] != "0125e72e-7801" {
		t.Fatal("namespaces must differ only in the hashed bits")
	}
	for _, bad := range []string{"", "Doujins", "a/b", "a b", strings.Repeat("x", 65)} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("namespace %q accepted", bad)
				}
			}()
			LegacyID(bad, "gallery", 1, time.Time{})
		}()
	}
}
