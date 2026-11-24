package critical_test

import (
	"bytes"
	"image"
	"image/png"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/media"
)

// withPosters gives the tiny video kinds a poster upload grabbed from their
// video and rendered by an image preset, one more still at most, and an
// image upload that takes no frames.
func withPosters(c *ck.Config) {
	for i := range c.Media.Config.Kinds {
		k := &c.Media.Config.Kinds[i]
		if k.Name == "clip" || k.Name == "siteclip" {
			k.Uploads = append(k.Uploads, media.Upload{Path: "poster", Types: []string{"image/png"}, MaxBytes: 1 << 20, Frames: "source"},
				media.Upload{Path: "stills/{name}", Types: []string{"image/png"}, MaxBytes: 1 << 20, Max: 1, Frames: "source"},
				media.Upload{Path: "art", Types: []string{"image/png"}, MaxBytes: 1 << 20})
			k.Private = append(k.Private, media.Private{Name: "poster", From: "poster", To: "posters/poster.webp", Image: &media.Image{Width: 64, Height: 36}})
		}
	}
}

// Posters come from frame ops the actual worker grabs with ffmpeg from the
// encoded video: the item's own, or another item's (of any kind in the
// namespace) that the editor may also edit. Hosts decode nothing.
func TestMediaFramePosters(t *testing.T) {
	f := newTinyHLSConfiguredFixture(t, false, func(c *ck.Config) {
		withPosters(c)
		c.Content.Channels.Uploads.QuotaBytes = 1 << 30 // enable the shared usage ledger
	})
	admin := f.app.engine.Client()
	frame := func(n int) image.Image { return sourceFrame(t, f.Source, n) }
	// The grabbed still is the source's frame n, the one a player shows at
	// its time: nearer it than frame other.
	requireFrame := func(t *testing.T, poster media.FileInfo, n, other int) {
		t.Helper()
		res, err := http.Get(poster.URL)
		must(t, err)
		defer res.Body.Close()
		still, err := png.Decode(res.Body)
		must(t, err)
		if b := still.Bounds(); b.Dx() != 256 || b.Dy() != 144 || poster.W != 256 || poster.H != 144 {
			t.Fatalf("grabbed still %v recorded %dx%d, want the video's 256x144", b, poster.W, poster.H)
		}
		near, far := frameDistance(still, frame(n)), frameDistance(still, frame(other))
		if near > 4 || near*2 > far {
			t.Fatalf("grabbed still is not frame %d: distance %.1f to it, %.1f to frame %d", n, near, far, other)
		}
	}
	commit := func(client *ck.Client, ref media.RefBody, ops ...media.Op) error {
		_, err := client.PostMediaUploadCommit(t.Context(), media.CommitBody{Ref: ref, OperationID: uuid.NewString(), Ops: ops})
		return err
	}
	seconds := func(v float64) *float64 { return &v }
	newClip := func(t *testing.T, client *ck.Client, title string) media.RefBody {
		item, err := client.PostContents(t.Context(), catalog.ContentInput{Kind: "clip", Versions: []catalog.VersionInput{{Languages: []string{"en"}, Text: map[string]catalog.VersionText{"en": {Title: title}}}}})
		must(t, err)
		return media.RefBody{Kind: "clip", ID: item.ID, Version: item.Versions[0].ID}
	}
	from := &media.CopyFrom{ID: f.Ref.ID, Version: f.Ref.Version, Path: "source.mp4"}
	var grabbedOf string

	t.Run("own_video", func(t *testing.T) {
		// 1.625 s falls inside frame 19 (1.583-1.667 s): that frame, not the next.
		must(t, commit(admin, f.Ref, media.Op{Op: media.OpFrame, Path: "poster", T: seconds(1.625)}))
		poster := awaitPoster(t, admin, f.Ref)
		if poster.Frame == nil || poster.Frame.T != 1.625 || poster.Frame.Of == "" || poster.Frame.From != nil {
			t.Fatalf("own frame provenance: %+v", poster.Frame)
		}
		grabbedOf = poster.Frame.Of
		requireFrame(t, poster, 19, 20)
	})

	t.Run("another_items_video", func(t *testing.T) {
		dest := newClip(t, admin, "Poster from another clip")
		crop := &media.Edit{Crop: &media.Crop{X: 64, Y: 0, W: 128, H: 144}}
		must(t, commit(admin, dest, media.Op{Op: media.OpFrame, Path: "poster", T: seconds(0.5), Edit: crop, From: from}))
		poster := awaitPoster(t, admin, dest)
		want := media.CopyFrom{Kind: "clip", ID: f.Ref.ID, Version: f.Ref.Version, Path: "source.mp4"}
		if poster.Frame == nil || poster.Frame.From == nil || *poster.Frame.From != want || poster.Frame.T != 0.5 || poster.Frame.Of != grabbedOf {
			t.Fatalf("cross-item frame provenance: %+v", poster.Frame)
		}
		if poster.Edit == nil || poster.Edit.Crop == nil || *poster.Edit.Crop != *crop.Crop {
			t.Fatalf("crop in the source's pixels was not kept: %+v", poster.Edit)
		}
		requireFrame(t, poster, 6, 18)
	})

	t.Run("another_kinds_item", func(t *testing.T) {
		id, err := uuid.NewV7()
		must(t, err)
		site := media.RefBody{Kind: "siteclip", ID: id.String()}
		// The site kind's poster also declares its own video, which this item lacks.
		must(t, commit(admin, site, media.Op{Op: media.OpFrame, Path: "poster", T: seconds(1.5), From: &media.CopyFrom{Kind: "clip", ID: from.ID, Version: from.Version, Path: "source"}}))
		poster := awaitPoster(t, admin, site)
		if poster.Frame == nil || poster.Frame.From == nil || poster.Frame.From.Kind != "clip" || poster.Frame.From.Path != "source.mp4" {
			t.Fatalf("cross-kind frame provenance: %+v", poster.Frame)
		}
		requireFrame(t, poster, 18, 3)
	})

	t.Run("source_is_authorized_and_checked", func(t *testing.T) {
		dest := newClip(t, admin, "Refused posters")
		for name, tc := range map[string]struct {
			op     media.Op
			status int
			code   string
		}{
			"not_a_video":       {media.Op{Op: media.OpFrame, Path: "poster", T: seconds(0.5), From: &media.CopyFrom{ID: from.ID, Version: from.Version, Path: "poster.png"}}, 404, "not_found"},
			"missing_upload":    {media.Op{Op: media.OpFrame, Path: "poster", T: seconds(0.5), From: &media.CopyFrom{ID: from.ID, Version: from.Version, Path: "absent.mp4"}}, 404, "not_found"},
			"past_the_end":      {media.Op{Op: media.OpFrame, Path: "poster", T: seconds(99), From: from}, 400, "invalid_request"},
			"unknown_kind":      {media.Op{Op: media.OpFrame, Path: "poster", T: seconds(0.5), From: &media.CopyFrom{Kind: "nope", ID: from.ID, Version: from.Version, Path: "source.mp4"}}, 400, "invalid_request"},
			"not_an_image_path": {media.Op{Op: media.OpFrame, Path: "source", T: seconds(0.5), From: from}, 400, "invalid_request"},
			"no_frames_upload":  {media.Op{Op: media.OpFrame, Path: "art", T: seconds(0.5), From: from}, 400, "invalid_request"},
			"kind_is_for_frame": {media.Op{Op: media.OpCopy, From: &media.CopyFrom{Kind: "clip", ID: from.ID, Version: from.Version, Path: "source.mp4"}, To: "source"}, 400, "invalid_request"},
		} {
			t.Run(name, func(t *testing.T) {
				requireError(t, commit(admin, dest, tc.op), tc.status, tc.code)
			})
		}
		// A contributor edits only its own clips: it may not grab another's video.
		person, token := f.world.person(t, 0)
		authtest.GrantRole(t, f.world.tenant[0], f.app.group, iam.UserSubject(person.ID), f.world.contributor)
		contributor := f.app.client(token)
		own := newClip(t, contributor, "Contributor clip")
		requireError(t, commit(contributor, own, media.Op{Op: media.OpFrame, Path: "poster", T: seconds(0.5), From: from}), 403, "forbidden")
		// Its own item passes authorization and is checked for a video.
		requireError(t, commit(contributor, own, media.Op{Op: media.OpFrame, Path: "poster", T: seconds(0.5), From: &media.CopyFrom{ID: own.ID, Version: own.Version, Path: "source"}}), 404, "not_found")
		for _, ref := range []media.RefBody{dest, own} {
			if read := editorRead(t, admin, ref); len(read.Files) != 0 {
				t.Fatalf("refused frame ops wrote %s: %+v", ref.ID, read.Files)
			}
		}
	})

	t.Run("CK-J12-S06_frames_count_toward_max", func(t *testing.T) {
		dest := newClip(t, admin, "Bounded stills")
		still := func(name string, at float64) media.Op {
			return media.Op{Op: media.OpFrame, Path: "stills/" + name, T: seconds(at), From: from}
		}
		requireError(t, commit(admin, dest, still("a", 0.5), still("b", 1)), 409, "too_many_files")
		must(t, commit(admin, dest, still("a", 0.5)))
		requireError(t, commit(admin, dest, still("b", 1)), 409, "too_many_files")
		must(t, commit(admin, dest, still("a", 1))) // replacing the one still is no new file
		var stills []string
		for _, file := range editorRead(t, admin, dest).Files {
			if file.Upload && strings.HasPrefix(file.Path, "stills/") {
				stills = append(stills, file.Path)
			}
		}
		if len(stills) != 1 || stills[0] != "stills/a.png" {
			t.Fatalf("stills past Max: %v", stills)
		}
	})

	t.Run("CK-J12-S06_a_frame_is_charged_its_still_and_refunded_once", func(t *testing.T) {
		person, token := f.world.person(t, 0)
		authtest.GrantRole(t, f.world.tenant[0], f.app.group, iam.UserSubject(person.ID), f.world.contributor)
		owner := f.app.client(token)
		own := newClip(t, owner, "Charged poster")
		_, err := owner.IngestMedia(t.Context(), http.DefaultClient, own, uuid.NewString(), []ck.MediaFile{{Path: "source.mp4", Type: "video/mp4", Body: bytes.NewReader(f.Source)}})
		must(t, err)
		awaitMediaReady(t, owner, own)
		target, err := f.config.Kinds.VersionRef(own.Kind, own.ID, own.Version)
		must(t, err)
		usage := func() int64 {
			var used int64
			must(t, f.pool.QueryRow(t.Context(), `SELECT used_bytes FROM `+pgx.Identifier{f.schema, "content_media_usage"}.Sanitize()+` WHERE tenant_id=$1 AND owner_id=$2`, target.TenantID, string(person.ID)).Scan(&used))
			return used
		}
		before := usage()
		must(t, commit(owner, own, media.Op{Op: media.OpFrame, Path: "poster", T: seconds(1)}))
		poster := awaitPoster(t, owner, own)
		charged := max(poster.Size, 64<<10) // each upload is charged at least 64 KiB
		if got := usage(); got != before+charged {
			t.Fatalf("a %d-byte still moved usage from %d to %d, want %d", poster.Size, before, got, before+charged)
		}
		must(t, commit(owner, own, media.Op{Op: media.OpRemove, Path: poster.Path}))
		if got := usage(); got != before {
			t.Fatalf("removing the still left usage at %d, want %d", got, before)
		}
	})
}

