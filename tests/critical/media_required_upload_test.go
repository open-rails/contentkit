package critical_test

import (
	"bytes"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	mediastore "github.com/open-rails/contentkit/internal/mediastore"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/media"
)

// Readiness is a pure function of the registry and manifest. A required
// upload counts only once attached (and, for pages, included); one that is
// present but processing or failed reports that state instead.
func TestRequiredUploadReadiness(t *testing.T) {
	registry, err := mediastore.NewRegistry(media.Config{Namespace: "required", Kinds: []media.Kind{
		{Name: "clip", Uploads: []media.Upload{
			{Path: "source", Types: []string{"video/mp4"}, MaxBytes: 1 << 20, Required: true},
			{Path: "poster", Types: []string{"image/png"}, MaxBytes: 1 << 20},
		}, Private: []media.Private{{Name: "stream", From: "source", To: "streams/", HLS: &media.HLS{Ladder: []int{144}}}}},
		{Name: "gallery", Uploads: []media.Upload{{Path: "pages/{name}", Types: []string{"image/png"}, MaxBytes: 1 << 20, Required: true}},
			Private: []media.Private{{Name: "reader", From: "pages/{name}", To: "reader/{name}.webp", Image: &media.Image{Width: 48, Height: 48}}}},
		{Name: "optional", Uploads: []media.Upload{{Path: "source", Types: []string{"video/mp4"}, MaxBytes: 1 << 20}}},
	}}, mediastore.Hooks{})
	must(t, err)
	failed := media.File{Path: "source.mp4", Type: "video/mp4", Blob: "sha256-failed"}
	failed.Failed = &media.Failure{Of: failed.Key(), Message: "unreadable"}
	poster := media.File{Path: "poster.png", Type: "image/png", Blob: "sha256-poster"}
	page := func(name string, included bool) media.File {
		return media.File{Path: "pages/" + name + ".png", Type: "image/png", Blob: "sha256-" + name, Meta: map[string]any{"page": map[string]any{"included": included}}}
	}
	for _, c := range []struct {
		name, kind, state string
		files             []media.File
		missing           []string
	}{
		{"no uploads", "clip", media.StateMissing, nil, []string{"source"}},
		{"only an optional upload", "clip", media.StateMissing, []media.File{poster}, []string{"source"}},
		{"source staged", "clip", media.StateProcessing, []media.File{{Path: "source.mp4", Type: "video/mp4", Staged: "u-1"}}, nil},
		{"source encoding", "clip", media.StateProcessing, []media.File{{Path: "source.mp4", Type: "video/mp4", Blob: "sha256-source", Pending: []string{"stream"}}}, nil},
		{"source encoded", "clip", media.StateReady, []media.File{poster, {Path: "source.mp4", Type: "video/mp4", Blob: "sha256-source"}}, nil},
		{"source not attached", "clip", media.StateMissing, []media.File{{Path: "source.mp4", Type: "video/mp4", Blob: "sha256-source", Unattached: true}}, []string{"source"}},
		{"source failed", "clip", media.StateFailed, []media.File{failed}, nil},
		{"one included page", "gallery", media.StateReady, []media.File{page("01", false), page("02", true)}, nil},
		{"every page excluded", "gallery", media.StateMissing, []media.File{page("01", false)}, []string{"pages/{name}"}},
		{"nothing required", "optional", media.StateReady, nil, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			k, err := registry.Kind(c.kind)
			must(t, err)
			got := k.Readiness(&mediastore.Manifest{V: media.ManifestVersion, Files: c.files})
			if got.State != c.state || !slices.Equal(got.Missing, c.missing) || got.Ready() != (c.state == media.StateReady) {
				t.Fatalf("readiness = %+v, want state %q missing %v", got, c.state, c.missing)
			}
		})
	}
}

