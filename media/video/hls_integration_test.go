package video_test

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/video"
	"github.com/open-rails/contentkit/media/workqueue"
)

// A 720p source with two audio tracks and a text track, in AV1 and H.264:
// the ladder is its own 720 rung and 480, each a byte-range fMP4 with its
// segment index; the audio and text tracks and the sprite sit beside them.
// mp4-480 is remuxed from the 480 H.264 rendition, mp4-360 is its own H.264
// run, and mp4-1080 (above the source) has no output.
func TestLadderTracksAndMP4(t *testing.T) {
	e := newEnv(t, opts{codecs: []media.Codec{media.CodecAV1, media.CodecH264}, mp4: []int{1080, 480, 360}})
	e.start()
	e.put("source", "video/x-matroska", fixture{w: 1280, h: 720, secs: 9, audio: 2, subs: true, tone: 440}.make(t), nil)
	e.wait()

	m := e.manifest()
	src := e.file(m, "source.mkv")
	if src.W != 1280 || src.H != 720 || math.Abs(src.Dur-9) > 0.1 || len(src.Pending) != 0 || src.Failed != nil {
		t.Fatalf("source %+v", src)
	}
	if r := e.readiness(m); !r.Ready() {
		t.Fatalf("readiness %+v", r)
	}
	want := []string{"hls/720-av1.mp4", "hls/480-av1.mp4", "hls/720-h264.mp4", "hls/480-h264.mp4",
		"hls/audio-a1.mp4", "hls/audio-a2.mp4", "hls/subs-s1.vtt", "hls/sprite.jpg"}
	if got := outputPaths(m, "source.mkv", "hls"); !slices.Equal(got, want) {
		t.Fatalf("hls outputs %v, want %v", got, want)
	}
	hls := m.Outputs("source.mkv", "hls")
	fp := hls[0].FP
	var segs [][]media.Segment
	for _, o := range hls {
		if o.FP == "" || o.Track == nil || (o.Track.Kind == media.TrackSubs) != (o.FP != fp) {
			t.Fatalf("output %s fp %q (renditions %q) track %+v", o.Path, o.FP, fp, o.Track)
		}
		switch o.Track.Kind {
		case media.TrackVideo:
			idx := e.index(o)
			path := e.blob(o.Blob)
			p := ffprobe(t, path)
			prefix := map[string]string{"av1": "av01.0.", "h264": "avc1.64"}[o.Track.Codec]
			if p.Streams[0].Width != o.W || p.Streams[0].Height != o.H || !strings.HasPrefix(o.Track.Codecs, prefix) ||
				o.Track.Bandwidth <= 0 || o.Track.Average <= 0 || o.Track.Average > o.Track.Bandwidth || o.Type != "video/mp4" {
				t.Fatalf("rendition %s %+v %+v probed %dx%d", o.Path, o, o.Track, p.Streams[0].Width, p.Streams[0].Height)
			}
			checkByteRanges(t, path, idx.Segments, "video", 9)
			segs = append(segs, idx.Segments)
		case media.TrackAudio:
			checkByteRanges(t, e.blob(o.Blob), e.index(o).Segments, "audio", 9)
		case media.TrackSubs:
			if vtt := string(must(os.ReadFile(e.blob(o.Blob)))); !strings.HasPrefix(vtt, "WEBVTT") || !strings.Contains(vtt, "World") {
				t.Fatalf("source subtitles %q", vtt)
			}
		case media.TrackSprite:
			if s := e.index(o).Sprite; s == nil || s.Cols != 10 || s.Rows != 10 || s.W != 160 || s.H != 90 || math.Abs(s.Interval-0.09) > 0.01 {
				t.Fatalf("sprite grid %+v", s)
			}
			if p := ffprobe(t, e.blob(o.Blob)); p.Streams[0].Width != 1600 || p.Streams[0].Height != 900 || o.W != 1600 {
				t.Fatalf("sprite %dx%d, file %+v", p.Streams[0].Width, p.Streams[0].Height, o)
			}
		}
	}
	for _, s := range segs[1:] {
		if len(s) != len(segs[0]) || s[0].Seconds != segs[0][0].Seconds {
			t.Fatal("renditions differ in segments")
		}
	}
	a1, a2, subs := e.file(m, "hls/audio-a1.mp4").Track, e.file(m, "hls/audio-a2.mp4").Track, e.file(m, "hls/subs-s1.vtt").Track
	if a1.ID != "a1" || a1.Lang != "ja" || !a1.Default || a1.Codecs != "mp4a.40.2" || a2.Label != "Commentary" || a2.Lang != "en" || a2.Default ||
		subs.ID != "s1" || subs.Lang != "en" {
		t.Fatalf("tracks %+v %+v %+v", a1, a2, subs)
	}

	for _, c := range []struct {
		path string
		w, h int
	}{{"video/source-480p.mp4", 854, 480}, {"video/source-360p.mp4", 640, 360}} {
		f := e.file(m, c.path)
		path := e.blob(f.Blob)
		p := ffprobe(t, path)
		body := must(os.ReadFile(path))
		if f.W != c.w || f.H != c.h || math.Abs(f.Dur-9) > 0.1 || f.FP == "" || p.count("video") != 1 || p.count("audio") != 1 ||
			p.Streams[0].CodecName != "h264" || p.Streams[0].Width != c.w || p.Streams[1].Disposition.Default != 1 ||
			bytes.Index(body, []byte("moov")) > bytes.Index(body, []byte("mdat")) {
			t.Fatalf("mp4 %s %+v probed %+v", c.path, f, p)
		}
	}
	if m.Find("video/source-1080p.mp4") >= 0 {
		t.Fatal("an MP4 above the source")
	}
	if got := e.runs(); !slices.Equal(got, []string{"hls:480", "hls:720", "mp4-360:360"}) {
		t.Fatalf("encode runs %v", got)
	}
}

