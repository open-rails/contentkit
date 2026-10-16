package catalog

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/open-rails/contentkit/media"
)

// Segment writes and page selection must serialize before either holds a
// version row lock. Contend against the original media lock namespace too.
func TestSegmentUpdatesSharePageSelectionLock(t *testing.T) {
	f := newFixture(t, nil)
	id := f.create("staff", gallery("Locked page selection", "en"))
	v := f.read("staff", "main.test", id)[0].Versions[0]
	segments := []Segment{{Title: "First", Start: 0, End: 2}}
	input := []VersionPut{{ContentID: id, VersionID: v.ID, VersionInput: VersionInput{Segments: &segments}}}
	release, err := media.PGLocker(f.pool).Lock(f.ctx, "contentkit:versions:"+tenant+":"+v.ID)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	defer func() {
		if !released {
			release()
		}
	}()
	ctx, cancel := context.WithTimeout(f.ctx, 250*time.Millisecond)
	defer cancel()
	if _, err := f.s.PutVersions(ctx, f.as("staff"), input); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("segment write did not wait for the page-selection lock: %v", err)
	}
	if got := f.read("staff", "main.test", id)[0].Versions[0].Segments; len(got) != 0 {
		t.Fatalf("timed-out segment write changed the version: %+v", got)
	}
	release()
	released = true
	if _, err := f.s.PutVersions(f.ctx, f.as("staff"), input); err != nil {
		t.Fatal(err)
	}
	if got := f.read("staff", "main.test", id)[0].Versions[0].Segments; len(got) != 1 || got[0].Start != 0 || got[0].End != 2 {
		t.Fatalf("segment write did not complete after release: %+v", got)
	}
}
