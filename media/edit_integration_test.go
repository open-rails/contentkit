package media_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
)

func TestFileCeilings(t *testing.T) {
	e := newUploadEnv(t, nil, nil)
	ref := media.RefBody{Kind: "mixed", ID: cid(1), Version: "v1"}
	png := func(seed uint64) string { return e.upload(t, "alice", ref, "image/png", data(seed, 1000)) }
	mp4 := func(seed uint64) string { return e.upload(t, "alice", ref, "video/mp4", data(seed, 2<<20)) }

	// A video over the kind's 1 MiB cap is allowed by its type limit.
	if status, _, er := e.commit(t, "alice", ref, insert("a.png", png(1)), insert("clip.mp4", mp4(2))); status != 200 {
		t.Fatalf("commit: %d %+v", status, er)
	}
	status, _, er := e.commit(t, "alice", ref, insert("clip2.mp4", mp4(3)))
	if status != http.StatusConflict || er.Code != media.CodeTooManyFiles {
		t.Fatalf("second video: %d %+v", status, er)
	}
	if status, _, er := e.commit(t, "alice", ref, insert("b.png", png(4))); status != 200 {
		t.Fatalf("third file: %d %+v", status, er)
	}
	if status, _, er := e.commit(t, "alice", ref, insert("c.png", png(5))); status != http.StatusConflict || er.Code != media.CodeTooManyFiles {
		t.Fatalf("fourth file: %d %+v", status, er)
	}
	// At the cap, swapping a file in one commit is allowed, and nothing was written by the refusals.
	status, out, er := e.commit(t, "alice", ref, media.Op{Op: media.OpRemove, Name: "b.png"}, insert("c.png", png(5)))
	if status != 200 || len(out.Files) != 3 || out.Files[2].Name != "c.png" {
		t.Fatalf("swap: %d %+v %+v", status, er, out)
	}
}

