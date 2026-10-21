package versions_test

import (
	"strings"
	"testing"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/versions"
)

// The daily pass re-reads movie versions not known to play: one whose
// rendition is out becomes playable though no hook reported it, and one with
// no media at all is recorded as not playing.
func TestRecordPlayableRereadsVersionsNotKnownToPlay(t *testing.T) {
	s := newSite(t, siteOptions{})
	c := s.content("movie", `{"level":"public"}`)
	playing, empty := s.version(c), s.version(c)
	ref := s.ref("movie", playing)
	s.mustUpload(s.creator, ref, "subs/en", "text/vtt", []byte("WEBVTT\n\n00:00.000 --> 00:01.000\nhi\n"))
	s.settles()
	if _, err := s.ms.EditExisting(s.ctx, ref, func(m *media.Manifest) error {
		for _, f := range m.Files {
			if strings.HasPrefix(f.Path, "subs/") {
				m.Files = append(m.Files, media.File{Path: "hls/480/index.m3u8", Blob: f.Blob, Type: "application/vnd.apple.mpegurl", Size: f.Size,
					From: "source", Preset: "hls", Track: &media.Track{Kind: media.TrackVideo}})
				return nil
			}
		}
		t.Fatal("no subtitle upload to borrow a blob from")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s.exec(`UPDATE `+s.table("content_versions")+` SET playable = false WHERE id = $1`, playing.Version)
	if err := versions.RecordPlayable(s.svc, s.ctx); err != nil {
		t.Fatal(err)
	}
	playable := func(v versions.Target) *bool {
		t.Helper()
		var got *bool
		if err := s.pool.QueryRow(s.ctx, `SELECT playable FROM `+s.table("content_versions")+` WHERE id = $1`, v.Version).Scan(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := playable(playing); got == nil || !*got {
		t.Fatalf("a version whose rendition is out: playable = %v, want true", got)
	}
	if got := playable(empty); got == nil || *got {
		t.Fatalf("a version with no media: playable = %v, want false", got)
	}
	if !s.cat.wasRefreshed(playing.Version) || !s.cat.wasRefreshed(empty.Version) {
		t.Fatal("recorded facts did not refresh the listing")
	}
}
