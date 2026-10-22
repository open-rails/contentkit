package versions_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/groups"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/versions"
)

// httpUpload presigns, PUTs and commits body through the upload API as
// actor: the committed path, or the refusing status.
func (s *site) httpUpload(actor access.Actor, ref media.RefBody, path, typ string, body []byte) (string, int) {
	s.t.Helper()
	sum := sha256.Sum256(body)
	resp, b := s.call(actor, "POST", "/media/upload/presign", media.PresignBody{Ref: ref, Path: path, Type: typ, Size: int64(len(body)),
		SHA256: hex.EncodeToString(sum[:])})
	if resp.StatusCode != http.StatusOK {
		return "", resp.StatusCode
	}
	var plan media.PresignReply
	if err := json.Unmarshal(b, &plan); err != nil {
		s.t.Fatal(err)
	}
	if plan.Put != nil {
		req, _ := http.NewRequest(plan.Put.Method, plan.Put.URL, bytes.NewReader(body))
		for k, v := range plan.Put.Headers {
			req.Header.Set(k, v)
		}
		req.ContentLength = int64(len(body))
		put, err := http.DefaultClient.Do(req)
		if err != nil {
			s.t.Fatal(err)
		}
		put.Body.Close()
	}
	resp, _ = s.call(actor, "POST", "/media/upload/commit", media.CommitBody{Ref: ref, OperationID: uuid.NewString(),
		Ops: []media.Op{{Op: media.OpPut, Path: plan.Path, Blob: plan.Blob}}})
	return plan.Path, resp.StatusCode
}