func TestEditOpAndSlotFromFile(t *testing.T) {
	e := newUploadEnv(t, nil, nil)
	ctx := context.Background()
	ref := media.RefBody{Kind: "mixed", ID: cid(2), Version: "v1"}
	page := data(10, 3000)
	pageName := e.upload(t, "alice", ref, "image/png", page)
	clip := e.upload(t, "alice", ref, "video/mp4", data(11, 5000))
	if status, _, er := e.commit(t, "alice", ref, insert("p.png", pageName), insert("clip.mp4", clip)); status != 200 {
		t.Fatalf("commit: %d %+v", status, er)
	}
	crop := func(x, y, w, h int) *media.Edit { return &media.Edit{Crop: &media.Crop{X: x, Y: y, W: w, H: h}} }

	for name, tc := range map[string]struct {
		op   media.Op
		code string
	}{
		"video":      {media.Op{Op: media.OpEdit, Name: "clip.mp4", Edit: &media.Edit{Rotate: 90}}, media.CodeInvalid},
		"bad rotate": {media.Op{Op: media.OpEdit, Name: "p.png", Edit: &media.Edit{Rotate: 45}}, media.CodeInvalid},
		"empty crop": {media.Op{Op: media.OpEdit, Name: "p.png", Edit: crop(0, 0, 0, 10)}, media.CodeInvalid},
		"no file":    {media.Op{Op: media.OpEdit, Name: "missing.png", Edit: &media.Edit{Rotate: 90}}, media.CodeNotFound},
		"on move":    {media.Op{Op: media.OpMove, Name: "p.png", Index: new(int), Edit: &media.Edit{Rotate: 90}}, media.CodeInvalid},
	} {
		if _, _, er := e.commit(t, "alice", ref, tc.op); er.Code != tc.code {
			t.Errorf("%s: %+v, want %s", name, er, tc.code)
		}
	}

	// Before processing records the page's size only the shape is checked; once
	// it is known, a crop outside it is refused.
	status, out, er := e.commit(t, "alice", ref, media.Op{Op: media.OpEdit, Name: "p.png", Edit: crop(10, 20, 900, 900)})
	if status != 200 || out.Files[0].Edit == nil || out.Files[0].Edit.Crop.W != 900 {
		t.Fatalf("edit: %d %+v %+v", status, er, out)
	}
	cref := contentref.NewVersion(e.Tenant, "mixed", cid(2), "v1")
	if _, err := e.manifests.Edit(ctx, cref, func(m *media.Manifest) error {
		m.Files[0].Dims, m.Files[0].Edit = &media.Dims{W: 400, H: 200}, nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, er := e.commit(t, "alice", ref, media.Op{Op: media.OpEdit, Name: "p.png", Edit: crop(300, 0, 200, 100)}); er.Code != media.CodeInvalid {
		t.Fatalf("out of bounds: %+v", er)
	}
	if status, _, er := e.commit(t, "alice", ref, media.Op{Op: media.OpEdit, Name: "p.png", Edit: &media.Edit{Rotate: 180}}); status != 200 {
		t.Fatalf("rotate: %d %+v", status, er)
	}
	// Identity clears.
	if _, out, _ := e.commit(t, "alice", ref, media.Op{Op: media.OpEdit, Name: "p.png", Edit: &media.Edit{}}); out.Files[0].Edit != nil {
		t.Fatalf("identity edit kept: %+v", out.Files[0].Edit)
	}

	// Slot from file: the page's bytes become the slot original; the crop's
	// height follows the slot's 1:2 aspect.
	e.queue.jobs = nil
	slotBody := func(edit *media.Edit) media.SlotFromFileBody {
		return media.SlotFromFileBody{Ref: ref, Slot: "cover", File: "p.png", Edit: edit}
	}
	var sm media.SlotManifest
	if status, er := e.call(t, "alice", "/commit-slot-from-file", slotBody(crop(200, 0, 100, 0)), &sm); status != 200 || !sm.Pending || sm.Aspect != media.Ratio("1:2") {
		t.Fatalf("slot from file: %d %+v %+v", status, er, sm)
	}
	slotEdit := func() string {
		t.Helper()
		rec, err := e.manifests.Slot(ctx, contentref.New(e.Tenant, "mixed", cid(2)), "cover")
		if err != nil {
			t.Fatal(err)
		}
		if rec.Edit == nil {
			return ""
		}
		b, _ := json.Marshal(rec.Edit)
		return string(b)
	}
	rec, err := e.manifests.Slot(ctx, contentref.New(e.Tenant, "mixed", cid(2)), "cover")
	if err != nil {
		t.Fatal(err)
	}
	rc, obj, err := e.Store.Get(ctx, e.Tenant+"/mixed/"+cid(2)+"/originals/"+rec.Original, media.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != string(page) || obj.ContentType != "image/png" || slotEdit() != `{"crop":{"x":200,"y":0,"w":100,"h":200}}` ||
		*sm.Edit.Crop != (media.Crop{X: 200, Y: 0, W: 100, H: 200}) {
		t.Fatalf("slot original: %d bytes %+v, edit %s", len(got), obj, slotEdit())
	}
	if len(e.queue.jobs) != 1 || e.queue.jobs[0].Slot != "cover" || e.queue.jobs[0].Ref.Version() != "" {
		t.Fatalf("jobs: %+v", e.queue.jobs)
	}
	for name, tc := range map[string]struct {
		body   media.SlotFromFileBody
		status int
	}{
		"outside":   {slotBody(crop(350, 0, 100, 0)), 400},
		"video":     {media.SlotFromFileBody{Ref: ref, Slot: "cover", File: "clip.mp4"}, 400},
		"no file":   {media.SlotFromFileBody{Ref: ref, Slot: "cover", File: "nope.png"}, 404},
		"no slot":   {media.SlotFromFileBody{Ref: ref, Slot: "banner", File: "p.png"}, 404},
		"forbidden": {slotBody(nil), 403},
	} {
		actor := "alice"
		if name == "forbidden" {
			actor = "reader"
		}
		if status, er := e.call(t, actor, "/commit-slot-from-file", tc.body, nil); status != tc.status {
			t.Errorf("%s: %d %+v", name, status, er)
		}
	}
	// No edit given: the file's own edit is used; an empty one clears it.
	if status, _, er := e.commit(t, "alice", ref, media.Op{Op: media.OpEdit, Name: "p.png", Edit: &media.Edit{Rotate: 90}}); status != 200 {
		t.Fatalf("rotate: %d %+v", status, er)
	}
	for _, tc := range []struct {
		edit *media.Edit
		want string
	}{{nil, `{"rotate":90}`}, {&media.Edit{}, ""}} {
		if status, er := e.call(t, "alice", "/commit-slot-from-file", slotBody(tc.edit), nil); status != 200 {
			t.Fatalf("slot from file: %d %+v", status, er)
		}
		if got := slotEdit(); got != tc.want {
			t.Fatalf("slot edit %q, want %q", got, tc.want)
		}
	}
}
