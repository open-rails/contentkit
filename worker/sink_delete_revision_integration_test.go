package worker

import (
	"context"
	"testing"

	"github.com/open-rails/contentkit/search"
)

// delayedDeleteSink models a remote delete accepted before the caller times
// out, but applied after a subsequent worker tick has published newer content.
// Both operations must fence delayed effects by the publication revision.
type delayedDeleteSink struct {
	title         string
	version       int64
	pending       func()
	deleteVersion int64
}

func (s *delayedDeleteSink) Upsert(_ context.Context, doc search.PublishedDocument) error {
	if doc.Version > s.version {
		s.title, s.version = doc.Title, doc.Version
	}
	return nil
}
func (s *delayedDeleteSink) Delete(_ context.Context, _ search.DocumentKey, version int64) error {
	s.deleteVersion = version
	s.pending = func() {
		if version > s.version {
			s.title, s.version = "", version
		}
	}
	return context.DeadlineExceeded
}

func TestIntegrationDelayedSinkDeleteCannotEraseNewerUpsert(t *testing.T) {
	for _, kind := range []string{"explicit", "missing", "empty"} {
		t.Run(kind, func(t *testing.T) { testDelayedSinkDelete(t, kind) })
	}
}

func testDelayedSinkDelete(t *testing.T, kind string) {
	ctx, pool, schema := workerFixture(t)
	docs := map[string]string{cid(1): "original"}
	opts := workerOptions(pool, schema, titles(docs))
	sink := &delayedDeleteSink{}
	opts.Sink = sink
	key := search.DocumentKey{ContentRef: gallery(cid(1)), Language: "en"}
	mark := func(deleted bool) {
		t.Helper()
		if err := search.MarkDirty(ctx, pool, schema, []search.DirtyMark{{DocumentKey: key, Deleted: deleted}}); err != nil {
			t.Fatal(err)
		}
	}
	tick := func() {
		t.Helper()
		if err := SyncOnce(ctx, opts); err != nil {
			t.Fatal(err)
		}
	}
	mark(false)
	tick()
	switch kind {
	case "explicit":
		mark(true)
	case "missing":
		delete(docs, cid(1))
		mark(false)
	case "empty":
		docs[cid(1)] = ""
		mark(false)
	}
	var deleteRevision int64
	if err := pool.QueryRow(ctx, "SELECT revision FROM "+schema+".content_search_dirty").Scan(&deleteRevision); err != nil {
		t.Fatal(err)
	}
	tick()
	if sink.deleteVersion != deleteRevision || deleteRevision <= sink.version {
		t.Fatalf("delete version %d, queue revision %d, last upsert %d", sink.deleteVersion, deleteRevision, sink.version)
	}
	if sink.pending == nil {
		t.Fatal("remote delete was not accepted")
	}
	if n := count(t, ctx, pool, schema, "content_search_dirty", "true"); n != 1 {
		t.Fatalf("timed-out deletion must remain queued: %d", n)
	}
	docs[cid(1)] = "republished"
	mark(false)
	tick()
	if sink.title != "republished" {
		t.Fatalf("new upsert failed: %q", sink.title)
	}
	sink.pending()
	if sink.title != "republished" {
		t.Fatalf("late remote delete erased the newer upsert: %q", sink.title)
	}
	if got := document(t, ctx, pool, schema); got != "republished" {
		t.Fatalf("keyword document: %q", got)
	}
}
