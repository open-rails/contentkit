package versions_test

import (
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
	"github.com/open-rails/contentkit/media/token"
	"github.com/open-rails/contentkit/media/versions"
)

// A content kind's media lives in one folder per version, each its own item
// with its own token; the content's folder holds no item.
func TestVersionFolders(t *testing.T) {
	s := newSite(t, siteOptions{})
	c := s.content("gallery", `{"level":"public"}`)
	v1, v2 := s.version(c), s.version(c)
	r1, r2 := s.ref("gallery", v1), s.ref("gallery", v2)
	if item, err := s.reg.Item(r1); err != nil || item.Prefix() != s.tenant+"/gallery/"+c+"/"+v1.Version+"/" {
		t.Fatalf("%s: %q %v", r1, item.Prefix(), err)
	}
	if _, err := s.reg.VersionRef("gallery", c, ""); err == nil {
		t.Fatal("a content took an item of its own")
	}
	s.mustUpload(s.creator, r1, "cover", "image/png", pngOf(t, 64, 64, 1))
	s.mustUpload(s.creator, r1, "originals/001", "image/png", pngOf(t, 30, 40, 2))
	s.mustUpload(s.creator, r2, "originals/001", "image/png", pngOf(t, 30, 40, 3))
	if _, err := s.reg.VersionRef("user", s.creator.ID, v1.Version); err == nil {
		t.Fatal("a kind without versions took a versioned ref")
	}

	// Every object belongs to exactly one of the versions' folders.
	folders := map[string]int{}
	for o, err := range s.Store.List(s.ctx, s.tenant+"/gallery/"+c+"/") {
		if err != nil {
			t.Fatal(err)
		}
		k, ok := layout.Parse(o.Key)
		if !ok {
			t.Fatalf("unparsed key %s", o.Key)
		}
		folders[k.Folder()]++
	}
	if len(folders) != 2 {
		t.Fatalf("folders %v", folders)
	}

	// A version's read grants its folder alone.
	resp, body := s.call(s.creator, "GET", "/media/gallery/"+c+"/versions/"+v1.Version+"?editor", nil)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"path":"originals/001.png"`) {
		t.Fatalf("version read: %d %s", resp.StatusCode, body)
	}
	eventually(t, "a rendition", time.Minute, func() bool {
		f, ok := s.manifest(r1).Get("low-res/001.webp")
		return ok && f.Blob != ""
	})
	g, err := s.reader.Grant(s.ctx, r1, s.creator)
	if err != nil || !g.Full() {
		t.Fatalf("editor grant: %v", err)
	}
	f, _ := g.Manifest.Get("low-res/001.webp")
	u, err := g.URL(f, false)
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(u)
	key := strings.TrimPrefix(parsed.Path, layout.URLPrefix)
	ring, _ := token.NewRing(signer, nil)
	tok := parsed.Query().Get("t")
	if err := ring.VerifyPrivate(tok, key, time.Now()); err != nil {
		t.Fatalf("version token on its blob: %v", err)
	}
	other := s.manifest(r2).Files[0].Blob
	if err := ring.VerifyPrivate(tok, s.tenant+"/gallery/"+c+"/"+v2.Version+"/private/"+other, time.Now()); err == nil {
		t.Fatal("a version's token opened its sibling")
	}
	if resp, body := s.call(access.Actor{}, "GET", "/media/gallery/"+c+"/versions/"+v1.Version, nil); resp.StatusCode != 200 ||
		!strings.Contains(string(body), `"preset":"cover"`) {
		t.Fatalf("version read: %d %s", resp.StatusCode, body)
	}
	if resp, _ := s.call(access.Actor{}, "GET", "/media/gallery/"+c, nil); resp.StatusCode < 400 {
		t.Fatalf("content-level read: %d", resp.StatusCode)
	}

	// Deleting a version takes its folder alone; deleting the content takes the rest.
	del := func(targets ...versions.Target) {
		t.Helper()
		tx, err := s.pool.Begin(s.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(s.ctx)
		if err := s.svc.DeleteTx(s.ctx, tx, targets); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(s.ctx); err != nil {
			t.Fatal(err)
		}
	}
	gone := func(ref contentref.ContentRef) func() bool {
		return func() bool {
			m, _, err := s.ms.Get(s.ctx, ref)
			return err == nil && m.Deleted || errors.Is(err, media.ErrNotFound)
		}
	}
	del(v2)
	eventually(t, "version deleted", time.Minute, gone(r2))
	if m := s.manifest(r1); m.Deleted || len(m.Files) == 0 {
		t.Fatal("deleting a version took its sibling")
	}
	del(versions.Target{Content: c})
	eventually(t, "content deleted", time.Minute, gone(r1))
}

// Deleting a content waits for a publisher holding its version, as a
// promotion does while it retires a publication the deletion retires too,
// instead of deadlocking with it.
func TestDeleteWaitsForPublisher(t *testing.T) {
	s := newSite(t, siteOptions{})
	c := s.content("gallery", `{"level":"public"}`)
	v := s.version(c)
	s.mustUpload(s.creator, s.ref("gallery", v), "originals/001", "image/png", pngOf(t, 30, 40, 1))
	pubs, items := s.table("content_version_publications"), s.table("content_version_publication_items")
	var pub, item string
	eventually(t, "a current publication and a drained queue", 2*time.Minute, func() bool {
		var busy bool
		err := s.pool.QueryRow(s.ctx, `SELECT p.id, i.id, EXISTS (SELECT 1 FROM `+pgx.Identifier{s.host, "river_job"}.Sanitize()+`
 WHERE state NOT IN ('completed', 'cancelled', 'discarded') AND kind <> 'contentkit_media_sweep')
FROM `+pubs+` p JOIN `+items+` i ON i.tenant_id = p.tenant_id AND i.publication_id = p.id
WHERE p.tenant_id = $1 AND p.version_id = $2 AND p.state = 'current'`, s.tenant, v.Version).Scan(&pub, &item, &busy)
		return err == nil && !busy
	})

	// A publisher holds the version, then its publication, as promoteTx does.
	publisher, err := s.pool.Begin(s.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer publisher.Rollback(s.ctx)
	var publisherPID uint32
	if err := publisher.QueryRow(s.ctx, `SELECT pg_backend_pid()`).Scan(&publisherPID); err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.Exec(s.ctx, `SELECT FROM `+s.table("content_versions")+` WHERE tenant_id = $1 AND id = $2 FOR UPDATE`, s.tenant, v.Version); err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.Exec(s.ctx, `SELECT FROM `+pubs+` WHERE tenant_id = $1 AND id = $2 FOR UPDATE`, s.tenant, pub); err != nil {
		t.Fatal(err)
	}

	deleter, err := s.pool.Begin(s.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer deleter.Rollback(s.ctx)
	var deleterPID uint32
	if err := deleter.QueryRow(s.ctx, `SELECT pg_backend_pid()`).Scan(&deleterPID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.svc.DeleteTx(s.ctx, deleter, []versions.Target{{Content: c}}) }()
	for blocked := false; !blocked; time.Sleep(10 * time.Millisecond) {
		select {
		case err := <-done:
			t.Fatalf("deletion did not wait for the publisher: %v", err)
		default:
		}
		if err := s.pool.QueryRow(s.ctx, `SELECT $2::int = ANY(pg_blocking_pids($1::int))`, deleterPID, publisherPID).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
	}

	// The publisher retires that publication's item, then commits.
	if err := s.jobs.DeleteItemsTx(s.ctx, publisher, media.Deletion{Ref: contentref.New(s.tenant, "gallery.published", item)}); err != nil {
		t.Fatalf("publisher: %v", err)
	}
	if err := publisher.Commit(s.ctx); err != nil {
		t.Fatalf("publisher: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("deletion: %v", err)
	}
	if err := deleter.Commit(s.ctx); err != nil {
		t.Fatal(err)
	}
}

// Editors of a content write its items: staff and its creators; a person
// writes their own avatar; nobody but publication writes published items
// and preview copies.
func TestUploadRights(t *testing.T) {
	s := newSite(t, siteOptions{})
	c := s.content("gallery", `{"level":"public"}`)
	v := s.version(c)
	ref := s.ref("gallery", v)
	page := pngOf(t, 16, 16, 9)
	code := func(err error) string {
		if ue, ok := media.AsUploadError(err); ok {
			return ue.Code
		}
		if err != nil {
			return err.Error()
		}
		return ""
	}
	for _, a := range []access.Actor{s.creator, s.staff} {
		if err := s.upload(a, ref, "originals/"+a.ID[:8], "image/png", page); err != nil {
			t.Fatalf("editor %s: %v", a.ID, err)
		}
	}
	other := s.version(s.content("gallery", `{"level":"public"}`))
	s.exec(`DELETE FROM `+s.table("content_creators")+` WHERE tenant_id = $1 AND content_id = $2`, s.tenant, other.Content)
	for name, try := range map[string]func() error{
		"stranger": func() error { return s.upload(s.stranger, ref, "originals/x", "image/png", page) },
		"anonymous": func() error {
			return s.upload(access.Actor{Anonymous: true, IP: "203.0.113.9"}, ref, "originals/x", "image/png", page)
		},
		"preview copy":    func() error { return s.upload(s.creator, ref, "previews/1", "image/webp", page) },
		"another content": func() error { return s.upload(s.creator, s.ref("gallery", other), "originals/x", "image/png", page) },
		"a stray version": func() error {
			return s.upload(s.creator, s.ref("gallery", versions.Target{Content: c, Version: other.Version}), "originals/x", "image/png", page)
		},
		"published item": func() error {
			return s.upload(s.staff, contentref.New(s.tenant, "gallery.published", contentref.NewID()), "low-res/x", "image/webp", page)
		},
		"another's avatar": func() error {
			return s.upload(s.stranger, contentref.New(s.tenant, "user", s.creator.ID), "avatar", "image/png", page)
		},
	} {
		if got := code(try()); got != media.CodeForbidden {
			t.Errorf("%s: %q, want forbidden", name, got)
		}
	}
	if err := s.upload(s.stranger, contentref.New(s.tenant, "user", s.stranger.ID), "avatar", "image/png", page); err != nil {
		t.Fatalf("own avatar: %v", err)
	}
}

// A version's media writes its facts: pages and their size, and audio; the
// catalog refreshes its facets.
func TestDerivedFacts(t *testing.T) {
	s := newSite(t, siteOptions{})
	v := s.version(s.content("gallery", `{"level":"public"}`))
	ref := s.ref("gallery", v)
	s.mustUpload(s.creator, ref, "originals/001", "image/png", pngOf(t, 30, 40, 1))
	s.mustUpload(s.creator, ref, "originals/002", "image/png", pngOf(t, 50, 20, 2))
	s.mustUpload(s.creator, ref, "audio/theme", "audio/wav", wav(t))
	type facts struct {
		pages, w, h *int
		audio       bool
	}
	var f facts
	eventually(t, "facts", 2*time.Minute, func() bool {
		if err := s.pool.QueryRow(s.ctx, `SELECT pages, width, height, has_audio FROM `+s.table("content_versions")+` WHERE tenant_id = $1 AND id = $2`,
			s.tenant, v.Version).Scan(&f.pages, &f.w, &f.h, &f.audio); err != nil {
			t.Fatal(err)
		}
		return f.pages != nil && *f.pages == 2 && f.w != nil && *f.w == 30 && *f.h == 40 && f.audio
	})
	if !s.cat.wasRefreshed(v.Version) {
		t.Fatal("facets were not refreshed")
	}
	s.commit(s.creator, ref, media.Op{Op: media.OpRemove, Path: "originals/001.png"})
	eventually(t, "facts after a removal", time.Minute, func() bool {
		_ = s.pool.QueryRow(s.ctx, `SELECT pages, width FROM `+s.table("content_versions")+` WHERE tenant_id = $1 AND id = $2`,
			s.tenant, v.Version).Scan(&f.pages, &f.w)
		return *f.pages == 1 && f.w != nil && *f.w == 50
	})
}

// A delayed facts callback records the manifest current once it holds the
// version, never the older one current when it was called, so it cannot
// overwrite a newer commit's facts.
func TestDelayedCallbackKeepsNewerFacts(t *testing.T) {
	s := newSite(t, siteOptions{})
	hooks := s.svc.Hooks()
	for _, c := range []struct {
		name string
		call func(contentref.ContentRef) error
	}{
		{"ItemCommitted", func(ref contentref.ContentRef) error { return hooks.ItemCommitted(s.ctx, ref) }},
		{"ItemReady", func(ref contentref.ContentRef) error {
			return pgx.BeginFunc(s.ctx, s.pool, func(tx pgx.Tx) error {
				return hooks.ItemReady(s.ctx, tx, ref, media.Readiness{State: media.StateReady})
			})
		}},
	} {
		v := s.version(s.content("gallery", `{"level":"public"}`))
		ref := s.ref("gallery", v)
		// Hold off the publications the callbacks queue: they record facts too.
		unlock, err := versions.LockSource(s.ctx, s.pool, s.tenant, v.Version)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(unlock)
		blob, err := s.ms.NewBlobs(s.ctx, ref, layout.AreaPrivate, [][]byte{make([]byte, 32)})
		if err != nil {
			t.Fatal(err)
		}
		page := func(w int) {
			t.Helper()
			if _, err := s.ms.Edit(s.ctx, ref, func(m *media.Manifest) error {
				m.Files = []media.File{{Path: "originals/001.png", Blob: blob[0], Type: "image/png", W: w, H: w}}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		width := func() int {
			t.Helper()
			var w *int
			if err := s.pool.QueryRow(s.ctx, `SELECT width FROM `+s.table("content_versions")+` WHERE tenant_id = $1 AND id = $2`,
				s.tenant, v.Version).Scan(&w); err != nil || w == nil {
				t.Fatalf("%s: width %v %v", c.name, w, err)
			}
			return *w
		}
		page(10)
		if err := c.call(ref); err != nil || width() != 10 {
			t.Fatalf("%s: first callback: %v", c.name, err)
		}

		// The old callback is delayed behind a newer one, which holds the
		// version while its commit lands and records that commit's facts.
		newer, pid := s.lockVersion(v, true)
		done := make(chan error, 1)
		go func() { done <- c.call(ref) }()
		if !s.waitBlocked(pid, done) {
			t.Fatalf("%s: the delayed callback did not wait for the newer one", c.name)
		}
		page(20)
		if _, err := newer.Exec(s.ctx, `UPDATE `+s.table("content_versions")+` SET width = 20, height = 20 WHERE tenant_id = $1 AND id = $2`,
			s.tenant, v.Version); err != nil {
			t.Fatal(err)
		}
		if err := newer.Commit(s.ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatalf("%s: delayed callback: %v", c.name, err)
		}
		if w := width(); w != 20 {
			t.Errorf("%s: the delayed callback recorded width %d over the newer 20", c.name, w)
		}
	}
}

// Page selection records the manifest current once it holds the version,
// not the one its own edit wrote, so a commit landing in between keeps its
// facts.
func TestPageSelectionKeepsNewerFacts(t *testing.T) {
	s := newSite(t, siteOptions{})
	v := s.version(s.content("gallery", `{"level":"public"}`))
	ref := s.ref("gallery", v)
	s.mustUpload(s.creator, ref, "originals/001", "image/png", pngOf(t, 30, 40, 1))
	width := func() int {
		t.Helper()
		var w *int
		if err := s.pool.QueryRow(s.ctx, `SELECT width FROM `+s.table("content_versions")+` WHERE tenant_id = $1 AND id = $2`,
			s.tenant, v.Version).Scan(&w); err != nil {
			t.Fatal(err)
		}
		if w == nil {
			return 0
		}
		return *w
	}
	eventually(t, "facts", time.Minute, func() bool { return width() == 30 })
	// Background publications record facts too; start once none is queued
	// (delayed sweeps record none).
	idle := func() bool {
		t.Helper()
		var n int
		if err := s.pool.QueryRow(s.ctx, `SELECT (SELECT count(*) FROM `+pgx.Identifier{s.workers, "river_job"}.Sanitize()+` WHERE state NOT IN ('completed', 'cancelled', 'discarded') AND kind <> 'contentkit_media_sweep')
 + (SELECT count(*) FROM `+pgx.Identifier{s.host, "river_job"}.Sanitize()+` WHERE state NOT IN ('completed', 'cancelled', 'discarded') AND kind <> 'contentkit_media_sweep')`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 0
	}
	eventually(t, "the queues to drain", 2*time.Minute, idle)

	// A newer commit's callback holds the version while that commit lands,
	// after page selection wrote its edit and before it records facts.
	newer, pid := s.lockVersion(v, true)
	done := make(chan error, 1)
	go func() { done <- versions.Publish(s.svc, s.ctx, v) }()
	if !s.waitBlocked(pid, done) {
		t.Fatal("page selection did not wait for the newer callback")
	}
	var waiting string
	if err := s.pool.QueryRow(s.ctx, `SELECT query FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))`, pid).Scan(&waiting); err != nil ||
		!strings.Contains(waiting, "content_items") || !strings.Contains(waiting, "FOR UPDATE") {
		t.Fatalf("waiting elsewhere than recording facts: %q %v", waiting, err)
	}
	if !idle() {
		t.Fatal("a queued job, not this page selection, may be the one waiting")
	}
	if _, err := s.ms.EditExisting(s.ctx, ref, func(m *media.Manifest) error {
		for i := range m.Files {
			if m.Files[i].IsUpload() && strings.HasPrefix(m.Files[i].Path, "originals/001") {
				m.Files[i].W, m.Files[i].H = 60, 80
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := newer.Exec(s.ctx, `UPDATE `+s.table("content_versions")+` SET width = 60, height = 80 WHERE tenant_id = $1 AND id = $2`,
		s.tenant, v.Version); err != nil {
		t.Fatal(err)
	}
	if err := newer.Commit(s.ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil && !errors.Is(err, versions.ErrNotReady) {
		t.Fatal(err)
	}
	if w := width(); w != 60 {
		t.Errorf("page selection recorded width %d over the newer 60", w)
	}
}

// A movie's versions each carry their own ladder, subtitles and poster, and
// open like their version: a kind that publishes nothing grants its
// versions' items.
func TestMovieVersions(t *testing.T) {
	s := newSite(t, siteOptions{})
	c := s.content("movie", `{"level":"tier","tiers":["premium"]}`)
	v := s.version(c)
	item, err := s.reg.Item(s.ref("movie", v))
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, r := range item.Kind().UploadRules() {
		paths = append(paths, r.Path)
	}
	if strings.Join(paths, ",") != "cover,source,subs/{name},poster" {
		t.Fatalf("version uploads %v", paths)
	}
	if p := item.Kind().PrivateFor("source"); len(p) != 2 || p[0].HLS == nil || len(p[0].HLS.Ladder) != 2 {
		t.Fatalf("version presets %+v", p)
	}
	s.mustUpload(s.creator, s.ref("movie", v), "subs/en", "text/vtt", []byte("WEBVTT\n\n00:00.000 --> 00:01.000\nhi\n"))
	for actor, full := range map[*access.Actor]bool{&s.fan: true, &s.stranger: false} {
		g, err := s.reader.Grant(s.ctx, s.ref("movie", v), *actor)
		if err != nil || g.Full() != full {
			t.Fatalf("%s: full %v, want %v (%v)", actor.ID, g != nil && g.Full(), full, err)
		}
	}
}
