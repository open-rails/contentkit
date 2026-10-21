package versions

import (
	"strings"
	"testing"

	"github.com/open-rails/contentkit/media"
)

// A player version plays once a rendition the public read lists is out: a
// source alone, or a rendition of an unattached source, does not play, and a
// kind without a player has no say.
func TestFactsPlayableOnlyOnceARenditionIsOut(t *testing.T) {
	k := &media.Kind{Name: "video"}
	source := media.File{Path: "source.mp4", Blob: "sha256-" + strings.Repeat("a", 64) + "-0192aaaa-0000-7000-8000-000000000001", Type: "video/mp4", Dur: 12.5, W: 1280, H: 720}
	rung := media.File{Path: "hls/480/index.m3u8", Blob: "sha256-" + strings.Repeat("b", 64) + "-0192aaaa-0000-7000-8000-000000000002", From: "source.mp4",
		Preset: "hls", Track: &media.Track{Kind: media.TrackVideo}}
	unattached := source
	unattached.Unattached = true
	for _, tc := range []struct {
		name   string
		files  []media.File
		player bool
		want   *bool
	}{
		{name: "source only", files: []media.File{source}, player: true, want: new(false)},
		{name: "first rendition out", files: []media.File{source, rung}, player: true, want: new(true)},
		{name: "source not attached", files: []media.File{unattached, rung}, player: true, want: new(false)},
		{name: "not a player", files: []media.File{source, rung}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := factsOf(k, &media.Manifest{Files: tc.files}, tc.player).Playable
			if (got == nil) != (tc.want == nil) || got != nil && *got != *tc.want {
				t.Fatalf("playable = %v, want %v", deref(got), deref(tc.want))
			}
		})
	}
}

func deref(b *bool) any {
	if b == nil {
		return nil
	}
	return *b
}