// Each rung is published before the next: the first stage with the tracks
// and the sprite, the upload pending until the last. Every rendition has a
// keyframe every 4 s and the same segments, so a player switches between
// the stages' rungs seamlessly. Progress names the upload and its stage.
func TestProgressiveStages(t *testing.T) {
	e := newEnv(t, opts{ladder: []int{720, 480, 240}})
	progress, err := workqueue.NewProgressSource(e.pool, e.schema)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var stages [][2]int
	done := make(chan struct{})
	polled := make(chan struct{})
	go func() {
		defer close(polled)
		for {
			select {
			case <-done:
				return
			case <-time.After(50 * time.Millisecond):
			}
			st, err := progress.EncodeProgress(e.ctx, e.ref)
			if p, ok := st.Files["source.mkv"]; err == nil && ok && p.Stages > 0 {
				mu.Lock()
				if len(stages) == 0 || stages[len(stages)-1] != [2]int{p.Stage, p.Stages} {
					stages = append(stages, [2]int{p.Stage, p.Stages})
				}
				mu.Unlock()
			}
		}
	}()
	e.start()
	e.put("source", "video/x-matroska", fixture{w: 1280, h: 720, secs: 9, audio: 1, tone: 440}.make(t), nil)
	var first map[string]string
	for stage := 1; stage <= 3; stage++ {
		e.next((workqueue.VideoAssembleArgs{}).Kind())
		m := e.manifest()
		src := e.file(m, "source.mkv")
		var ladder []string
		blobs := map[string]string{}
		for _, o := range m.Outputs("source.mkv", "hls") {
			blobs[o.Path] = o.Blob
			if o.Track.Kind == media.TrackVideo {
				ladder = append(ladder, o.Path)
			}
		}
		want := [][]string{nil, {"hls/240-h264.mp4"}, {"hls/480-h264.mp4", "hls/240-h264.mp4"},
			{"hls/720-h264.mp4", "hls/480-h264.mp4", "hls/240-h264.mp4"}}[stage]
		pending := slices.Contains(src.Pending, "hls")
		if !slices.Equal(ladder, want) || pending != (stage < 3) || e.readiness(m).Ready() != (stage == 3) {
			t.Fatalf("stage %d: ladder %v pending %v readiness %+v", stage, ladder, src.Pending, e.readiness(m))
		}
		if stage == 1 {
			first = blobs
		} else if blobs["hls/audio-a1.mp4"] != first["hls/audio-a1.mp4"] || blobs["hls/sprite.jpg"] != first["hls/sprite.jpg"] ||
			blobs["hls/240-h264.mp4"] != first["hls/240-h264.mp4"] {
			t.Fatalf("stage %d changed the first stage's outputs", stage)
		}
	}
	e.wait()
	close(done)
	<-polled
	mu.Lock()
	if len(stages) == 0 || slices.ContainsFunc(stages, func(s [2]int) bool { return s[1] != 3 || s[0] < 1 || s[0] > 3 }) {
		t.Fatalf("progress stages %v", stages)
	}
	mu.Unlock()

	m := e.manifest()
	var paths []string
	var segs [][]media.Segment
	for _, o := range m.Outputs("source.mkv", "hls") {
		if o.Track.Kind != media.TrackVideo {
			continue
		}
		path := e.blob(o.Blob)
		idx := e.index(o)
		checkByteRanges(t, path, idx.Segments, "video", 9)
		if k := keyframes(t, path); len(k) != 3 || k[2]-k[1] != 4000 {
			t.Fatalf("%s keyframes %v", o.Path, k)
		}
		paths, segs = append(paths, path), append(segs, idx.Segments)
	}
	switchRungs(t, paths, segs)
	var left int
	if err := e.pool.QueryRow(e.ctx, "SELECT count(*) FROM "+e.schema+".river_job WHERE metadata ? 'contentkit_progress'").Scan(&left); err != nil || left != 0 {
		t.Fatalf("progress left on %d jobs (%v)", left, err)
	}
}

