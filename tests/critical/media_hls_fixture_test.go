package critical_test

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/worker"
)

// tinyHLSFixture is shared by public Go authorization and shipped TS return
// contracts. Both consume an actual two-second H264 video and real worker HLS.
type tinyHLSFixture struct {
	*resilientMedia
	Ref                               media.RefBody
	Source                            []byte
	MasterPath, ChildPath, SegmentURL string
	Reader                            *ck.Client
	private                           bool
}

func newTinyHLSFixture(t *testing.T, private ...bool) *tinyHLSFixture {
	return newTinyHLSConfiguredFixture(t, len(private) > 0 && private[0], nil)
}

func newTinyHLSConfiguredFixture(t *testing.T, private bool, configure func(*ck.Config)) *tinyHLSFixture {
	t.Helper()
	m := newResilientMedia(t, func(c *ck.Config) {
		c.Content.Kinds["clip"] = kinds.Kind{Route: "clips", Presentation: "player", Versions: kinds.VersionRules{Languages: "one", Hidden: true}}
		c.Media.Delivery.TTL = 5 * time.Minute
		c.Media.Config.Kinds = append(c.Media.Config.Kinds, media.Kind{Name: "clip", KeepOriginals: true,
			Uploads: []media.Upload{{Path: "source", Types: []string{"video/mp4"}, MaxBytes: 10 << 20}},
			Private: []media.Private{{Name: "stream", From: "source", To: "streams/", HLS: &media.HLS{Ladder: []int{144}}}},
		})
		site := c.Media.Config.Kinds[len(c.Media.Config.Kinds)-1]
		site.Name = "siteclip"
		c.Media.Config.Kinds = append(c.Media.Config.Kinds, site)
		if configure != nil {
			configure(c)
		}
	})
	config := m.config
	config.VideoCodecs = []media.Codec{media.CodecH264}
	config.VideoEncoder, config.Preset, config.TopPreset = "cpu", "ultrafast", "ultrafast"
	config.TempDir = t.TempDir()
	w, err := worker.New(t.Context(), config)
	must(t, err)
	run, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(run) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("stop tiny-video worker: %v", err)
			}
		case <-time.After(20 * time.Second):
			t.Error("tiny-video worker did not stop")
		}
	})
	source := filepath.Join(t.TempDir(), "source.mp4")
	command := exec.CommandContext(t.Context(), "ffmpeg", "-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=256x144:rate=12", "-t", "2", "-an", "-c:v", "libx264", "-threads", "1", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-movflags", "+faststart", source)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("create actual video: %v\n%s", err, output)
	}
	data, err := os.ReadFile(source)
	must(t, err)
	f := &tinyHLSFixture{resilientMedia: m, Source: data, MasterPath: "streams/master.m3u8", Reader: m.app.client(""), private: private}
	if f.private {
		// This fixture checks media capability scope, not billing grants.
		// The authorized editor receives an ordinary completed-reader view.
		f.Reader = m.app.engine.Client()
	}
	f.Ref = f.addClip(t, "Tiny public HLS")
	master, err := f.Reader.GetMediaVersionHls(t.Context(), f.Ref.Kind, f.Ref.ID, f.Ref.Version, f.MasterPath, ck.GetMediaVersionHlsQuery{})
	must(t, err)
	if master.Status != 200 || !bytes.Contains(master.Body, []byte("#EXT-X-STREAM-INF")) || !bytes.Contains(master.Body, []byte("avc1")) {
		t.Fatalf("actual H264 master invalid: %d %s", master.Status, master.Body)
	}
	f.ChildPath = path.Join(path.Dir(f.MasterPath), playlistURIForTest(t, master.Body))
	child, err := f.Reader.GetMediaVersionHls(t.Context(), f.Ref.Kind, f.Ref.ID, f.Ref.Version, f.ChildPath, ck.GetMediaVersionHlsQuery{})
	must(t, err)
	if !bytes.Contains(child.Body, []byte("#EXT-X-BYTERANGE:")) || !bytes.Contains(child.Body, []byte("#EXT-X-MAP:")) {
		t.Fatalf("actual HLS child has no byte-range segments: %s", child.Body)
	}
	f.SegmentURL = playlistURIForTest(t, child.Body)
	if !strings.HasPrefix(f.SegmentURL, m.storage.gateway.URL+"/") {
		t.Fatalf("child playlist bypasses the gateway: %s", f.SegmentURL)
	}
	return f
}

func (f *tinyHLSFixture) addSiteClip(t *testing.T) media.RefBody {
	t.Helper()
	id, err := uuid.NewV7()
	must(t, err)
	ref := media.RefBody{Kind: "siteclip", ID: id.String()}
	admin := f.app.engine.Client()
	_, err = admin.IngestMedia(t.Context(), nil, ref, uuid.NewString(), []ck.MediaFile{{Path: "source.mp4", Type: "video/mp4", Body: bytes.NewReader(f.Source)}})
	must(t, err)
	waitForWithin(t, 90*time.Second, func() bool {
		time.Sleep(550 * time.Millisecond)
		read, err := admin.GetMedia(t.Context(), ref.Kind, ref.ID, ck.GetMediaQuery{Editor: true})
		must(t, err)
		if read.Status != nil && read.Status.FailureCount != 0 {
			t.Fatalf("site video processing failed: %+v", read.Status.Failures)
		}
		return read.State == media.StateReady && len(read.HLS) == 1
	})
	return ref
}

func (f *tinyHLSFixture) addClip(t *testing.T, title string) media.RefBody {
	t.Helper()
	admin := f.app.engine.Client()
	item, err := admin.PostContents(t.Context(), catalog.ContentInput{Kind: "clip", Text: map[string]catalog.Text{"en": {Title: title}}})
	must(t, err)
	if len(item.Versions) != 1 {
		t.Fatal("clip must have its single hidden version")
	}
	ref := media.RefBody{Kind: "clip", ID: item.ID, Version: item.Versions[0].ID}
	if f.private {
		_, err = admin.PatchContentVersion(t.Context(), ref.ID, ref.Version, catalog.VersionInput{Access: &access.Rule{Level: access.PPV}})
		must(t, err)
	}
	_, err = admin.IngestMedia(t.Context(), http.DefaultClient, ref, uuid.NewString(), []ck.MediaFile{{Path: "source.mp4", Type: "video/mp4", Body: bytes.NewReader(f.Source)}})
	must(t, err)
	waitForWithin(t, 90*time.Second, func() bool {
		time.Sleep(550 * time.Millisecond)
		read, err := admin.GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{Editor: true})
		must(t, err)
		if read.Status != nil && read.Status.FailureCount != 0 {
			t.Fatalf("real video processing failed: %+v", read.Status.Failures)
		}
		return read.State == media.StateReady && len(read.HLS) == 1
	})
	_, err = admin.PostContentPublish(t.Context(), ref.ID, catalog.PublishInput{})
	must(t, err)
	awaitEditionPublication(t, f.app, admin, ref.ID, ref.Version)
	deadline := time.Now().Add(45 * time.Second)
	for {
		master, err := f.Reader.GetMediaVersionHls(t.Context(), ref.Kind, ref.ID, ref.Version, f.MasterPath, ck.GetMediaVersionHlsQuery{})
		if err == nil && master.Status == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("completed HLS publication unavailable: %v", err)
		}
		time.Sleep(550 * time.Millisecond)
	}
	return ref
}

func playlistURIForTest(t *testing.T, body []byte) string {
	t.Helper()
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			return line
		}
	}
	t.Fatalf("playlist has no playable URI: %s", body)
	return ""
}
