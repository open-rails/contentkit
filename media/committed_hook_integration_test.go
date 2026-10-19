package media_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
)

type committed struct {
	mu   sync.Mutex
	refs []string
	fail error
}

func (c *committed) hook(_ context.Context, ref contentref.ContentRef) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail != nil {
		err := c.fail
		c.fail = nil
		return err
	}
	c.refs = append(c.refs, ref.ContentKind+"/"+ref.ContentID)
	return nil
}

func (c *committed) seen() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.refs...)
}

func fixtureWithCommitted(t *testing.T, c *committed) *fixture {
	t.Helper()
	return newFixtureOn(t, s3test.Open(t), func(cfg *media.Config) { cfg.Hooks.ItemCommitted = c.hook })
}

func TestItemCommittedReportsEveryCommit(t *testing.T) {
	c := &committed{}
	f := fixtureWithCommitted(t, c)
	f.visible(1)
	g := f.ref("gallery", 1)

	f.put(g, "originals/1.png", "image/png", png(11))
	want := g.ContentKind + "/" + g.ContentID
	if seen := c.seen(); len(seen) != 1 || seen[0] != want {
		t.Fatalf("the first commit reported %v, want one %s", seen, want)
	}

	f.put(g, "originals/1.png", "image/png", png(22))
	if seen := c.seen(); len(seen) != 2 {
		t.Fatalf("a replacement reported %v, want two commits", seen)
	}
}

func TestItemCommittedFiresWhileProcessingIsPending(t *testing.T) {
	c := &committed{}
	var f *fixture
	f = newFixtureOn(t, s3test.Open(t), func(cfg *media.Config) {
		cfg.Hooks.ItemCommitted = func(ctx context.Context, ref contentref.ContentRef) error {
			// Host callbacks may themselves edit media. Holding the source's
			// folder lock during notification would deadlock this write.
			_, err := f.ms.EditExisting(ctx, ref, func(m *media.Manifest) error {
				m.Meta = map[string]any{"notified": true}
				return nil
			})
			if err != nil {
				return err
			}
			return c.hook(ctx, ref)
		}
	})
	f.visible(1)
	g := f.ref("gallery", 1)

	m := f.put(g, "originals/1.png", "image/png", png(33))
	if len(c.seen()) != 1 {
		t.Fatalf("the commit was not reported: %v", c.seen())
	}
	if state := f.reg.Config().Kinds[0].Readiness(m).State; state != media.StateProcessing {
		t.Fatalf("readiness is %v at commit time; this hook exists because it is not settled yet", state)
	}
	if m, _, err := f.ms.Get(t.Context(), g); err != nil || m.Meta["notified"] != true {
		t.Fatalf("reentrant host notification was lost: %v", err)
	}
}

func TestItemCommittedRefusalFailsTheCommit(t *testing.T) {
	c := &committed{fail: errors.New("the host said no")}
	f := fixtureWithCommitted(t, c)
	f.visible(1)
	g := f.ref("gallery", 1)

	p, blob := f.upload(g, "originals/1.png", "image/png", png(44))
	_, err := f.up.Commit(context.Background(), f.editor, g, uuid.NewString(), []media.Op{{Op: media.OpPut, Path: p, Blob: blob}})
	if err == nil {
		t.Fatal("a refused ItemCommitted let the commit report success")
	}
	if !strings.Contains(err.Error(), "the host said no") {
		t.Fatalf("the commit hid why the host refused: %v", err)
	}
	if jobs := f.q.take(); len(jobs) != 1 {
		t.Fatalf("host notification failure lost processing: %+v", jobs)
	}
	// Restart with the same database, not the request's in-memory mutation.
	journal, err := media.NewPGJournal(f.env.Pool(), f.env.ContentSchema(), f.q.Queue)
	if err != nil {
		t.Fatal(err)
	}
	restarted := s3test.Manifests(t, f.env.Store, f.reg, media.ManifestOptions{Journal: journal})
	for range 2 {
		if err := restarted.RecoverPending(t.Context(), 100); err != nil {
			t.Fatal(err)
		}
	}
	if len(c.seen()) != 1 || len(f.q.take()) != 0 {
		t.Fatalf("notification was lost or replayed after acknowledgement: %v", c.seen())
	}
}

func TestRecoveryOrdersUnfinishedThenRotatedNotifications(t *testing.T) {
	c := &committed{}
	f := fixtureWithCommitted(t, c)
	ctx := t.Context()
	refused := errors.New("host unavailable")
	var refs [3]contentref.ContentRef
	for i := range refs {
		f.visible(i + 1)
		refs[i] = f.ref("gallery", i+1)
	}
	// Two refused commits leave two pending notifications, the first older.
	for i, ref := range refs[:2] {
		c.fail = refused
		p, blob := f.upload(ref, "originals/1.png", "image/png", png(60+i))
		if _, err := f.up.Commit(ctx, f.editor, ref, uuid.NewString(), []media.Op{{Op: media.OpPut, Path: p, Blob: blob}}); !errors.Is(err, refused) {
			t.Fatalf("refused commit: %v", err)
		}
	}
	// A newer unfinished commit is still recovered before either notification.
	item, err := f.reg.Item(refs[2])
	if err != nil {
		t.Fatal(err)
	}
	table := pgx.Identifier{f.env.ContentSchema(), "content_media_commits"}.Sanitize()
	if _, err := f.env.Pool().Exec(ctx, `INSERT INTO `+table+`
(tenant_id, operation_id, content_kind, content_id, folder_prefix, actor_id, fingerprint, lease_id)
VALUES ($1, gen_random_uuid(), $2, $3, $4, '', decode(repeat('00', 32), 'hex'), gen_random_uuid())`,
		refs[2].TenantID, refs[2].ContentKind, refs[2].ContentID, item.Prefix()); err != nil {
		t.Fatal(err)
	}
	recoverOne := func() error { return f.ms.RecoverPending(ctx, 1) }
	if err := recoverOne(); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := f.env.Pool().QueryRow(ctx, `SELECT state FROM `+table+` WHERE tenant_id = $1 AND folder_prefix = $2`,
		refs[2].TenantID, item.Prefix()).Scan(&state); err != nil || state != "absent" {
		t.Fatalf("unfinished commit is %q, want absent: %v", state, err)
	}
	if seen := c.seen(); len(seen) != 0 {
		t.Fatalf("notifications %v were delivered before the unfinished commit", seen)
	}

	// A failed delivery rotates behind the other pending notification.
	c.fail = refused
	if err := recoverOne(); !errors.Is(err, refused) {
		t.Fatalf("oldest notification was not attempted first: %v", err)
	}
	for range 2 {
		if err := recoverOne(); err != nil {
			t.Fatal(err)
		}
	}
	name := func(r contentref.ContentRef) string { return r.ContentKind + "/" + r.ContentID }
	if seen := c.seen(); !slices.Equal(seen, []string{name(refs[1]), name(refs[0])}) {
		t.Fatalf("notifications delivered as %v, want the refused one last", seen)
	}
}

func TestCommitsWorkWithoutTheHook(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	g := f.ref("gallery", 1)

	if m := f.put(g, "originals/1.png", "image/png", png(55)); len(m.Files) == 0 {
		t.Fatal("a commit without ItemCommitted stored nothing")
	}
}
