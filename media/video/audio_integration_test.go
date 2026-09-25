package video_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
	"github.com/open-rails/contentkit/media/token"
	"github.com/open-rails/contentkit/media/video"
	"github.com/open-rails/contentkit/media/workqueue"
)

var audioTypes = []string{"audio/mpeg", "audio/flac", "audio/wav", "audio/ogg", "audio/mp4"}

// newAudioEnv is newEnv over kind "video" that also takes audio files with
// settings a; failed collects Hooks.Failed.
func newAudioEnv(t *testing.T, a media.Audio, queue media.ProcessQueue) (*env, *[]string) {
	t.Helper()
	requireFFmpeg(t)
	s3 := s3test.Open(t)
	e := &env{Env: s3, store: s3.Store}
	var err error
	if e.kinds, err = media.NewRegistry(media.Kind{Name: "video", Versioned: true, Video: &media.Video{PosterWidths: posterWidths},
		Audio: &a, Types: slices.Concat([]string{"video/x-matroska"}, audioTypes, media.SubtitleTypes)}); err != nil {
		t.Fatal(err)
	}
	locker := s3test.Locker(t, e.store)
	if e.manifests, err = media.NewManifests(e.store, e.kinds, media.ManifestOptions{Locker: locker}); err != nil {
		t.Fatal(err)
	}
	if e.uploads, err = media.NewUploads(media.UploadOptions{Store: e.store, Kinds: e.kinds, Manifests: e.manifests,
		Authorizer: grants{}, Queue: queue}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	failed := &[]string{}
	hooks := media.Hooks{Failed: func(_ context.Context, _ contentref.ContentRef, file string, _ error) {
		mu.Lock()
		*failed = append(*failed, file)
		mu.Unlock()
	}}
	if e.encoder, err = video.New(video.Config{Store: e.store, Locker: locker, TempDir: t.TempDir(), Threads: 2,
		Encoder: video.EncoderCPU, Hooks: hooks}); err != nil {
		t.Fatal(err)
	}
	e.ref = contentref.NewVersion(s3.Tenant, "video", cid(88), "v1")
	return e, failed
}

type audioFixture struct {
	ext, codec string
	secs, rate int
	mono       bool
	volume     float64 // dB
	lang       string
}

func (f audioFixture) make(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "source."+f.ext)
	args := []string{"-v", "error", "-nostdin", "-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=440:duration=%d:sample_rate=%d", f.secs, f.rate)}
	if f.volume != 0 {
		args = append(args, "-af", fmt.Sprintf("volume=%gdB", f.volume))
	}
	if !f.mono {
		args = append(args, "-ac", "2")
	}
	if f.lang != "" {
		args = append(args, "-metadata", "language="+f.lang, "-metadata", "title=Opening")
	}
	args = append(args, "-c:a", f.codec, "-y", out)
	if b, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("fixture %s: %v: %s", f.ext, err, b)
	}
	return out
}