// A replaced source's outputs keep serving until the new source's first
// stage replaces them; then every output has the new fingerprint.
func TestReplacedSource(t *testing.T) {
	e := newEnv(t, opts{ladder: []int{360}, mp4: []int{360}})
	var mu sync.Mutex
	var atPublish *media.Manifest
	var second string
	defer video.SetBeforePublish(func() {
		mu.Lock()
		defer mu.Unlock()
		if m, _, err := e.ms.Get(e.ctx, e.ref); err == nil && atPublish == nil && second != "" && strings.HasPrefix(sourceBlob(m), second+"-") {
			atPublish = m
		}
	})()
	e.start()
	defer e.stopWorker()
	e.put("source", "video/x-matroska", fixture{w: 640, h: 360, secs: 3, audio: 1, tone: 440}.make(t), nil)
	e.wait()
	before := e.manifest()

	b := fixture{w: 640, h: 360, secs: 3, rate: 12, audio: 1, tone: 880}.make(t)
	mu.Lock()
	second = blobOf(t, b)
	mu.Unlock()
	if m := e.put("source", "video/x-matroska", b, nil); !slices.Equal(e.file(m, "source.mkv").Pending, []string{"hls", "mp4-360"}) {
		t.Fatalf("pending after the put %v", e.file(m, "source.mkv").Pending)
	}
	e.wait()
	mu.Lock()
	defer mu.Unlock()
	if atPublish == nil {
		t.Fatal("no publish for the new source")
	}
	for _, o := range before.Outputs("source.mkv", "hls") {
		if f := e.file(atPublish, o.Path); f.Blob != o.Blob || f.FP != o.FP {
			t.Fatalf("%s changed before the new source's first stage", o.Path)
		}
	}
	after := e.manifest()
	for _, preset := range []string{"hls", "mp4-360"} {
		old, cur := before.Outputs("source.mkv", preset), after.Outputs("source.mkv", preset)
		if len(cur) != len(old) || len(cur) == 0 {
			t.Fatalf("%s outputs %d, before %d", preset, len(cur), len(old))
		}
		for i := range cur {
			if cur[i].FP == old[i].FP || cur[i].Path != old[i].Path || cur[i].Type == "video/mp4" && cur[i].Blob == old[i].Blob {
				t.Fatalf("%s %s kept its old blob or fingerprint", preset, cur[i].Path)
			}
		}
	}
	if r := e.readiness(after); !r.Ready() {
		t.Fatalf("readiness %+v", r)
	}
}

