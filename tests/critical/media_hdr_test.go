package critical_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/internal/media/worker"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/media"
)

// HDR uploads keep their range in 10-bit AV1 with its metadata; their
// H.264 renditions, sprite and posters are tone-mapped BT.709 SDR; the
// master playlist labels every variant's range. SDR uploads are as before.
func TestMediaHDRVideo(t *testing.T) {
	// The browser's page, API and media share an origin, as the poster
	// picker's do.
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer func() { server.CloseClientConnections(); server.Close() }()
	m := newResilientMedia(t, func(c *ck.Config) {
		c.Media.BaseURL = server.URL
		c.Content.Kinds["clip"] = kinds.Kind{Route: "clips", Presentation: "player", Versions: kinds.VersionRules{Languages: "one", Hidden: true}}
		c.Media.Delivery.TTL = 5 * time.Minute
		c.Media.Config.Kinds = append(c.Media.Config.Kinds, media.Kind{Name: "clip", KeepOriginals: true,
			Uploads: []media.Upload{{Path: "source", Types: []string{"video/mp4"}, MaxBytes: 10 << 20},
				{Path: "poster", Types: []string{"image/png"}, MaxBytes: 1 << 20, Frames: "source"}},
			Private: []media.Private{{Name: "stream", From: "source", To: "streams/", HLS: &media.HLS{Ladder: []int{144}}},
				{Name: "poster", From: "poster", To: "posters/poster.webp", Image: &media.Image{Width: 64, Height: 36}}},
		})
	})
	config := m.config
	config.VideoCodecs = []media.Codec{media.CodecAV1, media.CodecH264}
	config.VideoEncoder, config.Preset, config.TopPreset = "cpu", "ultrafast", "ultrafast"
	config.TempDir = t.TempDir()
	processor, err := worker.New(t.Context(), config)
	must(t, err)
	run, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- processor.Run(run) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("stop HDR worker: %v", err)
			}
		case <-time.After(20 * time.Second):
			t.Error("HDR worker did not stop")
		}
	})
	app, err := url.Parse(m.app.server.URL)
	must(t, err)
	gateway, err := url.Parse(m.storage.gateway.URL)
	must(t, err)
	mux.Handle("/v1/", httputil.NewSingleHostReverseProxy(gateway))
	mux.Handle("/content/", httputil.NewSingleHostReverseProxy(app))
	mux.Handle("/", http.FileServer(http.Dir(".browser-dist")))
	admin := m.app.engine.Client()
	dir := t.TempDir()
	var pq media.RefBody
	// The SDR picture every sample is made from, its first frame, its frame at
	// 0.5 s and its seek sprite as the worker tiles one (2 s: a tile each 20 ms).
	const picture = "testsrc2=s=256x144:r=12:d=2"
	reference := func(name string, vf ...string) [2]float64 {
		out := filepath.Join(dir, name)
		args := []string{"-f", "lavfi", "-i", picture}
		if len(vf) > 0 {
			args = append(args, "-vf", vf[0])
		}
		hdrFFmpeg(t, append(args, "-frames:v", "1", "-update", "1", out)...)
		return hdrLuma(t, out)
	}
	firstFrame, posterFrame := reference("first.png"), reference("poster.png", "select=eq(n\\,6)")
	spriteSheet := reference("sprite.png", "fps=50,scale=160:90:flags=lanczos,tile=10x10")

	upload := func(t *testing.T, name string, source string) (media.RefBody, map[string]media.FileInfo, map[string]map[string]string) {
		t.Helper()
		data, err := os.ReadFile(source)
		must(t, err)
		item, err := admin.PostContents(t.Context(), catalog.ContentInput{Kind: "clip", Versions: []catalog.VersionInput{{Languages: []string{"en"}, Text: map[string]catalog.VersionText{"en": {Title: name}}}}})
		must(t, err)
		ref := media.RefBody{Kind: "clip", ID: item.ID, Version: item.Versions[0].ID}
		_, err = admin.IngestMedia(t.Context(), http.DefaultClient, ref, uuid.NewString(), []ck.MediaFile{{Path: "source.mp4", Type: "video/mp4", Body: bytes.NewReader(data)}})
		must(t, err)
		waitForWithin(t, 2*time.Minute, func() bool {
			time.Sleep(50 * time.Millisecond)
			read := editorRead(t, admin, ref)
			if read.Status != nil && read.Status.FailureCount != 0 {
				t.Fatalf("%s processing failed: %+v", name, read.Status.Failures)
			}
			return read.State == media.StateReady && len(read.HLS) == 1
		})
		half := 0.5
		_, err = admin.PostMediaUploadCommit(t.Context(), media.CommitBody{Ref: ref, OperationID: uuid.NewString(), Ops: []media.Op{{Op: media.OpFrame, Path: "poster", T: &half}}})
		must(t, err)
		poster := awaitPoster(t, admin, ref)
		files := map[string]media.FileInfo{"poster": poster}
		for _, f := range editorRead(t, admin, ref).Files {
			if !f.Upload {
				files[f.Path] = f
			}
		}
		master, err := admin.GetMediaVersionHls(t.Context(), ref.Kind, ref.ID, ref.Version, "streams/master.m3u8", ck.GetMediaVersionHlsQuery{Editor: true})
		must(t, err)
		if master.Status != http.StatusOK {
			t.Fatalf("%s master: %d %s", name, master.Status, master.Body)
		}
		return ref, files, variants(t, master.Body)
	}
	fetch := func(t *testing.T, f media.FileInfo, name string) string {
		t.Helper()
		if f.URL == "" {
			t.Fatalf("no delivered %s", name)
		}
		res, err := http.Get(f.URL)
		must(t, err)
		body, err := io.ReadAll(io.LimitReader(res.Body, 10<<20))
		res.Body.Close()
		must(t, err)
		if res.StatusCode != http.StatusOK || len(body) == 0 {
			t.Fatalf("delivered %s: HTTP %d, %d bytes", name, res.StatusCode, len(body))
		}
		out := filepath.Join(t.TempDir(), name)
		must(t, os.WriteFile(out, body, 0o600))
		return out
	}
	// SDR picture: the tone-mapped one keeps the original's brightness and
	// contrast, where PQ or HLG code values shown as BT.709 lose them.
	requireSDRPicture := func(t *testing.T, what, path string, want [2]float64) {
		t.Helper()
		if got := hdrLuma(t, path); math.Abs(got[0]-want[0]) > 25 || got[1] < 0.8*want[1] {
			t.Fatalf("%s luma %.1f±%.1f, the SDR picture's %.1f±%.1f", what, got[0], got[1], want[0], want[1])
		}
	}

	hdr := func(t *testing.T, trc, label, transfer string) {
		source := filepath.Join(dir, label+".mp4")
		x265 := "log-level=error:pools=1:frame-threads=1:repeat-headers=1:colorprim=bt2020:colormatrix=bt2020nc:range=limited:transfer=" + trc
		if label == "PQ" {
			x265 += ":hdr10=1:master-display=G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,50):max-cll=1000,400"
		}
		hdrFFmpeg(t, "-f", "lavfi", "-i", picture, "-vf", "format=gbrpf32le,zscale=tin=bt709:pin=bt709:min=gbr:rin=pc:t="+trc+":p=bt2020:m=bt2020nc:r=tv:npl=1000,format=yuv420p10le",
			"-c:v", "libx265", "-preset", "ultrafast", "-x265-params", x265, "-tag:v", "hvc1", "-movflags", "+faststart", source)
		ref, files, master := upload(t, label+" clip", source)
		if label == "PQ" {
			pq = ref
		}

		av1 := fetch(t, files["streams/144-av1.mp4"], "av1.mp4")
		if s := hdrStream(t, av1); s.PixFmt != "yuv420p10le" || s.Transfer != trc || s.Primaries != "bt2020" || s.Matrix != "bt2020nc" || s.Range != "tv" {
			t.Fatalf("AV1 rendition is not 10-bit %s BT.2020: %+v", label, s)
		}
		meta := obuSideData(t, av1)
		if pq := label == "PQ"; pq != strings.Contains(meta, "max_luminance=256000/256") || pq != strings.Contains(meta, ":max_content=1000|") || pq != strings.HasSuffix(meta, ":max_average=400") ||
			pq != strings.Contains(meta, "red_x=44564/65536") || !pq && (strings.Contains(meta, "Mastering") || strings.Contains(meta, "Content light")) {
			t.Fatalf("AV1 bitstream HDR10 metadata for %s: %s", label, meta)
		}
		h264 := fetch(t, files["streams/144-h264.mp4"], "h264.mp4")
		if s := hdrStream(t, h264); s.PixFmt != "yuv420p" || s.Transfer != "bt709" || s.Primaries != "bt709" || s.Matrix != "bt709" {
			t.Fatalf("H.264 rendition is not BT.709 SDR: %+v", s)
		}
		requireSDRPicture(t, "H.264 rendition", h264, firstFrame)
		requireSDRPicture(t, "seek sprite", fetch(t, files["streams/sprite.jpg"], "sprite.jpg"), spriteSheet)
		requireSDRPicture(t, "poster", fetch(t, files["poster"], "poster.png"), posterFrame)

		if v := master["av01"]; v["VIDEO-RANGE"] != label || !regexp.MustCompile(`^av01\.0\.\d\dM\.10\.0\.11\d\.09\.`+transfer+`\.09\.0(,|$)`).MatchString(v["CODECS"]) {
			t.Fatalf("AV1 variant: %v", v)
		}
		if v := master["avc1"]; v["VIDEO-RANGE"] != "SDR" {
			t.Fatalf("H.264 variant: %v", v)
		}
	}
	t.Run("CK-J12-S07 a PQ upload keeps HDR10 in AV1 and plays tone-mapped SDR elsewhere", func(t *testing.T) {
		hdr(t, "smpte2084", "PQ", "16")
	})
	t.Run("CK-J12-S08 an HLG upload keeps HLG in AV1 and plays tone-mapped SDR elsewhere", func(t *testing.T) {
		hdr(t, "arib-std-b67", "HLG", "18")
	})
	t.Run("CK-J12-S09 an SDR upload is encoded as before and labelled SDR", func(t *testing.T) {
		source := filepath.Join(dir, "sdr.mp4")
		hdrFFmpeg(t, "-f", "lavfi", "-i", picture, "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-movflags", "+faststart", source)
		_, files, master := upload(t, "SDR clip", source)
		tags := hdrStream(t, source)
		for _, name := range []string{"streams/144-av1.mp4", "streams/144-h264.mp4"} {
			path := fetch(t, files[name], filepath.Base(name))
			if s := hdrStream(t, path); s.PixFmt != "yuv420p" || s.Transfer != tags.Transfer || s.Primaries != tags.Primaries || s.Matrix != tags.Matrix {
				t.Fatalf("%s is not the 8-bit encode with the source's own tags (%+v) an SDR upload always had: %+v", name, tags, s)
			}
			if strings.HasSuffix(name, "av1.mp4") {
				if meta := obuSideData(t, path); strings.Contains(meta, "Mastering") || strings.Contains(meta, "Content light") {
					t.Fatalf("SDR AV1 carries HDR metadata: %s", meta)
				}
			}
			requireSDRPicture(t, name, path, firstFrame)
		}
		requireSDRPicture(t, "poster", fetch(t, files["poster"], "poster.png"), posterFrame)
		if v := master["av01"]; v["VIDEO-RANGE"] != "SDR" || !regexp.MustCompile(`^av01\.0\.\d\dM\.08(,|$)`).MatchString(v["CODECS"]) {
			t.Fatalf("SDR AV1 variant: %v", v)
		}
		if v := master["avc1"]; v["VIDEO-RANGE"] != "SDR" {
			t.Fatalf("SDR H.264 variant: %v", v)
		}
	})
	t.Run("CK-J12-S10 the shipped player plays HDR only on an HDR screen whose decoder confirms it", func(t *testing.T) {
		if _, err := os.Stat(".browser-dist/video-player.html"); err != nil {
			t.Fatal("build the shipped UI and critical browser harness before this selected test: pnpm --dir ../../sdk/ui build; pnpm --dir ../../sdk/ui exec vite build --config ../../tests/critical/browser/vite.config.mjs")
		}
		if pq.ID == "" {
			t.Fatal("no PQ clip: CK-J12-S07 failed")
		}
		editor, token := m.person(t, 0)
		authtest.GrantRole(t, m.tenant[0], m.app.group, iam.UserSubject(editor.ID), m.writer)
		results, err := filepath.Abs("results/video-player")
		must(t, err)
		must(t, os.MkdirAll(results, 0o755))
		fixture, err := json.Marshal(map[string]any{"url": server.URL, "token": token, "ref": pq, "results": results})
		must(t, err)
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
		defer cancel()
		command := exec.CommandContext(ctx, "node", "browser/video-player.mjs")
		command.Env = append(os.Environ(), "CONTENTKIT_PLAYER_FIXTURE="+string(fixture))
		command.Cancel = func() error { return command.Process.Signal(os.Interrupt) }
		command.WaitDelay = 5 * time.Second
		var output bytes.Buffer
		command.Stdout, command.Stderr = &output, &output
		if err := command.Run(); err != nil {
			t.Fatalf("shipped player: %v\n%s", err, output.String())
		}
		var receipt struct{ Status string }
		if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &receipt); err != nil || receipt.Status != "passed" {
			t.Fatalf("browser did not complete the player journey: %v\n%s", err, output.String())
		}
	})
}