// commitFile uploads path as an original of type typ and inserts or replaces file name.
func (e *env) commitFile(t *testing.T, path, name, typ, op string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	orig := media.SHA256Name(sum[:])
	key, _ := e.item(t).Original(orig)
	if _, err := e.store.Put(context.Background(), key, bytes.NewReader(body), int64(len(body)),
		media.PutOptions{ContentType: typ, ChecksumSHA256: sum[:]}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.uploads.Commit(context.Background(), admin, e.ref, []media.Op{{Op: op, Name: name, Original: orig}}); err != nil {
		t.Fatal(err)
	}
	return orig
}

func encodeAudio(t *testing.T, e *env, a media.Audio, report video.Report) {
	t.Helper()
	if err := e.encoder.Encode(context.Background(), video.Job{Ref: e.ref, Versioned: true, Audio: &a}, report); err != nil {
		t.Fatal(err)
	}
}

// loudness is a file's EBU R128 integrated loudness in LUFS.
func loudness(t *testing.T, path string) float64 {
	t.Helper()
	b, err := exec.Command("ffmpeg", "-hide_banner", "-nostats", "-nostdin", "-i", path, "-af", "ebur128", "-f", "null", "-").CombinedOutput()
	if err != nil {
		t.Fatalf("ebur128 %s: %v: %s", path, err, b)
	}
	m := regexp.MustCompile(`(?s)Integrated loudness:\s+I:\s+(-?[\d.]+) LUFS`).FindSubmatch(b)
	if m == nil {
		t.Fatalf("no integrated loudness in %s", b)
	}
	v, _ := strconv.ParseFloat(string(m[1]), 64)
	return v
}

func TestAudioEncode(t *testing.T) {
	e, failed := newAudioEnv(t, media.Audio{}, nil)
	files := map[string]audioFixture{
		"a.mp3":  {ext: "mp3", codec: "libmp3lame", secs: 9, rate: 44100, lang: "jpn"},
		"b.flac": {ext: "flac", codec: "flac", secs: 5, rate: 96000, mono: true},
		"c.wav":  {ext: "wav", codec: "pcm_s16le", secs: 5, rate: 22050},
		"d.ogg":  {ext: "ogg", codec: "libvorbis", secs: 5, rate: 48000},
		"e.m4a":  {ext: "m4a", codec: "aac", secs: 5, rate: 44100},
	}
	types := map[string]string{"mp3": "audio/mpeg", "flac": "audio/flac", "wav": "audio/wav", "ogg": "audio/ogg", "m4a": "audio/mp4"}
	names := slices.Sorted(maps.Keys(files))
	for _, n := range names {
		e.commitFile(t, files[n].make(t), n, types[files[n].ext], media.OpInsert)
	}
	junk := filepath.Join(t.TempDir(), "junk.mp3")
	if err := os.WriteFile(junk, bytes.Repeat([]byte("not audio "), 500), 0o600); err != nil {
		t.Fatal(err)
	}
	e.commitFile(t, junk, "junk.mp3", "audio/mpeg", media.OpInsert)

	var mu sync.Mutex
	phases := map[string]bool{}
	encodeAudio(t, e, media.Audio{}, func(_ context.Context, fs map[string]media.EncodeProgress) {
		mu.Lock()
		defer mu.Unlock()
		for _, p := range fs {
			phases[p.Phase] = true
		}
	})
	for _, p := range []string{media.PhaseDownloading, media.PhaseProbing, media.PhaseEncoding, media.PhaseMuxing, media.PhaseUploading, media.PhasePublishing} {
		if !phases[p] {
			t.Errorf("no %s progress; saw %v", p, phases)
		}
	}

	m, etag := e.manifest(t)
	for _, n := range names {
		f := m.Files[m.File(n)]
		h := f.HLS
		if h == nil || h.Error != "" || len(h.Video) != 0 || len(h.Audio) != 1 || h.Source != f.Source() || f.State() != media.StateReady {
			t.Fatalf("%s: hls %+v state %s", n, h, f.State())
		}
		a := h.Audio[0]
		if a.ID != "a1" || !a.Default || a.Codecs != "mp4a.40.2" || a.Bandwidth <= 0 {
			t.Fatalf("%s: track %+v", n, a)
		}
		if n == "a.mp3" && (a.Lang != "ja" || a.Label != "Opening") {
			t.Fatalf("%s: track language %q label %q", n, a.Lang, a.Label)
		}
		secs := float64(files[n].secs)
		if d, _ := f.Meta["duration"].(float64); math.Abs(d-secs) > 0.2 {
			t.Fatalf("%s: duration %v", n, f.Meta["duration"])
		}
		checkByteRanges(t, e.blob(t, a.Blob), a.Segments, "audio", secs)

		v, ok := f.Variants[media.AudioVariant]
		dl, dok := m.Downloads[media.AudioDownloadKey(n)]
		if !ok || !dok || v.Blob != dl.Blob || v.Type != "audio/mp4" || dl.Type != "audio/mp4" || v.Size != dl.Size || dl.Inputs != f.Source() {
			t.Fatalf("%s: variant %+v download %+v", n, v, dl)
		}
		path := e.blob(t, v.Blob)
		p := ffprobe(t, path)
		body, _ := os.ReadFile(path)
		if p.count("audio") != 1 || p.count("video") != 0 || p.Streams[0].CodecName != "aac" || math.Abs(p.duration(t)-secs) > 0.2 ||
			bytes.Index(body, []byte("moov")) > bytes.Index(body, []byte("mdat")) {
			t.Fatalf("%s: m4a %+v (moov after mdat?)", n, p)
		}
		sr := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=sample_rate,channels", "-of", "csv=p=0", path)
		if out, err := sr.Output(); err != nil || strings.TrimSpace(string(out)) != "48000,2" {
			t.Fatalf("%s: m4a stream %q %v", n, out, err)
		}
	}
	junkFile := m.Files[m.File("junk.mp3")]
	if h := junkFile.HLS; h == nil || h.Error == "" || junkFile.State() != media.StateFailed || len(junkFile.Variants) != 0 ||
		!slices.Equal(*failed, []string{"junk.mp3"}) {
		t.Fatalf("junk: %+v, failed %v", junkFile, *failed)
	}
	if _, ok := m.Downloads[media.AudioDownloadKey("junk.mp3")]; ok {
		t.Fatal("failed file has a download")
	}
	if servable := m.Servable(); servable.File("junk.mp3") >= 0 || servable.File("a.mp3") < 0 {
		t.Fatal("viewers see a failed audio file, or miss an encoded one")
	}

	// Fresh files are skipped: no manifest write, no new blobs.
	blobs := e.blobs(t)
	encodeAudio(t, e, media.Audio{}, nil)
	if _, again := e.manifest(t); again != etag || !slices.Equal(e.blobs(t), blobs) {
		t.Fatal("a fresh manifest was re-encoded")
	}

	// A removed file loses its download on the next pass.
	if _, err := e.uploads.Commit(context.Background(), admin, e.ref, []media.Op{{Op: media.OpRemove, Name: "c.wav"}}); err != nil {
		t.Fatal(err)
	}
	encodeAudio(t, e, media.Audio{}, nil)
	if m, _ := e.manifest(t); m.Downloads[media.AudioDownloadKey("c.wav")].Blob != "" {
		t.Fatal("removed file kept its download")
	}
}

func TestAudioLoudness(t *testing.T) {
	quiet := media.Audio{Loudness: -16}
	e, _ := newAudioEnv(t, quiet, nil)
	e.commitFile(t, audioFixture{ext: "flac", codec: "flac", secs: 8, rate: 44100, volume: -30}.make(t), "quiet.flac", "audio/flac", media.OpInsert)
	encodeAudio(t, e, media.Audio{}, nil)
	m, _ := e.manifest(t)
	plain := m.Files[0].Variants[media.AudioVariant]
	if l := loudness(t, e.blob(t, plain.Blob)); l > -30 {
		t.Fatalf("unnormalized loudness %.1f LUFS", l)
	}

	// Another target is another spec: the file is re-encoded.
	encodeAudio(t, e, quiet, nil)
	m, _ = e.manifest(t)
	f := m.Files[0]
	v := f.Variants[media.AudioVariant]
	if v.Blob == plain.Blob || v.Spec != video.AudioSpec(quiet) || f.HLS.Spec != v.Spec {
		t.Fatalf("not re-encoded for loudness: %+v", f)
	}
	for _, blob := range []string{v.Blob, f.HLS.Audio[0].Blob} {
		if l := loudness(t, e.blob(t, blob)); math.Abs(l-(-16)) > 1 {
			t.Fatalf("normalized loudness %.1f LUFS, want -16", l)
		}
	}
}

// lavfiAudio writes a FLAC from a lavfi audio graph.
func lavfiAudio(t *testing.T, graph string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "source.flac")
	if b, err := exec.Command("ffmpeg", "-v", "error", "-nostdin", "-f", "lavfi", "-i", graph, "-c:a", "flac", "-y", out).CombinedOutput(); err != nil {
		t.Fatalf("fixture %s: %v: %s", graph, err, b)
	}
	return out
}

