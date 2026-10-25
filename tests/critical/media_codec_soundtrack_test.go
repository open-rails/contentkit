package critical_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/worker"
)

// Exercise the actual codecs and soundtrack, rather than treating a playlist
// or a successful encoder process as proof that the delivered media is usable.
func TestMediaModernFallbackAndSoundtrack(t *testing.T) {
	m := newResilientMedia(t, func(c *ck.Config) {
		c.Content.Kinds["clip"] = kinds.Kind{Route: "clips", Presentation: "player", Versions: kinds.VersionRules{Languages: "one", Hidden: true}}
		c.Media.Delivery.TTL = 5 * time.Minute
		c.Media.Config.Kinds = append(c.Media.Config.Kinds, media.Kind{Name: "clip", KeepOriginals: true,
			Uploads: []media.Upload{{Path: "source", Types: []string{"video/mp4"}, MaxBytes: 10 << 20}},
			Private: []media.Private{{Name: "stream", From: "source", To: "streams/", HLS: &media.HLS{Ladder: []int{144}}}},
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
				t.Errorf("stop actual codec worker: %v", err)
			}
		case <-time.After(20 * time.Second):
			t.Error("actual codec worker did not stop")
		}
	})

	source := filepath.Join(t.TempDir(), "source.mp4")
	command := exec.CommandContext(t.Context(), "ffmpeg", "-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=256x144:rate=12",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", "2", "-c:v", "libx264", "-threads", "1", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "aac", "-movflags", "+faststart", source)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generate audiovisual source: %v\n%s", err, output)
	}
	data, err := os.ReadFile(source)
	must(t, err)
	client := m.app.engine.Client()
	item, err := client.PostContents(t.Context(), catalog.ContentInput{Kind: "clip", Text: map[string]catalog.Text{"en": {Title: "Both codecs and a soundtrack"}}})
	must(t, err)
	if len(item.Versions) != 1 {
		t.Fatal("clip has no unique default version")
	}
	ref := media.RefBody{Kind: "clip", ID: item.ID, Version: item.Versions[0].ID}
	_, err = client.IngestMedia(t.Context(), http.DefaultClient, ref, uuid.NewString(), []ck.MediaFile{{Path: "source.mp4", Type: "video/mp4", Body: bytes.NewReader(data)}})
	must(t, err)
	ready := errors.New("media processing complete")
	watch, stopWatch := context.WithTimeout(t.Context(), 2*time.Minute)
	defer stopWatch()
	err = client.WatchMedia(watch, ref, "", func(update ck.MediaProgress) error {
		if update.Read == nil || update.Read.Status == nil {
			return nil
		}
		status := update.Read.Status
		if status.FailureCount != 0 {
			return fmt.Errorf("audiovisual processing failed: %+v", status.Failures)
		}
		if status.State == media.StateReady && status.Uploads.Total > 0 {
			return ready
		}
		return nil
	})
	if !errors.Is(err, ready) {
		t.Fatalf("observe actual codec processing: %v", err)
	}
	read, err := client.GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{Editor: true, Limit: 64})
	must(t, err)
	if len(read.HLS) != 1 {
		t.Fatalf("ready video has no unique HLS master: %+v", read.HLS)
	}
	codecs := map[string]bool{}
	for _, file := range read.Files {
		if file.Upload || file.Type != "video/mp4" && file.Type != "audio/mp4" {
			continue
		}
		response, err := http.Get(file.URL)
		must(t, err)
		body, err := io.ReadAll(io.LimitReader(response.Body, 10<<20))
		response.Body.Close()
		must(t, err)
		if response.StatusCode != 200 || len(body) == 0 {
			t.Fatalf("delivered rendition %s: HTTP%d, %d bytes", file.Path, response.StatusCode, len(body))
		}
		local := filepath.Join(t.TempDir(), "rendition.mp4")
		must(t, os.WriteFile(local, body, 0600))
		output, err := exec.CommandContext(t.Context(), "ffprobe", "-v", "error", "-show_entries", "stream=codec_name,codec_type", "-of", "json", local).CombinedOutput()
		if err != nil {
			t.Fatalf("probe delivered rendition %s: %v\n%s", file.Path, err, output)
		}
		var probe struct {
			Streams []struct {
				Codec string `json:"codec_name"`
				Type  string `json:"codec_type"`
			} `json:"streams"`
		}
		must(t, json.Unmarshal(output, &probe))
		for _, stream := range probe.Streams {
			codecs[stream.Type+":"+stream.Codec] = true
		}
	}
	for _, codec := range []string{"video:av1", "video:h264", "audio:aac"} {
		if !codecs[codec] {
			t.Fatalf("actual delivered %s missing: %v", codec, codecs)
		}
	}
	_, err = client.PostContentPublish(t.Context(), item.ID, catalog.PublishInput{})
	must(t, err)
	awaitEditionPublication(t, m.app, client, ref.ID, ref.Version)
	master, err := m.app.client("").GetMediaVersionHls(t.Context(), ref.Kind, ref.ID, ref.Version, "streams/master.m3u8", ck.GetMediaVersionHlsQuery{})
	must(t, err)
	for _, marker := range []string{"av01", "avc1", "mp4a.40.2", "TYPE=AUDIO", "AUDIO="} {
		if !strings.Contains(string(master.Body), marker) {
			t.Fatalf("public master omits %q: %s", marker, master.Body)
		}
	}
}
