package critical_test

import (
	"bytes"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/discovery"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/media"
)

// hlsEnd is the longest track of a published ladder: where a player's
// timeline ends. Its media playlists are read through the public HLS route.
func hlsEnd(t *testing.T, reader *ck.Client, ref media.RefBody, master string) float64 {
	t.Helper()
	read := func(p string) string {
		t.Helper()
		got, err := reader.GetMediaVersionHls(t.Context(), ref.Kind, ref.ID, ref.Version, p, ck.GetMediaVersionHlsQuery{})
		must(t, err)
		if got.Status != http.StatusOK {
			t.Fatalf("playlist %s: %d %s", p, got.Status, got.Body)
		}
		return string(got.Body)
	}
	var children []string
	for _, line := range strings.Split(read(master), "\n") {
		line = strings.TrimSpace(line)
		if _, uri, ok := strings.Cut(line, `URI="`); ok && strings.HasPrefix(line, "#EXT-X-MEDIA:") {
			children = append(children, uri[:strings.IndexByte(uri, '"')])
		} else if line != "" && !strings.HasPrefix(line, "#") {
			children = append(children, line)
		}
	}
	var end float64
	for _, child := range children {
		child, _, _ = strings.Cut(child, "?")
		var sum float64
		for _, line := range strings.Split(read(path.Join(path.Dir(master), child)), "\n") {
			if d, ok := strings.CutPrefix(strings.TrimSpace(line), "#EXTINF:"); ok {
				d, _, _ = strings.Cut(d, ",")
				seconds, err := strconv.ParseFloat(d, 64)
				must(t, err)
				sum += seconds
			}
		}
		end = max(end, sum)
	}
	if end == 0 {
		t.Fatal("the ladder lists no timed media playlist")
	}
	return end
}

// TestVideoCompletion watches a real encoded video to the end of its HLS
// rendition. Its source's audio outlasts its video by 600 ms, as encodes
// often do by a few frames: the version's duration_ms is the probed video
// stream, the player's timeline the longer rendition.
func TestVideoCompletion(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	configureDiscoverySignals(t, w, a)
	attachMedia(t, w, a, func(c *ck.Config) {
		c.Content.Kinds["clip"] = kinds.Kind{Route: "clips", Presentation: "player", Versions: kinds.VersionRules{Languages: "one", Hidden: true}}
		c.Media.Config.Kinds = append(c.Media.Config.Kinds, media.Kind{Name: "clip", KeepOriginals: true,
			Uploads: []media.Upload{{Path: "source", Types: []string{"video/mp4"}, MaxBytes: 10 << 20}},
			Private: []media.Private{{Name: "stream", From: "source", To: "streams/", HLS: &media.HLS{Ladder: []int{144}}}},
		})
	})
	ctx := t.Context()
	source := filepath.Join(t.TempDir(), "source.mp4")
	encode := exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-v", "error",
		"-f", "lavfi", "-i", "testsrc2=size=256x144:rate=12:duration=3",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=3.6",
		"-map", "0:v", "-map", "1:a", "-c:v", "libx264", "-threads", "1", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-b:a", "64k", "-movflags", "+faststart", source)
	if output, err := encode.CombinedOutput(); err != nil {
		t.Fatalf("create actual video: %v\n%s", err, output)
	}
	data, err := os.ReadFile(source)
	must(t, err)

	admin := a.engine.Client()
	item, err := admin.PostContents(ctx, catalog.ContentInput{Kind: "clip", Versions: []catalog.VersionInput{{Languages: []string{"en"},
		Text: map[string]catalog.VersionText{"en": {Title: "Watched to the end"}}}}})
	must(t, err)
	ref := media.RefBody{Kind: "clip", ID: item.ID, Version: item.Versions[0].ID}
	_, err = admin.IngestMedia(ctx, http.DefaultClient, ref, uuid.NewString(), []ck.MediaFile{{Path: "source.mp4", Type: "video/mp4", Body: bytes.NewReader(data)}})
	must(t, err)
	waitForWithin(t, 2*time.Minute, func() bool {
		time.Sleep(50 * time.Millisecond)
		read, err := admin.GetMediaVersion(ctx, ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{Editor: true})
		must(t, err)
		if read.Status != nil && read.Status.FailureCount != 0 {
			t.Fatalf("real video processing failed: %+v", read.Status.Failures)
		}
		return read.State == media.StateReady && len(read.HLS) == 1
	})
	_, err = admin.PostContentPublish(ctx, ref.ID, catalog.PublishInput{})
	must(t, err)
	awaitEditionPublication(t, a, admin, ref.ID, ref.Version)
	version, err := admin.GetContentVersion(ctx, ref.ID, ref.Version, ck.GetContentVersionQuery{})
	must(t, err)
	if version.DurationMS == nil || *version.DurationMS <= 0 {
		t.Fatalf("published video has no duration: %+v", version.DurationMS)
	}
	stored := uint32(*version.DurationMS)

	_, credential := w.person(t, 0)
	reader := a.client(credential)
	end := uint32(math.Round(hlsEnd(t, reader, ref, "streams/master.m3u8") * 1000))
	if end <= stored {
		t.Fatalf("the rendition ends at %d ms, within the stored %d ms: the fixture does not reproduce a longer timeline", end, stored)
	}
	contexts, err := reader.PostActivityContexts(ctx, discovery.ActivityContextsInput{Targets: []discovery.ActivityTarget{{ContentID: ref.ID, VersionID: ref.Version}}})
	must(t, err)
	if !contexts.Enabled || len(contexts.Contexts) != 1 {
		t.Fatalf("no activity receipt: %+v", contexts)
	}
	// The viewer then watches for longer than the rendition runs.
	w.clock.set(w.clock.now().Add(30 * time.Second))
	watched := func(to uint32) discovery.EngagementBatch {
		return discovery.EngagementBatch{Snapshots: []discovery.EngagementSnapshot{{ContentID: ref.ID, VersionID: ref.Version,
			VisitID: uuid.NewString(), SegmentID: uuid.NewString(), Revision: 1, ActivityToken: contexts.Contexts[0].Token,
			ActiveMS: to, Watched: []discovery.WatchedRange{{FromMS: 0, ToMS: to}}}}}
	}

	t.Run("CK-J07-S35_watching_the_rendition_to_its_end_records_a_completion", func(t *testing.T) {
		if err := reader.PostEngagement(ctx, watched(end)); err != nil {
			t.Fatalf("watching %d ms of a rendition stored as %d ms was refused: %v", end, stored, err)
		}
		waitFor(t, func() bool {
			done, err := reader.GetMeHistory(ctx, ck.GetMeHistoryQuery{Kind: "clip", Status: "completed"})
			must(t, err)
			return len(done.Contents) == 1 && done.Contents[0].ID == ref.ID
		})
	})

	t.Run("CK-J07-S35_a_range_past_the_duration_and_its_tolerance_is_refused", func(t *testing.T) {
		// Two seconds past the stored duration is the tolerated overrun.
		err := reader.PostEngagement(ctx, watched(stored+2_001))
		requireError(t, err, 400, "invalid_request")
	})
}