// loudnessOf is the integrated loudness of [from, from+secs) of a file.
func loudnessOf(t *testing.T, path string, from, secs float64) float64 {
	t.Helper()
	cut := filepath.Join(t.TempDir(), "cut.flac")
	if b, err := exec.Command("ffmpeg", "-v", "error", "-nostdin", "-ss", fmt.Sprint(from), "-t", fmt.Sprint(secs), "-i", path,
		"-c:a", "flac", "-y", cut).CombinedOutput(); err != nil {
		t.Fatalf("cut: %v: %s", err, b)
	}
	return loudness(t, cut)
}

// Normalization is one linear gain: a source with a wide loudness range
// (where loudnorm's second pass turns dynamic) keeps its quiet/loud
// difference, and 5.1 is measured as downmixed to stereo.
func TestAudioLoudnessIsLinear(t *testing.T) {
	a := media.Audio{Loudness: -16}
	e, _ := newAudioEnv(t, a, nil)
	wide := lavfiAudio(t, "aevalsrc=exprs='if(lt(t\\,4)\\,0.005\\,0.3)*sin(2*PI*440*t)':s=48000:d=8")
	surround := lavfiAudio(t, "aevalsrc=exprs='0.05*sin(2*PI*440*t)|0.05*sin(2*PI*440*t)|0.2*sin(2*PI*330*t)|0|0.05*sin(2*PI*550*t)|0.05*sin(2*PI*550*t)':c=5.1:s=48000:d=8")
	e.commitFile(t, wide, "wide.flac", "audio/flac", media.OpInsert)
	e.commitFile(t, surround, "surround.flac", "audio/flac", media.OpInsert)
	encodeAudio(t, e, a, nil)
	m, _ := e.manifest(t)

	out := e.blob(t, m.Files[m.File("wide.flac")].Variants[media.AudioVariant].Blob)
	inGap := loudnessOf(t, wide, 4, 4) - loudnessOf(t, wide, 0, 4)
	outGap := loudnessOf(t, out, 4, 4) - loudnessOf(t, out, 0, 4)
	if inGap < 25 || math.Abs(outGap-inGap) > 1 {
		t.Fatalf("quiet-to-loud gap %.1f LU became %.1f LU: not a linear gain", inGap, outGap)
	}
	if l := loudness(t, out); math.Abs(l-(-16)) > 1 {
		t.Fatalf("wide source normalized to %.1f LUFS", l)
	}

	sur := e.blob(t, m.Files[m.File("surround.flac")].Variants[media.AudioVariant].Blob)
	if l := loudness(t, sur); math.Abs(l-(-16)) > 1 {
		t.Fatalf("5.1 source normalized to %.1f LUFS in stereo, want -16", l)
	}
	sr := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=sample_rate,channels", "-of", "csv=p=0", sur)
	if b, err := sr.Output(); err != nil || strings.TrimSpace(string(b)) != "48000,2" {
		t.Fatalf("5.1 output %q %v", b, err)
	}
}

