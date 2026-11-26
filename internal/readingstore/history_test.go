package readingstore

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestHistoryRoundTrip(t *testing.T) {
	day := time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)
	g1, g2 := uuid.New(), uuid.New()
	in := []Action{
		{Kind: ActionSearch, At: day.Add(9*time.Hour + 5*time.Minute), Offset: -300, SessionStart: true, Query: "cats"},
		{Kind: ActionGallery, At: day.Add(9*time.Hour + 6*time.Minute), Offset: -300, ID: g1, Tenths: 7, ActiveS: 300, Reaction: ReactionFavorite, Surface: SurfaceSearch},
		{Kind: ActionGallery, At: day.Add(23*time.Hour + 59*time.Minute), Offset: 345, SessionStart: true, ID: g2, Tenths: 10, ActiveS: 0},
	}
	packed := EncodeDay(day, []Action{in[2], in[0], in[1]})
	// 1 version + search (2 + 2 minute + 1 + 4) + gallery (2 + 2 + 19) + gallery (2 + 2 + 19)
	if len(packed) != 1+9+23+23 {
		t.Fatalf("packed %d bytes", len(packed))
	}
	out, err := DecodeDay(day, packed)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 {
		t.Fatalf("decoded %d actions", len(out))
	}
	if out[0].Kind != ActionSearch || out[0].Query != "cats" || !out[0].SessionStart || out[0].Offset != -300 || !out[0].At.Equal(in[0].At) {
		t.Fatalf("search = %+v", out[0])
	}
	if out[1].ID != g1 || out[1].Tenths != 7 || out[1].Reaction != ReactionFavorite || out[1].ActiveS != 256 || out[1].SessionStart || out[1].Surface != SurfaceSearch {
		t.Fatalf("gallery = %+v", out[1])
	}
	if out[2].ID != g2 || out[2].Offset != 345 || out[2].ActiveS != 0 || !out[2].SessionStart || out[2].Surface != SurfaceOutside {
		t.Fatalf("late gallery = %+v", out[2])
	}
}

func TestHistoryBoundsAndMalformed(t *testing.T) {
	day := time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)
	long := strings.Repeat("é", 200)
	out, err := DecodeDay(day, EncodeDay(day, []Action{{Kind: ActionSearch, At: day, Query: long, Offset: 2000}}))
	if err != nil {
		t.Fatal(err)
	}
	if len(out[0].Query) > maxQueryBytes || !strings.HasPrefix(long, out[0].Query) || out[0].Offset != 14*60 {
		t.Fatalf("bounded search = %d bytes, offset %d", len(out[0].Query), out[0].Offset)
	}
	gallery := EncodeDay(day, []Action{{Kind: ActionGallery, At: day, Surface: SurfaceHome}})
	for _, bad := range [][]byte{nil, {1}, {2, 0}, {2, 0, 0, 5}, {2, 1, 0, 0, 9, 'a'}, gallery[:len(gallery)-1]} {
		if _, err := DecodeDay(day, bad); err == nil {
			t.Fatalf("decoded malformed %v", bad)
		}
	}
	if ActiveBucket(0) != 0 || ActiveBucket(1) != 1 || ActiveBucket(3) != 2 || ActiveBucket(4) != 3 {
		t.Fatal("active buckets")
	}
}

func TestSurfaceOf(t *testing.T) {
	artist := func(name string) bool { return name == "artist" }
	for name, want := range map[string]Surface{
		"": SurfaceOutside, "home": SurfaceHome, "list": SurfaceList, "channel": SurfaceList, "popular": SurfacePopular,
		"similar": SurfaceSimilar, "Search": SurfaceSearch, "search_suggestions": SurfaceSearch, "featured": SurfaceEntity,
		"artist": SurfaceEntity, "for-you": SurfaceRecommended, "recommendations": SurfaceRecommended, "history": SurfaceContinue,
		"continue": SurfaceContinue, "favorites": SurfaceFavorites, "newsletter": SurfaceOther,
	} {
		if got := SurfaceOf(name, artist); got != want {
			t.Errorf("SurfaceOf(%q) = %d, want %d", name, got, want)
		}
	}
}
