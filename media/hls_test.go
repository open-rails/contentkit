package media

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestMasterPlaylistStartsAt1080(t *testing.T) {
	land := func(rung int) Rendition {
		return Rendition{Rung: rung, Width: rung * 16 / 9, Height: rung, Bandwidth: rung * 5000, Codec: CodecH264, Codecs: "avc1.640028"}
	}
	port := func(rung int) Rendition { r := land(rung); r.Width, r.Height = r.Height, r.Width; return r }
	uris := regexp.MustCompile(`(?m)^video/(\d+)-h264\.m3u8$`)
	for _, tc := range []struct {
		name  string
		video []Rendition
		want  []string
	}{
		{"default ladder", []Rendition{land(2160), land(1440), land(1080), land(720), land(480)}, []string{"1080", "2160", "1440", "720", "480"}},
		{"portrait", []Rendition{port(2160), port(1440), port(1080), port(720), port(480)}, []string{"1080", "2160", "1440", "720", "480"}},
		{"ascending input", []Rendition{land(480), land(720), land(1080), land(1440)}, []string{"1080", "1440", "720", "480"}},
		{"no 1080 rung", []Rendition{land(1440), land(900), land(480)}, []string{"900", "1440", "480"}},
		{"small source", []Rendition{land(720), land(480)}, []string{"720", "480"}},
		{"all above 1080", []Rendition{land(2160), land(1440)}, []string{"1440", "2160"}},
		{"one rung", []Rendition{land(480)}, []string{"480"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &Grant{units: 1, Manifest: &Manifest{Files: []File{{Name: "v", HLS: &HLS{Video: tc.video}}}}}
			before := slices.Clone(tc.video)
			pl, err := g.MasterPlaylist("v", MasterOptions{})
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, m := range uris.FindAllStringSubmatch(string(pl), -1) {
				got = append(got, m[1])
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("variants %v, want %v\n%s", got, tc.want, pl)
			}
			if !slices.EqualFunc(before, tc.video, func(a, b Rendition) bool { return a.Rung == b.Rung }) {
				t.Fatal("reordered the manifest's renditions")
			}
		})
	}
}

// Each codec's variants follow the ladder's codec order, each group starting
// at its 1080 rung, with the codec's CODECS and its own media playlist URI.
func TestMasterPlaylistCodecs(t *testing.T) {
	r := func(rung int, c Codec, codecs string) Rendition {
		return Rendition{Rung: rung, Codec: c, Width: rung * 16 / 9, Height: rung, Bandwidth: rung * 5000, Codecs: codecs}
	}
	video := []Rendition{r(2160, CodecAV1, "av01.0.12M.08"), r(1080, CodecAV1, "av01.0.08M.08"), r(480, CodecAV1, "av01.0.04M.08"),
		r(2160, CodecH264, "avc1.640033"), r(1080, CodecH264, "avc1.640028"), r(480, CodecH264, "avc1.64001e")}
	g := &Grant{units: 1, Manifest: &Manifest{Files: []File{{Name: "v", HLS: &HLS{Video: video,
		Audio: []AudioTrack{{ID: "a1", Default: true, Codecs: "mp4a.40.2"}}}}}}}
	pl, err := g.MasterPlaylist("v", MasterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?m)CODECS="([^"]+)".*\n(video/\S+)$`)
	var got []string
	for _, m := range re.FindAllStringSubmatch(string(pl), -1) {
		got = append(got, m[1]+" "+m[2])
	}
	want := []string{
		"av01.0.08M.08,mp4a.40.2 video/1080-av1.m3u8", "av01.0.12M.08,mp4a.40.2 video/2160-av1.m3u8", "av01.0.04M.08,mp4a.40.2 video/480-av1.m3u8",
		"avc1.640028,mp4a.40.2 video/1080-h264.m3u8", "avc1.640033,mp4a.40.2 video/2160-h264.m3u8", "avc1.64001e,mp4a.40.2 video/480-h264.m3u8",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("variants\n%s\nwant\n%s\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"), pl)
	}
}

func TestAudioKind(t *testing.T) {
	types := []string{"audio/mpeg"}
	for _, bad := range []Kind{
		{Name: "a", Types: types},
		{Name: "a", Types: types, Audio: &Audio{Loudness: -80}},
		{Name: "a", Types: types, Audio: &Audio{}, Specs: map[string]Spec{AudioVariant: {Width: 10}}},
	} {
		if _, err := NewRegistry(bad); err == nil {
			t.Fatalf("registered %+v", bad)
		}
	}
	if _, err := NewRegistry(Kind{Name: "a", Types: types, Audio: &Audio{Loudness: -16}}); err != nil {
		t.Fatal(err)
	}

	src := "sha256-" + strings.Repeat("a", 64)
	track := []AudioTrack{{ID: "a1", Blob: "sha256-" + strings.Repeat("b", 64), Segments: []Segment{{Offset: 10, Length: 5, Seconds: 1}}}}
	for _, tc := range []struct {
		hls      *HLS
		state    string
		servable bool
	}{
		{nil, StateProcessing, false},
		{&HLS{Source: src, Audio: track}, StateReady, true},
		{&HLS{Source: src, Error: "no audio"}, StateFailed, false},
		{&HLS{Source: "sha256-" + strings.Repeat("c", 64), Audio: track}, StateProcessing, true}, // replaced: the old track plays
	} {
		f := File{Name: "x.mp3", Original: src, Type: "audio/mpeg", HLS: tc.hls}
		if f.State() != tc.state || f.Servable() != tc.servable || tc.hls.playable() != (tc.servable) {
			t.Fatalf("%+v: state %s servable %v", tc.hls, f.State(), f.Servable())
		}
	}
	if downloadFile(AudioDownloadKey("x.mp3")) != "x.mp3" || downloadFile("x-1080p") != "x" {
		t.Fatal("download keys")
	}
}
