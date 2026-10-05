package media_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

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
	f := fixtureWithCommitted(t, c)
	f.visible(1)
	g := f.ref("gallery", 1)

	m := f.put(g, "originals/1.png", "image/png", png(33))
	if len(c.seen()) != 1 {
		t.Fatalf("the commit was not reported: %v", c.seen())
	}
	if state := f.reg.Config().Kinds[0].Readiness(m).State; state != media.StateProcessing {
		t.Fatalf("readiness is %v at commit time; this hook exists because it is not settled yet", state)
	}
}

func TestItemCommittedRefusalFailsTheCommit(t *testing.T) {
	c := &committed{fail: errors.New("the host said no")}
	f := fixtureWithCommitted(t, c)
	f.visible(1)
	g := f.ref("gallery", 1)

	p, blob := f.upload(g, "originals/1.png", "image/png", png(44))
	_, err := f.up.Commit(context.Background(), f.editor, g, []media.Op{{Op: media.OpPut, Path: p, Blob: blob}})
	if err == nil {
		t.Fatal("a refused ItemCommitted let the commit report success")
	}
	if !strings.Contains(err.Error(), "the host said no") {
		t.Fatalf("the commit hid why the host refused: %v", err)
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
