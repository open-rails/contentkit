package versions_test

import (
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
	"github.com/open-rails/contentkit/media/token"
	"github.com/open-rails/contentkit/media/versions"
)

// A content kind's media lives in a content-level folder (its cover) and one
// folder per version, each its own item with its own token.
func TestVersionFolders(t *testing.T) {
	s := newSite(t, siteOptions{})
	c := s.content("gallery", `{"level":"public"}`)
	v1, v2 := s.version(c), s.version(c)
	top, r1, r2 := s.ref("gallery", versions.Target{Content: c}), s.ref("gallery", v1), s.ref("gallery", v2)
	for ref, want := range map[contentref.ContentRef]string{top: s.tenant + "/gallery/" + c + "/", r1: s.tenant + "/gallery/" + c + "/" + v1.Version + "/"} {
		item, err := s.reg.Item(ref)
		if err != nil || item.Prefix() != want {
			t.Fatalf("%s: %q %v", ref, item.Prefix(), err)
		}
	}
	s.mustUpload(s.creator, top, "cover", "image/png", pngOf(t, 64, 64, 1))
	s.mustUpload(s.creator, r1, "originals/001", "image/png", pngOf(t, 30, 40, 2))
	s.mustUpload(s.creator, r2, "originals/001", "image/png", pngOf(t, 30, 40, 3))
	for _, wrong := range []struct {
		ref  contentref.ContentRef
		path string
	}{{top, "originals/002"}, {r1, "cover"}} {
		if err := s.upload(s.creator, wrong.ref, wrong.path, "image/png", pngOf(t, 8, 8, 4)); err == nil {
			t.Fatalf("%s took %s", wrong.ref, wrong.path)
		}
	}
	if _, err := s.reg.VersionRef("user", s.creator.ID, v1.Version); err == nil {
		t.Fatal("a kind without versions took a versioned ref")
	}

	// Every object belongs to exactly one of the three folders.
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
	if len(folders) != 3 {
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
	if err := ring.VerifyPrivate(tok, s.tenant+"/gallery/"+c+"/private/"+other, time.Now()); err == nil {
		t.Fatal("a version's token opened its content's item")
	}
	if resp, body := s.call(access.Actor{}, "GET", "/media/gallery/"+c, nil); resp.StatusCode != 200 || !strings.Contains(string(body), `"preset":"cover"`) {
		t.Fatalf("content read: %d %s", resp.StatusCode, body)
	}

	// Deleting a version takes its folder alone; deleting the content takes the rest.
	del := func(targets ...versions.Target) {
		t.Helper()
		tx, err := s.pool.Begin(s.ctx)
		if err != nil {
			t.Fatal(err)
		}
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
	if m := s.manifest(top); m.Deleted || len(m.Files) == 0 {
		t.Fatal("deleting a version took its content's item")
	}
	del(versions.Target{Content: c})
	eventually(t, "content deleted", time.Minute, func() bool { return gone(r1)() && gone(top)() })
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
	if strings.Join(paths, ",") != "source,subs/{name},poster" {
		t.Fatalf("version uploads %v", paths)
	}
	if p := item.Kind().PrivateFor("source"); len(p) != 2 || p[0].HLS == nil || len(p[0].HLS.Ladder) != 2 {
		t.Fatalf("version presets %+v", p)
	}
	top, err := s.reg.Item(s.ref("movie", versions.Target{Content: c}))
	if err != nil || len(top.Kind().Uploads) != 1 || top.Kind().Uploads[0].Path != "cover" {
		t.Fatalf("content-level uploads %+v %v", top.Kind().Uploads, err)
	}
	s.mustUpload(s.creator, s.ref("movie", v), "subs/en", "text/vtt", []byte("WEBVTT\n\n00:00.000 --> 00:01.000\nhi\n"))
	for actor, full := range map[*access.Actor]bool{&s.fan: true, &s.stranger: false} {
		g, err := s.reader.Grant(s.ctx, s.ref("movie", v), *actor)
		if err != nil || g.Full() != full {
			t.Fatalf("%s: full %v, want %v (%v)", actor.ID, g != nil && g.Full(), full, err)
		}
	}
}
