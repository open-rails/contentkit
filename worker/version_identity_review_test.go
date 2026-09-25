package worker

import (
	"context"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/search"
	"testing"
)

func TestIntegrationVersionedBuilderIdentityAndSinkRetry(t *testing.T) {
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
	sink := &recordingSink{failing: true}
	opts.Sink = sink
	if err := SyncOnce(ctx, opts); err != nil {
		t.Fatal(err)
	}
	if got := document(t, ctx, pool, schema); got != "Versioned title" {
		t.Fatalf("versioned document missing: %q", got)
	}
	if n := count(t, ctx, pool, schema, "content_search_dirty", "true"); n != 1 {
		t.Fatalf("failed sink lost retry: %d", n)
	}
	sink.failing = false
	if err := SyncOnce(ctx, opts); err != nil {
		t.Fatal(err)
	}
	if len(sink.upserts) != 1 || !sink.upserts[0].ContentRef.Equal(ref) || len(sink.deletes) != 0 {
		t.Fatalf("version delivery: upserts=%+v deletes=%+v", sink.upserts, sink.deletes)
	}
	if n := count(t, ctx, pool, schema, "content_search_dirty", "true"); n != 0 {
		t.Fatalf("successful delivery not acknowledged: %d", n)
	}
}
