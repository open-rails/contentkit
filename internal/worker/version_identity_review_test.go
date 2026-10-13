package worker

import (
	"context"
	"testing"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/search"
)

func TestIntegrationVersionedBuilderIdentity(t *testing.T) {
	ctx, pool, schema := workerFixture(t)
	ref := contentref.NewVersion(tenant, "gallery", cid(1), "edition-ja")
	key := search.DocumentKey{ContentRef: ref, Language: "en"}
	if err := search.MarkDirty(ctx, pool, schema, []search.DirtyMark{{DocumentKey: key}}); err != nil {
		t.Fatal(err)
	}
	opts := workerOptions(pool, schema, func(_ context.Context, tenant, kind, lang string, refs []contentref.ContentRef) ([]search.KeywordDocument, error) {
		// Host database hydration creates fresh version pointers, as real hosts do.
		return []search.KeywordDocument{{DocumentKey: search.DocumentKey{ContentRef: contentref.NewVersion(tenant, kind, refs[0].ContentID, refs[0].Version()), Language: lang}, Title: "Versioned title"}}, nil
	})
	if err := SyncOnce(ctx, opts); err != nil {
		t.Fatal(err)
	}
	if got := document(t, ctx, pool, schema); got != "Versioned title" {
		t.Fatalf("versioned document missing: %q", got)
	}
	if n := count(t, ctx, pool, schema, "content_search_dirty", "true"); n != 0 {
		t.Fatalf("published row not acknowledged: %d", n)
	}
}
