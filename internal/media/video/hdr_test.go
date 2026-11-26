package video

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-rails/contentkit/media"
)

// The command lines of an SDR source as they were before HDR support:
// identical arguments over identical input make identical bytes.
var sdrGolden = []string{
	"-v\x00error\x00-nostdin\x00-y\x00-threads\x004\x00-protocol_whitelist\x00http,https,tcp,tls,crypto\x00-format_whitelist\x00mov,matroska,avi,mpegts,flv,ogg,asf,mpeg\x00-ss\x008.000\x00-t\x008.000\x00-i\x00https://s/x\x00-filter_complex_threads\x004\x00-filter_complex\x00[0:0]fps=60,scale=1920:1080:flags=lanczos,setsar=1,split=2[v0][v1]\x00-map\x00[v0]\x00-c:v:0\x00libsvtav1\x00-maxrate:v:0\x007200k\x00-bufsize:v:0\x0014400k\x00-preset:v:0\x008\x00-crf:v:0\x0032\x00-svtav1-params:v:0\x00tune=0:mbr=7200:lp=3\x00-pix_fmt\x00yuv420p\x00-force_key_frames\x00expr:gte(t,n_forced*4)\x00-fflags\x00+bitexact\x00-flags\x00+bitexact\x00-muxdelay\x000\x00-muxpreload\x000\x00-f\x00hls\x00-hls_time\x004\x00-hls_playlist_type\x00vod\x00-hls_segment_type\x00fmp4\x00-hls_flags\x00single_file\x00-hls_segment_filename\x00/d/v1080-av1.mp4\x00/d/v1080-av1.m3u8\x00-map\x00[v1]\x00-c:v:0\x00libx264\x00-maxrate:v:0\x0012000k\x00-bufsize:v:0\x0024000k\x00-profile:v:0\x00high\x00-preset:v:0\x00fast\x00-crf:v:0\x0023\x00-sc_threshold:v:0\x000\x00-threads:v:0\x004\x00-pix_fmt\x00yuv420p\x00-force_key_frames\x00expr:gte(t,n_forced*4)\x00-fflags\x00+bitexact\x00-flags\x00+bitexact\x00-muxdelay\x000\x00-muxpreload\x000\x00-f\x00hls\x00-hls_time\x004\x00-hls_playlist_type\x00vod\x00-hls_segment_type\x00fmp4\x00-hls_flags\x00single_file\x00-hls_segment_filename\x00/d/v1080-h264.mp4\x00/d/v1080-h264.m3u8",
	"-v\x00error\x00-nostdin\x00-y\x00-threads\x004\x00-protocol_whitelist\x00http,https,tcp,tls,crypto\x00-format_whitelist\x00mov,matroska,avi,mpegts,flv,ogg,asf,mpeg\x00-i\x00https://s/x\x00-map\x000:1\x00-c:a\x00aac\x00-b:a\x00128k\x00-ar\x0048000\x00-ac\x002\x00-fflags\x00+bitexact\x00-flags\x00+bitexact\x00-muxdelay\x000\x00-muxpreload\x000\x00-f\x00hls\x00-hls_time\x004\x00-hls_playlist_type\x00vod\x00-hls_segment_type\x00fmp4\x00-hls_flags\x00single_file\x00-hls_segment_filename\x00/d/a0.mp4\x00/d/a0.m3u8\x00-map\x000:2\x00-c:s\x00webvtt\x00-f\x00webvtt\x00-y\x00/d/s0.vtt",
	"-v\x00error\x00-nostdin\x00-y\x00-threads\x004\x00-protocol_whitelist\x00file\x00-format_whitelist\x00mov,matroska,avi,mpegts,flv,ogg,asf,mpeg\x00-i\x00/d/v1080-h264.mp4\x00-filter_complex_threads\x004\x00-filter_complex\x00[0:0]fps=60,scale=1920:1080:flags=lanczos,setsar=1[sp];[sp]fps=1/0.200000000,scale=160:90:flags=lanczos,setsar=1[sprite]\x00-map\x00[sprite]\x00-frames:v\x00100\x00-f\x00image2\x00/d/t%03d.png",
	"-v\x00error\x00-nostdin\x00-y\x00-threads\x004\x00-protocol_whitelist\x00http,https,tcp,tls,crypto\x00-format_whitelist\x00mov,matroska,avi,mpegts,flv,ogg,asf,mpeg\x00-i\x00https://s/x\x00-filter_complex_threads\x004\x00-filter_complex\x00[0:0]fps=60,scale=3840:2160:flags=lanczos,setsar=1,split=2[v0][v1]\x00-map\x00[v0]\x00-c:v:0\x00av1_nvenc\x00-maxrate:v:0\x0019200k\x00-bufsize:v:0\x0038400k\x00-preset:v:0\x00p5\x00-tune:v:0\x00hq\x00-rc:v:0\x00vbr\x00-cq:v:0\x0040\x00-b:v:0\x000\x00-spatial-aq:v:0\x001\x00-temporal-aq:v:0\x001\x00-rc-lookahead:v:0\x0020\x00-forced-idr:v:0\x001\x00-no-scenecut:v:0\x001\x00-pix_fmt\x00yuv420p\x00-force_key_frames\x00expr:gte(t,n_forced*4)\x00-fflags\x00+bitexact\x00-flags\x00+bitexact\x00-muxdelay\x000\x00-muxpreload\x000\x00-f\x00hls\x00-hls_time\x004\x00-hls_playlist_type\x00vod\x00-hls_segment_type\x00fmp4\x00-hls_flags\x00single_file\x00-hls_segment_filename\x00/d/v2160-av1.mp4\x00/d/v2160-av1.m3u8\x00-map\x00[v1]\x00-c:v:0\x00h264_nvenc\x00-maxrate:v:0\x0032000k\x00-bufsize:v:0\x0064000k\x00-profile:v:0\x00high\x00-level:v:0\x005.1\x00-preset:v:0\x00p5\x00-tune:v:0\x00hq\x00-rc:v:0\x00vbr\x00-cq:v:0\x0027\x00-b:v:0\x000\x00-spatial-aq:v:0\x001\x00-temporal-aq:v:0\x001\x00-rc-lookahead:v:0\x0020\x00-forced-idr:v:0\x001\x00-no-scenecut:v:0\x001\x00-bf:v:0\x003\x00-b_ref_mode:v:0\x00middle\x00-pix_fmt\x00yuv420p\x00-force_key_frames\x00expr:gte(t,n_forced*4)\x00-fflags\x00+bitexact\x00-flags\x00+bitexact\x00-muxdelay\x000\x00-muxpreload\x000\x00-f\x00hls\x00-hls_time\x004\x00-hls_playlist_type\x00vod\x00-hls_segment_type\x00fmp4\x00-hls_flags\x00single_file\x00-hls_segment_filename\x00/d/v2160-h264.mp4\x00/d/v2160-h264.m3u8",
	"-v\x00error\x00-nostdin\x00-threads\x004\x00-protocol_whitelist\x00file\x00-format_whitelist\x00mov\x00-ss\x000.500000\x00-i\x00/d/frame.mp4\x00-map\x000:v:0\x00-vf\x00fps=10000:round=up:eof_action=pass,select='gte(t,0.999950)',scale=256:144:flags=lanczos,setsar=1\x00-frames:v\x001\x00-c:v\x00png\x00-f\x00image2\x00-y\x00/d/frame.png",
	"-v\x00error\x00-nostdin\x00-threads\x004\x00-protocol_whitelist\x00file\x00-format_whitelist\x00mov\x00-ss\x000.500000\x00-i\x00/d/frame.mp4\x00-map\x000:v:0\x00-vf\x00fps=10000:round=up:eof_action=pass,select='gte(t,0.999950)',scale=64:36,format=gray\x00-frames:v\x001\x00-f\x00rawvideo\x00pipe:1",
}