// sourceFrame decodes frame n of a tiny fixture's 12 fps source.
func sourceFrame(t *testing.T, source []byte, n int) image.Image {
	t.Helper()
	file := filepath.Join(t.TempDir(), "source.mp4")
	must(t, os.WriteFile(file, source, 0o600))
	out, err := exec.CommandContext(t.Context(), "ffmpeg", "-nostdin", "-v", "error", "-ss", strconv.FormatFloat(float64(n)/12-0.0005, 'f', 6, 64), "-i", file,
		"-frames:v", "1", "-f", "image2pipe", "-c:v", "png", "pipe:1").Output()
	must(t, err)
	frame, err := png.Decode(bytes.NewReader(out))
	must(t, err)
	return frame
}

func editorRead(t *testing.T, client *ck.Client, ref media.RefBody) media.ReadResult {
	t.Helper()
	var read media.ReadResult
	var err error
	if ref.Version != "" {
		read, err = client.GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{Editor: true})
	} else {
		read, err = client.GetMedia(t.Context(), ref.Kind, ref.ID, ck.GetMediaQuery{Editor: true})
	}
	must(t, err)
	return read
}

// awaitPoster waits until the worker grabbed poster.png and rendered it.
func awaitPoster(t *testing.T, client *ck.Client, ref media.RefBody) media.FileInfo {
	t.Helper()
	var poster media.FileInfo
	waitForWithin(t, 90*time.Second, func() bool {
		time.Sleep(50 * time.Millisecond)
		read := editorRead(t, client, ref)
		if read.Status != nil && read.Status.FailureCount != 0 {
			t.Fatalf("poster processing failed: %+v", read.Status.Failures)
		}
		rendered := false
		for _, file := range read.Files {
			if file.Upload && file.Path == "poster.png" {
				poster = file
			}
			rendered = rendered || file.Path == "posters/poster.webp" && file.From == "poster.png"
		}
		return rendered && poster.Size > 0 && len(poster.Pending) == 0 && poster.URL != ""
	})
	return poster
}

// frameDistance is the mean absolute RGB difference of two same-size frames.
func frameDistance(a, b image.Image) float64 {
	bounds := a.Bounds()
	var sum, n float64
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r1, g1, b1, _ := a.At(x, y).RGBA()
			r2, g2, b2, _ := b.At(x, y).RGBA()
			for _, d := range [][2]uint32{{r1, r2}, {g1, g2}, {b1, b2}} {
				sum += float64(max(d[0], d[1])-min(d[0], d[1])) / 257
				n++
			}
		}
	}
	return sum / n
}
