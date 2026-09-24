package media

import (
	"regexp"
	"slices"
	"testing"
)

func TestMasterPlaylistStartsAt1080(t *testing.T) {
	land := func(rung int) Rendition {
		return Rendition{Rung: rung, Width: rung * 16 / 9, Height: rung, Bandwidth: rung * 5000, Codecs: "avc1.640028"}
	}
	port := func(rung int) Rendition { r := land(rung); r.Width, r.Height = r.Height, r.Width; return r }
	uris := regexp.MustCompile(`(?m)^video/(\d+)\.m3u8$`)
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