func hdrFFmpeg(t *testing.T, args ...string) {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "ffmpeg", append([]string{"-nostdin", "-v", "error", "-y", "-threads", "1"}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg %v: %v\n%s", args, err, out)
	}
}

// hdrLuma is a picture's luma mean and deviation on a 64×36 thumbnail.
func hdrLuma(t *testing.T, path string) [2]float64 {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "ffmpeg", "-v", "error", "-i", path, "-frames:v", "1", "-vf", "scale=64:36,format=gray", "-f", "rawvideo", "pipe:1").Output()
	must(t, err)
	var sum, sq float64
	for _, v := range out {
		sum += float64(v)
		sq += float64(v) * float64(v)
	}
	n := float64(len(out))
	return [2]float64{sum / n, math.Sqrt(sq/n - sum*sum/n/n)}
}

type hdrStreamInfo struct {
	PixFmt    string `json:"pix_fmt"`
	Transfer  string `json:"color_transfer"`
	Primaries string `json:"color_primaries"`
	Matrix    string `json:"color_space"`
	Range     string `json:"color_range"`
}

func hdrStream(t *testing.T, path string) hdrStreamInfo {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "ffprobe", "-v", "error", "-select_streams", "v:0", "-show_streams", "-of", "json", path).Output()
	must(t, err)
	var p struct{ Streams []hdrStreamInfo }
	must(t, json.Unmarshal(out, &p))
	if len(p.Streams) != 1 {
		t.Fatalf("%s has %d video streams", path, len(p.Streams))
	}
	return p.Streams[0]
}