const sdrProbe = `{"streams":[
 {"index":0,"codec_type":"video","codec_name":"h264","profile":"High","width":1920,"height":1080,"sample_aspect_ratio":"1:1","pix_fmt":"yuv420p","field_order":"progressive","avg_frame_rate":"120/1","r_frame_rate":"120/1","color_range":"tv","color_space":"bt709","color_transfer":"bt709","color_primaries":"bt709"},
 {"index":1,"codec_type":"audio","codec_name":"aac","tags":{"language":"eng"},"disposition":{"default":1}},
 {"index":2,"codec_type":"subtitle","codec_name":"subrip","tags":{"language":"jpn"}}],
 "format":{"duration":"20.000","start_time":"0.000","format_name":"mov,mp4,m4a,3gp,3g2,mj2"}}`

func TestSDRCommandLinesUnchanged(t *testing.T) {
	var pr probeResult
	must(t, json.Unmarshal([]byte(sdrProbe), &pr))
	pl, err := newPlan(pr)
	must(t, err)
	enc := encoding{encoders: map[media.Codec]string{media.CodecAV1: "libsvtav1", media.CodecH264: "libx264"}, threads: 4, preset: "fast", topPreset: "fast"}
	r := rung{n: 1080, w: 1920, h: 1080}
	sprite := pl
	sprite.video, sprite.audio, sprite.subs = 0, nil, nil
	sprite.color, _ = sprite.color.output(media.CodecH264)
	nvenc := enc
	nvenc.encoders = map[media.Codec]string{media.CodecAV1: "av1_nvenc", media.CodecH264: "h264_nvenc"}
	in := frameInput{path: "/d/frame.mp4", opts: ownMP4, offset: 1.5}
	got := [][]string{
		ladderArgs("https://s/x", "/d", pl, pass{rung: r, codecs: []media.Codec{media.CodecAV1, media.CodecH264}, noTracks: true, start: 8, duration: 8, enc: enc}),
		ladderArgs("https://s/x", "/d", pl, pass{enc: encoding{threads: 4}}),
		ladderArgs("/d/v1080-h264.mp4", "/d", sprite, pass{rung: r, sprite: true, noTracks: true, enc: encoding{threads: 4}}),
		ladderArgs("https://s/x", "/d", pl, pass{rung: rung{n: 2160, w: 3840, h: 2160, level: "5.1"}, codecs: []media.Codec{media.CodecAV1, media.CodecH264}, noTracks: true, enc: nvenc}),
		append(in.args(4, in.sdr(fmt.Sprintf("scale=%d:%d:flags=lanczos,setsar=1", 256, 144))), "-c:v", "png", "-f", "image2", "-y", "/d/frame.png"),
		append(in.args(4, in.sdr("scale=64:36")+",format=gray"), "-f", "rawvideo", "pipe:1"),
	}
	for i, args := range got {
		if s := strings.Join(args, "\x00"); s != sdrGolden[i] {
			t.Errorf("command %d changed:\n got %q\nwant %q", i, s, sdrGolden[i])
		}
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// sample writes a short synthetic 10-bit HEVC source: testsrc2 converted to
// transfer trc in BT.2020, tagged, with x265's HDR10 SEI when PQ.
func sample(t *testing.T, dir, trc string) string {
	t.Helper()
	out := filepath.Join(dir, trc+".mp4")
	x265 := "log-level=error:pools=1:frame-threads=1:repeat-headers=1:colorprim=bt2020:colormatrix=bt2020nc:range=limited:transfer=" + trc
	if trc == "smpte2084" {
		x265 += ":hdr10=1:master-display=G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,50):max-cll=1000,400"
	}
	ffmpeg(t, "-f", "lavfi", "-i", "testsrc2=s=256x144:r=12:d=1",
		"-vf", "format=gbrpf32le,zscale=tin=bt709:pin=bt709:min=gbr:rin=pc:t="+trc+":p=bt2020:m=bt2020nc:r=tv:npl=1000,format=yuv420p10le",
		"-c:v", "libx265", "-preset", "ultrafast", "-x265-params", x265, "-tag:v", "hvc1", out)
	return out
}

func ffmpeg(t *testing.T, args ...string) {
	t.Helper()
	out, err := exec.Command("ffmpeg", append([]string{"-nostdin", "-v", "error", "-y", "-threads", "1"}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg %v: %v\n%s", args, err, out)
	}
}

// sourcePlan probes src as the planner does.
func sourcePlan(t *testing.T, src string) plan {
	t.Helper()
	pr, err := probe(context.Background(), src)
	must(t, err)
	pl, err := newPlan(pr)
	must(t, err)
	if pl.color.hdr() {
		pr.Frame, err = firstFrame(context.Background(), src, pl.video, inputOptions(sourceDemuxers))
		must(t, err)
		pl, err = newPlan(pr)
		must(t, err)
	}
	return pl
}

func TestProbeDynamicRange(t *testing.T) {
	dir := t.TempDir()
	pq, hlg := sourcePlan(t, sample(t, dir, "smpte2084")), sourcePlan(t, sample(t, dir, "arib-std-b67"))
	sdr := filepath.Join(dir, "sdr.mp4")
	ffmpeg(t, "-f", "lavfi", "-i", "testsrc2=s=256x144:r=12:d=1", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", sdr)
	if r := sourcePlan(t, sdr).color.dynamicRange(); r != media.RangeSDR {
		t.Fatalf("SDR source: %q", r)
	}
	if hlg.color.dynamicRange() != media.RangeHLG || hlg.color.mastering != nil || hlg.color.light != nil {
		t.Fatalf("HLG source: %+v", hlg.color)
	}
	c := pq.color
	if c.dynamicRange() != media.RangePQ || c.primaries != "bt2020" || c.matrix != "bt2020nc" || c.light == nil || *c.light != (light{1000, 400}) || c.mastering == nil {
		t.Fatalf("PQ source: %+v", c)
	}
	if m := *c.mastering; m.max != 1000 || m.min != 0.005 || m.r != [2]float64{0.68, 0.32} || m.white != [2]float64{0.3127, 0.329} {
		t.Fatalf("PQ mastering display: %+v", m)
	}
	if got := c.svtParams(); got != ":enable-hdr=1:mastering-display=G(0.2650,0.6900)B(0.1500,0.0600)R(0.6800,0.3200)WP(0.3127,0.3290)L(1000.0000,0.0050):content-light=1000,400" {
		t.Fatalf("SVT-AV1 HDR10 parameters: %s", got)
	}
}

type probed struct {
	Streams []struct {
		Codec     string `json:"codec_name"`
		PixFmt    string `json:"pix_fmt"`
		Transfer  string `json:"color_transfer"`
		Primaries string `json:"color_primaries"`
		Matrix    string `json:"color_space"`
	} `json:"streams"`
}

func probeFile(t *testing.T, path string) probed {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0", "-show_streams", "-of", "json", path).Output()
	must(t, err)
	var p probed
	must(t, json.Unmarshal(out, &p))
	if len(p.Streams) != 1 {
		t.Fatalf("%s: %+v", path, p)
	}
	return p
}

// bitstreamMetadata is the HDR10 side data dav1d reads from an AV1
// rendition's own OBUs, its container's boxes left behind.
func bitstreamMetadata(t *testing.T, path string) string {
	t.Helper()
	obu := strings.TrimSuffix(path, ".mp4") + ".obu"
	ffmpeg(t, "-i", path, "-c", "copy", "-f", "obu", obu)
	out, err := exec.Command("ffprobe", "-v", "error", "-read_intervals", "%+#1", "-show_frames", "-show_entries", "frame=side_data_list", "-of", "compact", obu).Output()
	must(t, err)
	return string(out)
}

// luma is the mean and deviation of a picture's luma on a 64×36 thumbnail.
func luma(t *testing.T, path string) (float64, float64) {
	t.Helper()
	out, err := exec.Command("ffmpeg", "-v", "error", "-i", path, "-frames:v", "1", "-vf", "scale=64:36,format=gray", "-f", "rawvideo", "pipe:1").Output()
	must(t, err)
	var sum, sq float64
	for _, v := range out {
		sum += float64(v)
		sq += float64(v) * float64(v)
	}
	n := float64(len(out))
	return sum / n, math.Sqrt(sq/n - sum*sum/n/n)
}

func TestHDRLadder(t *testing.T) {
	dir := t.TempDir()
	ref := filepath.Join(dir, "ref.png")
	ffmpeg(t, "-f", "lavfi", "-i", "testsrc2=s=256x144:r=12:d=1", "-frames:v", "1", "-update", "1", ref)
	refMean, refDev := luma(t, ref)
	refSprite := filepath.Join(dir, "sprite.png")
	ffmpeg(t, "-f", "lavfi", "-i", "testsrc2=s=256x144:r=12:d=1", "-vf", "fps=100,scale=160:90:flags=lanczos,tile=10x10", "-frames:v", "1", "-update", "1", refSprite)
	spriteMean, spriteDev := luma(t, refSprite)
	enc := encoding{encoders: map[media.Codec]string{media.CodecAV1: "libsvtav1", media.CodecH264: "libx264"}, threads: 1, preset: "ultrafast", topPreset: "ultrafast"}
	for _, tc := range []struct {
		trc   string
		codec string
	}{{"smpte2084", ".09.16.09.0"}, {"arib-std-b67", ".09.18.09.0"}} {
		t.Run(tc.trc, func(t *testing.T) {
			src := sample(t, dir, tc.trc)
			pl := sourcePlan(t, src)
			out := filepath.Join(dir, tc.trc)
			must(t, os.Mkdir(out, 0o755))
			r := rung{n: 144, w: 256, h: 144}
			must(t, ladder(context.Background(), src, out, pl, pass{rung: r, codecs: []media.Codec{media.CodecAV1, media.CodecH264}, sprite: true, noTracks: true, enc: enc}, nil))
			av1, h264 := filepath.Join(out, "v144-av1.mp4"), filepath.Join(out, "v144-h264.mp4")
			if s := probeFile(t, av1).Streams[0]; s.PixFmt != "yuv420p10le" || s.Transfer != tc.trc || s.Primaries != "bt2020" || s.Matrix != "bt2020nc" {
				t.Fatalf("AV1 rendition: %+v", s)
			}
			if s := probeFile(t, h264).Streams[0]; s.PixFmt != "yuv420p" || s.Transfer != "bt709" || s.Primaries != "bt709" || s.Matrix != "bt709" {
				t.Fatalf("H.264 rendition: %+v", s)
			}
			codecs, err := codecString(av1)
			must(t, err)
			if !strings.HasPrefix(codecs, "av01.0.") || !strings.Contains(codecs, "M.10.0.11") || !strings.HasSuffix(codecs, tc.codec) {
				t.Fatalf("AV1 codecs %q", codecs)
			}
			meta := bitstreamMetadata(t, av1)
			if pq := tc.trc == "smpte2084"; pq != strings.Contains(meta, "max_content=1000|") || pq != strings.Contains(meta, "max_luminance=256000/256") ||
				!pq && strings.Contains(meta, "side_data_type") {
				t.Fatalf("AV1 bitstream HDR10 metadata: %s", meta)
			}
			// Tone mapped, the H.264 frame keeps the SDR picture's contrast:
			// PQ code values shown as BT.709 halve it.
			mean, dev := luma(t, h264)
			if math.Abs(mean-refMean) > 25 || dev < 0.8*refDev {
				t.Fatalf("H.264 luma %.1f±%.1f, SDR picture %.1f±%.1f", mean, dev, refMean, refDev)
			}
			if mean, dev := luma(t, filepath.Join(out, "sprite.jpg")); math.Abs(mean-spriteMean) > 25 || dev < 0.8*spriteDev {
				t.Fatalf("sprite luma %.1f±%.1f, SDR sprite %.1f±%.1f", mean, dev, spriteMean, spriteDev)
			}
			// A frame cut from the AV1 rendition, as a poster is without H.264.
			png := filepath.Join(out, "frame.png")
			must(t, grabFrame(context.Background(), frameInput{path: av1, opts: ownMP4, offset: 0, toSDR: toneMap}, 256, 144, png, 1))
			if mean, dev := luma(t, png); math.Abs(mean-refMean) > 25 || dev < 0.8*refDev {
				t.Fatalf("poster luma %.1f±%.1f", mean, dev)
			}
		})
	}
}