func TestAudioPlaybackThroughWorker(t *testing.T) {
	e, _ := newAudioEnv(t, media.Audio{}, nil)
	e.commitFile(t, audioFixture{ext: "mp3", codec: "libmp3lame", secs: 9, rate: 44100}.make(t), "source", "audio/mpeg", media.OpInsert)
	encodeAudio(t, e, media.Audio{}, nil)

	for _, mode := range []media.DeliveryMode{media.DeliverCookie, media.DeliverURL} {
		t.Run(string(mode), func(t *testing.T) {
			d := newDelivery(t, e, mode)
			m, h := d.manifest(t)
			a := h.Audio[0]

			var read media.ReadResult
			r := get(t, d.client, d.api.URL+"/media/video/"+cid(88)+"@v1?variant="+media.AudioVariant)
			if r.status != 200 {
				t.Fatalf("read: %d %s", r.status, r.body)
			}
			if err := json.Unmarshal(r.body, &read); err != nil || len(read.Files) != 1 || !read.Files[0].HLS ||
				read.Files[0].Variant != media.AudioVariant || math.Abs(read.Files[0].Duration-9) > 0.2 {
				t.Fatalf("read %s: %v", r.body, err)
			}
			v := m.Files[0].Variants[media.AudioVariant]
			if got := get(t, d.client, read.Files[0].URL, 0, v.Size); got.status != http.StatusPartialContent || !bytes.Equal(got.body, d.blob(t, v.Blob)) {
				t.Fatalf("m4a variant: %d", got.status)
			}

			masterURL := d.url("hls/source/master.m3u8")
			pl := parseMaster(t, d.playlist(t, masterURL, media.HLSContentType))
			if len(pl.media) != 0 || len(pl.variants) != 1 || pl.variants[0]["CODECS"] != "mp4a.40.2" ||
				pl.variants[0]["BANDWIDTH"] != strconv.Itoa(a.Bandwidth) || pl.variants[0]["URI"] != "audio/a1.m3u8" {
				t.Fatalf("audio master %+v", pl)
			}
			base, _ := url.Parse(masterURL)
			u, _ := url.Parse(pl.variants[0]["URI"])
			d.checkRendition(t, base.ResolveReference(u).String(), a.Blob, a.Segments, mode, true)
			if r := get(t, d.client, d.url("hls/source/video/360.m3u8")); r.status != 404 {
				t.Fatalf("video playlist of an audio file: %d", r.status)
			}

			key := media.AudioDownloadKey("source")
			r = get(t, d.client, d.url("download/"+key))
			if name := "Title (audio).mp4"; r.status != 200 || r.header.Get("Content-Disposition") != token.Attachment(name) ||
				int64(len(r.body)) != m.Downloads[key].Size {
				t.Fatalf("download %s: %d %v", key, r.status, r.header)
			}

			if mode == media.DeliverURL {
				out := filepath.Join(t.TempDir(), "played.m4a")
				args := []string{"-v", "error", "-nostdin"}
				if h, _ := exec.Command("ffmpeg", "-h", "demuxer=hls").Output(); bytes.Contains(h, []byte("allowed_segment_extensions")) {
					args = append(args, "-allowed_segment_extensions", "ALL", "-extension_picky", "0")
				}
				if b, err := exec.Command("ffmpeg", append(args, "-i", masterURL, "-c", "copy", "-y", out)...).CombinedOutput(); err != nil {
					t.Fatalf("ffmpeg: %v: %s", err, b)
				}
				if p := ffprobe(t, out); p.count("audio") != 1 || math.Abs(p.duration(t)-9) > 0.5 {
					t.Fatalf("played %+v", p)
				}
			}
		})
	}
}