// obuSideData is the first frame's side data dav1d reads from an AV1
// rendition's bitstream alone: its OBUs, without the MP4 boxes.
func obuSideData(t *testing.T, path string) string {
	t.Helper()
	obu := path + ".obu"
	hdrFFmpeg(t, "-i", path, "-c", "copy", "-f", "obu", obu)
	out, err := exec.CommandContext(t.Context(), "ffprobe", "-v", "error", "-read_intervals", "%+#1", "-show_frames", "-show_entries", "frame=side_data_list", "-of", "compact", obu).Output()
	must(t, err)
	return strings.TrimSpace(string(out))
}

// variants are a master playlist's STREAM-INF attributes by video codec
// (av01, avc1); each codec has one variant at this ladder.
func variants(t *testing.T, master []byte) map[string]map[string]string {
	t.Helper()
	attr := regexp.MustCompile(`([A-Z-]+)=("[^"]*"|[^,]*)`)
	out := map[string]map[string]string{}
	for _, line := range strings.Split(string(master), "\n") {
		if !strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
			continue
		}
		v := map[string]string{}
		for _, m := range attr.FindAllStringSubmatch(strings.TrimPrefix(line, "#EXT-X-STREAM-INF:"), -1) {
			v[m[1]] = strings.Trim(m[2], `"`)
		}
		if v["VIDEO-RANGE"] == "" {
			t.Fatalf("variant without VIDEO-RANGE: %s", line)
		}
		codec := v["CODECS"][:4]
		if out[codec] != nil {
			t.Fatalf("two %s variants: %s", codec, master)
		}
		out[codec] = v
	}
	return out
}
