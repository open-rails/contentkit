package media

import (
	"crypto/sha256"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
)

// A retry keeps permanent-erasure ownership in the existing content purge.
func TestVersionErasureRetryKeepsPurgeOwnership(t *testing.T) {
	pool := pgtest.Pool(t, nil)
	schema := pgtest.Schema(t, t.Context(), pool)
	journal, err := NewPGJournal(pool, schema, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, versioned := range []bool{true, false} {
		ref := contentref.New("site", "gallery", contentref.NewID())
		folder := "site/gallery/" + ref.ContentID + "/"
		if versioned {
			ref = ref.WithVersion(contentref.NewID())
			folder = "site/gallery/" + ref.Version() + "/"
		}
		original := manifestCommit{ID: uuid.New(), Ref: ref, Folder: folder, Fingerprint: sha256.Sum256([]byte("retry erasure")), Effects: journalEffects{Erasure: true}}
		first, err := journal.begin(t.Context(), original)
		if err != nil {
			t.Fatal(err)
		}
		if err = journal.finish(t.Context(), first, false, nil); err != nil {
			t.Fatal(err)
		}
		retry, err := journal.begin(t.Context(), original)
		if err != nil {
			t.Fatal(err)
		}
		if !retry.Effects.Erasure {
			t.Fatal("retry lost its permanent erasure classification")
		}
		if retry.Effects.Ingest {
			t.Fatal("purge retry acquired an ingest owner")
		}

	}
}