// A group kind's item holds its named covers (a channel's avatars too),
// written by staff, its owner and its scope's editors; its selected cover,
// or a member content's, renders on cards and pages.
func TestGroupMedia(t *testing.T) {
	s := newSite(t, siteOptions{})
	store, err := groups.New(groups.Options{Pool: s.pool, Schema: s.schema, Tenant: s.tenant, Kinds: siteKinds, Covers: s.svc.GroupCovers()})
	if err != nil {
		t.Fatal(err)
	}
	scope := contentref.NewID()
	editor := access.Actor{ID: contentref.NewID(), Kind: "user"}
	s.grants.scoped[scope] = editor.ID
	name := func(n string) []groups.Name {
		return []groups.Name{{Language: "en", Kind: groups.NameCanonical, Name: n}}
	}
	made, err := store.CreateGroups(s.ctx, []groups.GroupInput{{Kind: "tag", Slug: "cats", Names: name("Cats")},
		{Kind: "channel", Slug: "studio", OwnerUserID: s.creator.ID, AuthScope: scope, Names: name("Studio")}})
	if err != nil {
		t.Fatal(err)
	}
	tag, channel := made[0], made[1]
	ch, err := s.reg.Kind("channel")
	if err != nil || len(ch.Uploads) != 2 || ch.Uploads[0].Path != "cover/{name}" || !ch.Uploads[0].Named || ch.Uploads[1].Path != "avatar/{name}" {
		t.Fatalf("channel media %+v %v", ch, err)
	}
	if tk, _ := s.reg.Kind("tag"); len(tk.Uploads) != 1 {
		t.Fatalf("tag media %+v", tk.Uploads)
	}
	image := pngOf(t, 64, 48, 5)
	tagRef, chRef := media.RefBody{Kind: "tag", ID: string(tag.ID)}, media.RefBody{Kind: "channel", ID: string(channel.ID)}
	for _, c := range []struct {
		who  access.Actor
		ref  media.RefBody
		path string
		want int
	}{
		{s.stranger, tagRef, "cover/a.png", 403}, {s.creator, tagRef, "cover/a.png", 403}, {editor, tagRef, "cover/a.png", 403},
		{s.creator, chRef, "cover/a.png", 200}, {editor, chRef, "avatar/a.png", 200}, {s.stranger, chRef, "cover/a.png", 403},
	} {
		if _, got := s.httpUpload(c.who, c.ref, c.path, "image/png", image); got != c.want {
			t.Fatalf("%s %s %s: %d, want %d", c.who.ID, c.ref.Kind, c.path, got, c.want)
		}
	}
	path, code := s.httpUpload(s.staff, tagRef, "cover/photo.png", "image/png", image)
	img := strings.TrimSuffix(strings.TrimPrefix(path, "cover/"), ".png")
	if code != 200 || !strings.HasPrefix(img, media.NamedPrefix) {
		t.Fatalf("staff cover %q %d", path, code)
	}

	// The selected image renders on the group's page and card.
	if _, err := store.UpdateGroups(s.ctx, []groups.GroupUpdate{{ID: tag.ID, Cover: &groups.Cover{Image: img}}}); err != nil {
		t.Fatal(err)
	}
	var d groups.GroupDetail
	eventually(t, "the cover's renditions", 2*time.Minute, func() bool {
		if d, err = store.Detail(s.ctx, tag.ID, "en"); err != nil {
			t.Fatal(err)
		}
		return d.Cover != nil && len(d.Cover.Renditions) > 0
	})
	if u := d.Cover.Renditions[0].URL; !strings.Contains(u, "/tag/"+string(tag.ID)+"/public/cover-"+img) || d.Cover.Renditions[0].W == 0 {
		t.Fatalf("cover %+v", d.Cover)
	}
	page, err := store.List(s.ctx, groups.ListOptions{Kind: "tag"})
	if err != nil || len(page.Groups) != 1 || page.Groups[0].Cover == nil || len(page.Groups[0].Cover.Renditions) != len(d.Cover.Renditions) {
		t.Fatalf("card %+v %v", page, err)
	}

	// A member content's cover: its default edition's, the first version,
	// which follows the versions' order.
	c := s.content("gallery", `{"level":"public"}`)
	a, b := s.version(c), s.version(c)
	s.mustUpload(s.creator, s.ref("gallery", a), "cover", "image/png", pngOf(t, 64, 64, 7))
	s.mustUpload(s.creator, s.ref("gallery", b), "cover", "image/png", pngOf(t, 64, 64, 8))
	if _, err := store.UpdateGroups(s.ctx, []groups.GroupUpdate{{ID: tag.ID, Cover: &groups.Cover{ContentID: c}}}); err != nil {
		t.Fatal(err)
	}
	coverOf := func(v versions.Target) {
		t.Helper()
		eventually(t, "the member's cover from "+v.Version, 2*time.Minute, func() bool {
			d, _ = store.Detail(s.ctx, tag.ID, "en")
			return d.Cover != nil && len(d.Cover.Renditions) > 0 && strings.Contains(d.Cover.Renditions[0].URL, "/gallery/"+v.Version+"/public/cover-")
		})
	}
	coverOf(a)
	for _, table := range []string{"content_versions", "content_facets"} {
		col := map[string]string{"content_versions": "id", "content_facets": "version_id"}[table]
		s.exec(`UPDATE `+s.table(table)+` SET position = CASE WHEN `+col+` = $2 THEN 0 ELSE 1 END WHERE tenant_id = $1 AND `+col+` = ANY($3)`,
			s.tenant, b.Version, []string{a.Version, b.Version})
	}
	coverOf(b)
	got, err := s.svc.Covers(s.ctx, []versions.CoverOf{{Kind: "gallery", ID: c}, {Kind: "tag", ID: string(tag.ID), ContentID: contentref.NewID()},
		{Kind: "channel", ID: string(channel.ID)}})
	if err != nil || len(got[0]) == 0 || got[0][0].URL != d.Cover.Renditions[0].URL || got[1] != nil || got[2] != nil {
		t.Fatalf("covers %+v %v", got, err)
	}

	// Anyone sees an active group's public files; its editors its uploads.
	ref, _ := s.reg.Ref("tag", string(tag.ID))
	if g, err := s.reader.Grant(s.ctx, ref, access.Actor{Anonymous: true, IP: "203.0.113.3"}); err != nil || g.Full() || g.Editor() {
		t.Fatalf("anonymous grant: %v", err)
	}
	if _, err := store.DeleteGroups(s.ctx, []groups.GroupID{tag.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.reader.Grant(s.ctx, ref, access.Actor{Anonymous: true, IP: "203.0.113.3"}); err == nil {
		t.Fatal("a deleted group's item shows")
	}
}
