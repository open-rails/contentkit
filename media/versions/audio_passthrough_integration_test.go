package versions_test

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
	"github.com/open-rails/contentkit/media/versions"
)

// Audio attachments are sealed and played unchanged. They neither request
// conversion nor expose page originals, even through a restricted reader grant.
func TestOriginalAudioPlaybackAndLegacyFallback(t *testing.T) {
	s := newSite(t, siteOptions{})
	v := s.version(s.content("gallery", `{"level":"tier","tiers":["premium"]}`))
	ref := s.ref("gallery", v)
	s.mustUpload(s.creator, ref, "originals/001", "image/png", pngOf(t, 30, 40, 7))
	s.plan(s.fan, v, func(p *versions.ReaderPlan) bool { return p.Pages != nil && *p.Pages == 1 })
	jobs := pgx.Identifier{s.workers, "river_job"}.Sanitize()
	var before int
	idle := func() bool {
		var active int
		if err := s.pool.QueryRow(s.ctx, `SELECT count(*) FILTER(WHERE kind='contentkit_media_image'), count(*) FILTER(WHERE finalized_at IS NULL) FROM `+jobs).Scan(&before, &active); err != nil {
			t.Fatal(err)
		}
		return active == 0
	}
	eventually(t, "page jobs finished", time.Minute, idle)
	body := wav(t)
	s.mustUpload(s.creator, ref, "audio/theme", "audio/wav", body)
	p := s.plan(s.fan, v, func(p *versions.ReaderPlan) bool { return p.Playback != nil && len(p.Playback.Tracks) == 1 })
	tr := p.Playback.Tracks[0]
	if tr.Path != "audio/theme.wav" || tr.Duration != 0 {
		t.Fatalf("original audio track %+v", tr)
	}
	if locked := s.plan(s.stranger, v, func(*versions.ReaderPlan) bool { return true }); locked.Playback != nil {
		t.Fatalf("locked viewer received playback %+v", locked.Playback)
	}
	var images, conversions, active int
	eventually(t, "audio placement finished", time.Minute, func() bool {
		if err := s.pool.QueryRow(s.ctx, `SELECT count(*) FILTER(WHERE kind='contentkit_media_image'),count(*) FILTER(WHERE kind='contentkit_media_audio'),count(*) FILTER(WHERE finalized_at IS NULL) FROM `+jobs).Scan(&images, &conversions, &active); err != nil {
			t.Fatal(err)
		}
		return active == 0
	})
	if images != before || conversions != 0 {
		t.Fatalf("audio upload added image=%d/conversion=%d jobs", images-before, conversions)
	}
	grant, err := s.reader.Grant(s.ctx, ref, s.fan)
	if err != nil {
		t.Fatal(err)
	}
	original, ok := grant.Manifest.Get(tr.Path)
	if !ok {
		t.Fatal("missing original audio")
	}
	if grant.Full() || grant.Cookie() != nil || !grant.Allowed(original) {
		t.Fatal("audio did not use a restricted per-file grant")
	}
	if _, err := grant.URL(original, false); err != nil {
		t.Fatal(err)
	}
	page, ok := grant.Manifest.Get("originals/001.png")
	if !ok {
		t.Fatal("missing image original")
	}
	if _, err := grant.URL(page, false); !errors.Is(err, media.ErrNotAllowed) {
		t.Fatalf("audio opened image original: %v", err)
	}
	locked, err := s.reader.Grant(s.ctx, ref, s.stranger)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := locked.URL(original, false); !errors.Is(err, media.ErrNotAllowed) {
		t.Fatalf("locked viewer opened audio: %v", err)
	}
	item, _ := s.reg.Item(ref)
	key, _ := item.Blob(original.Blob)
	rc, _, err := s.Store.Get(s.ctx, key, media.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("original audio changed: %v", err)
	}

	// Install an actual old-format M4A fixture, without a conversion job. Existing
	// converted media remains readable when its original container lacks support.
	dir := t.TempDir()
	wavPath, m4aPath := filepath.Join(dir, "source.wav"), filepath.Join(dir, "legacy.m4a")
	if err := os.WriteFile(wavPath, body, 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("ffmpeg", "-v", "error", "-nostdin", "-i", wavPath, "-c:a", "aac", "-movflags", "+faststart", m4aPath).CombinedOutput(); err != nil {
		t.Fatalf("legacy fixture: %v: %s", err, out)
	}
	legacy, err := os.ReadFile(m4aPath)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(legacy)
	blobs, err := s.ms.NewBlobs(s.ctx, ref, layout.AreaPrivate, [][]byte{sum[:]})
	if err != nil {
		t.Fatal(err)
	}
	legacyKey, _ := item.Blob(blobs[0])
	if _, err := s.Store.Put(s.ctx, legacyKey, bytes.NewReader(legacy), int64(len(legacy)), media.PutOptions{ContentType: "audio/mp4"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ms.EditExisting(s.ctx, ref, func(m *media.Manifest) error {
		m.Files = append(m.Files, media.File{Path: "listen/theme/audio.m4a", From: original.Path, Preset: "listen", Blob: blobs[0], Type: "audio/mp4", Size: int64(len(legacy)), Dur: 1})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	p = s.plan(s.fan, v, func(p *versions.ReaderPlan) bool {
		return p.Playback != nil && p.Playback.Tracks[0].Path == "listen/theme/audio.m4a"
	})
	read, err := s.reader.Read(s.ctx, ref, s.fan, media.ReadOptions{Paths: []string{p.Playback.Tracks[0].Path}})
	if err != nil {
		t.Fatal(err)
	}
	var served bool
	for _, f := range read.Files {
		served = served || f.Path == p.Playback.Tracks[0].Path && f.URL != ""
	}
	if !served {
		t.Fatal("legacy audio output was not readable")
	}
	// A replacement must not keep selecting a retired converter's old audio.
	replacement := bytes.Clone(body)
	replacement[len(replacement)-1] ^= 1
	s.mustUpload(s.creator, ref, "audio/theme", "audio/wav", replacement)
	if _, retained := s.manifest(ref).Get("listen/theme/audio.m4a"); retained {
		t.Fatal("replacing audio retained its stale legacy conversion")
	}
	s.plan(s.fan, v, func(p *versions.ReaderPlan) bool {
		return p.Playback != nil && len(p.Playback.Tracks) == 1 && p.Playback.Tracks[0].Path == "audio/theme.wav"
	})
}