// A version never releases without its required upload. Releases made before
// the upload was required (a clip's source, a gallery's pages) are withdrawn by
// Reconcile, those holding it stay, and a withdrawn version returns once its
// source is uploaded and encoded.
func TestRequiredUploadHoldsRelease(t *testing.T) {
	m := newResilientMedia(t, func(c *ck.Config) {
		c.Media.Kinds[0].Publish = &media.Publish{Pages: "pages/{name}", Renditions: []string{"reader"}}
		c.Content.Kinds["clip"] = kinds.Kind{Route: "clips", Presentation: "player", Versions: kinds.VersionRules{Languages: "one", Hidden: true}}
		c.Media.Config.Kinds = append(c.Media.Config.Kinds, media.Kind{Name: "clip", KeepOriginals: true,
			Uploads: []media.Upload{{Path: "source", Types: []string{"video/mp4"}, MaxBytes: 10 << 20}, {Path: "poster", Types: []string{"image/png"}, MaxBytes: 1 << 20}},
			Private: []media.Private{{Name: "stream", From: "source", To: "streams/", HLS: &media.HLS{Ladder: []int{144}}}},
			Public:  []media.Public{{Name: "poster", From: "poster", To: "poster-{w}.webp", Widths: []int{16}, Image: media.Image{Aspect: media.Square}}},
		})
	})
	encoded := filepath.Join(t.TempDir(), "source.mp4")
	command := exec.CommandContext(t.Context(), "ffmpeg", "-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=256x144:rate=12", "-t", "1", "-an", "-c:v", "libx264", "-threads", "1", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-movflags", "+faststart", encoded)
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("create real video source: %v %s", err, out)
	}
	video, err := os.ReadFile(encoded)
	must(t, err)
	source := func() ck.MediaFile { return ck.MediaFile{Path: "source.mp4", Type: "video/mp4", Body: bytes.NewReader(video)} }
	poster := func() ck.MediaFile {
		return ck.MediaFile{Path: "poster.png", Type: "image/png", Body: bytes.NewReader(imageBytes(t))}
	}
	admin := m.app.engine.Client()
	publish := func(ref media.RefBody) {
		_, err := admin.PostContentPublish(t.Context(), ref.ID, catalog.PublishInput{})
		must(t, err)
	}
	draft := func(title string, files ...ck.MediaFile) media.RefBody {
		item, err := admin.PostContents(t.Context(), catalog.ContentInput{Kind: "clip", Versions: []catalog.VersionInput{{Languages: []string{"en"}, Text: map[string]catalog.VersionText{"en": {Title: title}}}}})
		must(t, err)
		ref := media.RefBody{Kind: "clip", ID: item.ID, Version: item.Versions[0].ID}
		_, err = admin.IngestMedia(t.Context(), http.DefaultClient, ref, uuid.NewString(), files)
		must(t, err)
		return ref
	}
	clip := func(title string, files ...ck.MediaFile) media.RefBody {
		ref := draft(title, files...)
		publish(ref)
		return ref
	}
	gallery := func(title string, files ...ck.MediaFile) media.RefBody {
		ref := galleryRef(t, admin, title)
		_, err := admin.IngestMedia(t.Context(), http.DefaultClient, ref, uuid.NewString(), files)
		must(t, err)
		publish(ref)
		return ref
	}
	cover := func() ck.MediaFile { return ck.MediaFile{Path: "cover.png", Type: "image/png", Body: bytes.NewReader(imageBytes(t))} }
	version := func(ref media.RefBody) catalog.Version {
		v, err := admin.GetContentVersion(t.Context(), ref.ID, ref.Version, ck.GetContentVersionQuery{})
		must(t, err)
		return v
	}
	publication := func(ref media.RefBody) string {
		if p := version(ref).Publication; p != nil {
			return p.State
		}
		return ""
	}
	itemState := func(ref media.RefBody) string {
		read, err := admin.GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{Editor: true})
		must(t, err)
		if read.Status == nil || read.Status.State != read.State {
			t.Fatalf("editor read state %q disagrees with its status %+v", read.State, read.Status)
		}
		return read.State
	}
	listed := func(kind string) []string {
		page, err := m.app.client("").GetContents(t.Context(), ck.GetContentsQuery{Kind: kind})
		must(t, err)
		var ids []string
		for _, c := range page.Contents {
			ids = append(ids, c.ID)
		}
		slices.Sort(ids)
		return ids
	}
	ids := func(refs ...media.RefBody) []string {
		var out []string
		for _, ref := range refs {
			out = append(out, ref.ID)
		}
		slices.Sort(out)
		return out
	}
	stopWorker := startVideoFailureWorker(t, m)

	// Before the source is required, a version without one releases: the
	// data an import leaves behind.
	kept := clip("Encoded before the rule", source(), poster())
	legacy := clip("Released without a source", poster())
	paged := gallery("Pages before the rule", ck.MediaFile{Path: "pages/01.png", Type: "image/png", Body: bytes.NewReader(imageBytes(t))}, cover())
	pageless := gallery("Released without pages", cover())
	for _, ref := range []media.RefBody{kept, legacy, paged, pageless} {
		awaitEditionPublication(t, m.app, admin, ref.ID, ref.Version)
	}
	waitForWithin(t, 20*time.Second, func() bool {
		return slices.Equal(listed("clip"), ids(kept, legacy)) && slices.Equal(listed("gallery"), ids(paged, pageless))
	})
	// A release that fails while copying keeps its frozen plan for its retry.
	stale := draft("Release interrupted before the rule", poster())
	awaitMediaReady(t, admin, stale)
	m.wire.mu.Lock()
	m.wire.refuseOutput = func(r *http.Request) bool {
		return r.Method == http.MethodPut && r.Header.Get("X-Amz-Copy-Source") != "" && strings.Contains(r.URL.Path, stale.Version)
	}
	m.wire.mu.Unlock()
	publish(stale)
	requests := pgx.Identifier{m.schema, "content_publication_requests"}.Sanitize()
	// Processing settling after publication wakes a newer request; the frozen
	// one must be the last left before the rule changes.
	frozenID := ""
	waitForWithin(t, 60*time.Second, func() bool {
		var id, state string
		var frozen bool
		var processing int
		err := m.pool.QueryRow(t.Context(), `SELECT id::text,state,plan IS NOT NULL FROM `+requests+` WHERE tenant_id=$1 AND version_id=$2 AND current`, publicationTenant(m.app), stale.Version).Scan(&id, &state, &frozen)
		must(t, m.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+pgx.Identifier{m.config.Schema, "river_job"}.Sanitize()+` WHERE state IN ('available','running','retryable','scheduled')`).Scan(&processing))
		if err != nil || state != "failed" || !frozen || processing != 0 {
			frozenID = ""
			return false
		}
		if id == frozenID {
			return true
		}
		frozenID = id
		time.Sleep(time.Second)
		return false
	})

	stopWorker()
	m.app.server.Close()
	must(t, m.app.engine.Close(t.Context()))
	m.wire.mu.Lock()
	m.wire.refuseOutput = nil
	m.wire.mu.Unlock()
	for i := range m.app.cfg.Media.Kinds {
		if name := m.app.cfg.Media.Kinds[i].Name; name == "clip" || name == "gallery" {
			m.app.cfg.Media.Kinds[i].Uploads[0].Required = true
		}
	}
	m.app.engine, err = ck.New(t.Context(), m.app.cfg, m.app.deps)
	must(t, err)
	m.app.server = mountEngine(t, m.app.engine)
	config, err := m.app.cfg.MediaKinds()
	must(t, err)
	m.config.Kinds, err = mediastore.NewRegistry(config, mediastore.Hooks{})
	must(t, err)
	admin = m.app.engine.Client()
	reconciled, err := m.app.engine.Reconcile(t.Context())
	must(t, err)
	if reconciled.Releases != 2 {
		t.Fatalf("Reconcile queued %d releases, want the clip without a source and the gallery without pages", reconciled.Releases)
	}
	must(t, m.app.engine.StartJobs(t.Context()))
	startVideoFailureWorker(t, m)

	waitForWithin(t, 45*time.Second, func() bool {
		wakeDuePublicationJobs(t, m.app)
		return !version(legacy).Live && publication(legacy) == "waiting" && !version(pageless).Live && publication(pageless) == "waiting"
	})
	for deadline := time.Now().Add(45 * time.Second); publication(stale) != "waiting"; time.Sleep(50 * time.Millisecond) {
		if version(stale).Live {
			t.Fatal("a release planned before its source was required went live on retry")
		}
		if time.Now().After(deadline) {
			t.Fatalf("the interrupted release never waited for its source: %q", publication(stale))
		}
		wakeDuePublicationJobs(t, m.app)
	}
	if version(stale).Live {
		t.Fatal("a release planned before its source was required went live on retry")
	}
	waitForWithin(t, 20*time.Second, func() bool {
		return slices.Equal(listed("clip"), ids(kept)) && slices.Equal(listed("gallery"), ids(paged))
	})
	_, err = m.app.client("").GetVersion(t.Context(), legacy.Version, ck.GetVersionQuery{})
	requireError(t, err, 404, "not_found")
	if !version(kept).Live || !version(paged).Live {
		t.Fatal("Reconcile withdrew a release that holds its required upload")
	}
	for _, ref := range []media.RefBody{legacy, pageless, stale} {
		if state := itemState(ref); state != media.StateMissing {
			t.Fatalf("editors read a %s without its required upload as %q, want missing", ref.Kind, state)
		}
	}

	// A version published without its source never activates a release.
	fresh := clip("Published without a source", poster())
	waitForWithin(t, 45*time.Second, func() bool {
		wakeDuePublicationJobs(t, m.app)
		return publication(fresh) == "waiting" && itemState(fresh) == media.StateMissing
	})
	var released, activated bool
	must(t, m.pool.QueryRow(t.Context(), `SELECT released,release_id IS NOT NULL FROM `+pgx.Identifier{m.schema, "content_versions"}.Sanitize()+` WHERE tenant_id=$1 AND id=$2`,
		publicationTenant(m.app), fresh.Version).Scan(&released, &activated))
	if v := version(fresh); released || activated || v.Live || v.PublishedAt != nil {
		t.Fatalf("a version without its source was released: released=%v activated=%v live=%v published_at=%v", released, activated, v.Live, v.PublishedAt)
	}

	// The withdrawn version's source arrives: it stays unreleased while its
	// stream encodes, then releases.
	hold := m.wire.pause(t, func(r *http.Request) bool {
		return r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/internal/") && strings.HasPrefix(r.Header.Get("Content-Type"), "video/mp4")
	})
	_, err = admin.IngestMedia(t.Context(), http.DefaultClient, legacy, uuid.NewString(), []ck.MediaFile{source()})
	must(t, err)
	select {
	case <-hold.arrived:
	case <-time.After(60 * time.Second):
		t.Fatal("the encode never reached its held output")
	}
	wakeDuePublicationJobs(t, m.app)
	if state := itemState(legacy); state != media.StateProcessing || version(legacy).Live {
		t.Fatalf("a version released while its source encoded: state=%q", state)
	}
	hold.unblock()
	awaitMediaReady(t, admin, legacy)
	awaitEditionPublication(t, m.app, admin, legacy.ID, legacy.Version)
	waitForWithin(t, 20*time.Second, func() bool { return slices.Equal(listed("clip"), ids(kept, legacy)) })
	reconciled, err = m.app.engine.Reconcile(t.Context())
	must(t, err)
	if reconciled.Releases != 0 {
		t.Fatalf("Reconcile queued %d releases after every live release held its source", reconciled.Releases)
	}
}