// TestAudioOnlyKindWorker runs an audio-only kind's upload through the
// host queue and the River worker.
func TestAudioOnlyKindWorker(t *testing.T) {
	requireFFmpeg(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	pool := pgtest.Pool(t, nil)
	if err := workqueue.Migrate(ctx, pool, testSchema); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "DELETE FROM "+testSchema+".river_job"); err != nil {
		t.Fatal(err)
	}
	s3 := s3test.Open(t)
	kinds, err := media.NewRegistry(media.Kind{Name: "gallery_audio", Versioned: true, Types: audioTypes, Audio: &media.Audio{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := media.NewRegistry(media.Kind{Name: "raw", Types: audioTypes}); err == nil {
		t.Fatal("a kind taking audio without Audio registered")
	}
	locker := s3test.Locker(t, s3.Store)
	manifests, err := media.NewManifests(s3.Store, kinds, media.ManifestOptions{Locker: locker})
	if err != nil {
		t.Fatal(err)
	}
	enq, err := workqueue.New(pool, kinds, testSchema)
	if err != nil {
		t.Fatal(err)
	}
	uploads, err := media.NewUploads(media.UploadOptions{Store: s3.Store, Kinds: kinds, Manifests: manifests, Authorizer: grants{}, Queue: enq})
	if err != nil {
		t.Fatal(err)
	}
	enc, err := video.New(video.Config{Store: s3.Store, Locker: locker, TempDir: t.TempDir(), Threads: 2, Encoder: video.EncoderCPU})
	if err != nil {
		t.Fatal(err)
	}
	e := &env{Env: s3, store: s3.Store, kinds: kinds, manifests: manifests, uploads: uploads, encoder: enc,
		ref: contentref.NewVersion(s3.Tenant, "gallery_audio", contentref.NewID(), contentref.NewID())}

	wc := video.WorkerConfig{Encoder: enc, Pool: pool, Schema: testSchema, Kinds: kinds, Timeout: time.Hour}
	contribution, err := video.Contribution(wc)
	if err != nil {
		t.Fatal(err)
	}
	cfg := video.ClientConfig(wc)
	cfg.FetchPollInterval = 100 * time.Millisecond
	worker, err := riverhelpers.New(ctx, pool, cfg, contribution)
	if err != nil {
		t.Fatal(err)
	}
	done, stop := worker.Subscribe(river.EventKindJobCompleted, river.EventKindJobFailed, river.EventKindJobCancelled)
	defer stop()
	if err := worker.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stopCtx, c := context.WithTimeout(context.Background(), 30*time.Second)
		defer c()
		_ = worker.StopAndCancel(stopCtx)
	}()

	source := e.commitFile(t, audioFixture{ext: "ogg", codec: "libvorbis", secs: 5, rate: 48000}.make(t), "bgm.ogg", "audio/ogg", media.OpInsert)
	for {
		select {
		case ev := <-done:
			if ev.Kind != river.EventKindJobCompleted {
				t.Fatalf("job %s: %+v", ev.Kind, ev.Job.Errors)
			}
			if ev.Job.Kind != (workqueue.AudioArgs{}).Kind() {
				t.Fatalf("an audio-only kind ran a %s job", ev.Job.Kind)
			}
			if ev.Job.Queue != workqueue.AudioQueue {
				t.Fatalf("audio job on queue %s", ev.Job.Queue)
			}
		case <-ctx.Done():
			t.Fatal("audio job did not complete")
		}
		break
	}
	m, _ := e.manifest(t)
	f := m.Files[0]
	if f.HLS == nil || f.HLS.Source != source || len(f.HLS.Audio) != 1 || f.Variants[media.AudioVariant].Blob == "" {
		t.Fatalf("manifest after worker: %+v", m)
	}
	if r, err := manifests.Readiness(ctx, e.ref); err != nil || !r.Ready() {
		t.Fatalf("readiness %+v %v", r, err)
	}
}