// regenerate redoes only the named preset with force, and a deploy that
// changes a recipe (an output's fingerprint no longer matching) is redone
// with the upload marked pending meanwhile.
func TestRegenerate(t *testing.T) {
	e := newEnv(t, opts{ladder: []int{360}, mp4: []int{240}})
	var mu sync.Mutex
	var pendingAtPublish [][]string
	defer video.SetBeforePublish(func() {
		mu.Lock()
		defer mu.Unlock()
		if m, _, err := e.ms.Get(e.ctx, e.ref); err == nil {
			src, _ := m.Get("source.mkv")
			pendingAtPublish = append(pendingAtPublish, slices.Clone(src.Pending))
		}
	})()
	e.start()
	defer e.stopWorker()
	e.put("source", "video/x-matroska", fixture{w: 640, h: 360, secs: 3, audio: 1, tone: 440}.make(t), nil)
	e.wait()
	before := e.manifest()
	assembles := func() int {
		var n int
		if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM `+e.schema+`.river_job WHERE kind = $1 AND state = 'completed'`,
			(workqueue.VideoAssembleArgs{}).Kind()).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	n := assembles()

	e.commit(media.Op{Op: media.OpRegenerate, Preset: "mp4-240", Force: true})
	e.wait()
	after := e.manifest()
	if assembles() != n+1 || !slices.Equal(outputPaths(after, "source.mkv", "mp4-240"), []string{"video/source-240p.mp4"}) {
		t.Fatalf("forced mp4-240: %d assembles after %d", assembles(), n)
	}
	for _, preset := range []string{"hls", "mp4-240"} {
		for i, o := range before.Outputs("source.mkv", preset) {
			if cur := after.Outputs("source.mkv", preset)[i]; cur.Blob != o.Blob || cur.FP != o.FP {
				t.Fatalf("%s %s: %s/%s, before %s/%s", preset, o.Path, cur.Blob, cur.FP, o.Blob, o.FP)
			}
		}
	}

	// A deploy changed the HLS recipe: its outputs are stale, nothing pending.
	if _, err := e.ms.EditExisting(e.ctx, e.ref, func(m *media.Manifest) error {
		for i := range m.Files {
			if m.Files[i].Preset == "hls" {
				m.Files[i].FP = "old-recipe"
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	pendingAtPublish = nil
	mu.Unlock()
	if err := e.queue.Enqueue(e.ctx, media.ProcessJob{Ref: e.ref}); err != nil {
		t.Fatal(err)
	}
	e.wait()
	redone := e.manifest()
	for i, o := range redone.Outputs("source.mkv", "hls") {
		if was := before.Outputs("source.mkv", "hls")[i]; o.FP != was.FP || o.Blob != was.Blob {
			t.Fatalf("redone %s: %s/%s, want %s/%s", o.Path, o.Blob, o.FP, was.Blob, was.FP)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(pendingAtPublish) == 0 || !slices.Contains(pendingAtPublish[0], "hls") || len(e.file(redone, "source.mkv").Pending) != 0 {
		t.Fatalf("pending while redone %v, after %v", pendingAtPublish, e.file(redone, "source.mkv").Pending)
	}
}

// Rungs are short sides; w×h follows the source aspect, capped at 4096 per
// side and a 3840×2160 area. A source outside the aspect bounds fails once
// (Hooks.Failed), and so does the frame grabbed from it.
func TestAspects(t *testing.T) {
	e := newEnv(t, opts{ladder: []int{2000, 180, 120}})
	e.start()
	cases := []struct {
		src          fixture
		want         []string // rendition w×h, largest first
		tileW, tileH int
		codec        string
	}{
		{fixture{w: 420, h: 180, secs: 2}, []string{"420x180", "280x120"}, 210, 90, "avc1.64"},
		{fixture{w: 180, h: 420, secs: 2}, []string{"180x420", "120x280"}, 90, 210, "avc1.64"},
		// Over 4K: a 4800-wide 21:9 source is capped to 4096×1756 at level 5.1.
		{fixture{w: 4800, h: 2058, secs: 2, rate: 2}, []string{"4096x1756", "420x180", "280x120"}, 210, 90, "avc1.640033"},
	}
	for i, c := range cases {
		e.ref = e.refOf("video", 10+i)
		e.put("source", "video/x-matroska", c.src.make(t), nil)
	}
	e.ref = e.refOf("video", 20)
	e.put("source", "video/x-matroska", fixture{w: 480, h: 180, secs: 1}.make(t), nil, // 2.67:1, wider than 2.4:1
		media.Op{Op: media.OpFrame, Path: "poster", Auto: true})
	e.wait()

	for i, c := range cases {
		e.ref = e.refOf("video", 10+i)
		m := e.manifest()
		var got []string
		for _, o := range m.Outputs("source.mkv", "hls") {
			switch o.Track.Kind {
			case media.TrackVideo:
				got = append(got, fmt.Sprintf("%dx%d", o.W, o.H))
				if p := ffprobe(t, e.blob(o.Blob)); p.Streams[0].Width != o.W || p.Streams[0].Height != o.H {
					t.Fatalf("%s is %dx%d", o.Path, p.Streams[0].Width, p.Streams[0].Height)
				}
			case media.TrackSprite:
				if s := e.index(o).Sprite; s.W != c.tileW || s.H != c.tileH {
					t.Fatalf("sprite tile %dx%d, want %dx%d", s.W, s.H, c.tileW, c.tileH)
				}
			}
		}
		top := m.Outputs("source.mkv", "hls")[0]
		if !slices.Equal(got, c.want) || !strings.HasPrefix(top.Track.Codecs, c.codec) {
			t.Fatalf("%dx%d: ladder %v (%s), want %v %s", c.src.w, c.src.h, got, top.Track.Codecs, c.want, c.codec)
		}
	}

	e.ref = e.refOf("video", 20)
	m := e.manifest()
	src, poster := e.file(m, "source.mkv"), e.file(m, "poster.png")
	if fail := src.Fail(); fail == nil || !strings.Contains(fail.Message, "480x180") || len(src.Pending) != 0 ||
		len(m.Outputs("source.mkv", "hls")) != 0 || poster.Fail() == nil || e.readiness(m).State != media.StateFailed {
		t.Fatalf("source %+v poster %+v readiness %+v", src, poster, e.readiness(m))
	}
	var aspect, frame int
	for _, f := range e.failed() {
		switch {
		case f.ref == e.ref && f.path == "source.mkv" && errors.Is(f.err, video.ErrAspect):
			aspect++
		case f.ref == e.ref && f.path == "poster.png":
			frame++
		}
	}
	if aspect != 1 || frame != 1 {
		t.Fatalf("failures %+v", e.failed())
	}
	// Another job leaves the failure alone.
	if err := e.queue.Enqueue(e.ctx, media.ProcessJob{Ref: e.ref}); err != nil {
		t.Fatal(err)
	}
	e.wait()
	if len(e.failed()) != 2 {
		t.Fatalf("failures after a retry %+v", e.failed())
	}
}

func sourceBlob(m *media.Manifest) string {
	f, _ := m.Get("source.mkv")
	return f.Blob
}
