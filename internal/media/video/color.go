package video

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/open-rails/contentkit/media"
)

// HDR: a PQ or HLG source keeps its range in AV1 (10-bit, its transfer,
// primaries and HDR10 static metadata); its H.264 renditions, sprite and
// frames are tone-mapped to BT.709 SDR. An SDR source is encoded as before.

// color is a video's color description, ffmpeg's names.
type color struct {
	transfer, primaries, matrix, rng string
	mastering                        *mastering
	light                            *light
}

// mastering is SMPTE ST 2086 mastering display color volume: CIE 1931
// chromaticities and luminance in cd/m².
type mastering struct {
	r, g, b, white [2]float64
	min, max       float64
}

// light is CTA-861.3 content light level: MaxCLL and MaxFALL in cd/m².
type light struct{ maxCLL, maxFALL int }

// probeVersion marks probes that carry the color fields and HDR frame data.
const probeVersion = 2

func (c color) dynamicRange() media.DynamicRange {
	switch c.transfer {
	case "smpte2084":
		return media.RangePQ
	case "arib-std-b67":
		return media.RangeHLG
	}
	return media.RangeSDR
}

func (c color) hdr() bool { return c.dynamicRange() != media.RangeSDR }

// tagged is an HDR source's description with what it leaves unstated
// taken as BT.2020 in limited range: what its AV1 renditions declare and
// tone mapping reads.
func (c color) tagged() color {
	if !slices.Contains([]string{"bt2020", "bt709", "smpte431", "smpte432"}, c.primaries) {
		c.primaries = "bt2020"
	}
	if !slices.Contains([]string{"bt2020nc", "bt2020c", "bt709"}, c.matrix) {
		c.matrix = "bt2020nc"
	}
	if c.rng != "pc" {
		c.rng = "tv"
	}
	return c
}

// sdr709 is an HDR source's tone-mapped renditions.
var sdr709 = color{transfer: "bt709", primaries: "bt709", matrix: "bt709", rng: "tv"}

// output is the color codec c's renditions of a source of color in carry,
// and whether they are tone-mapped. An SDR source's outputs keep what
// ffmpeg passes through (zero color: no options).
func (in color) output(c media.Codec) (color, bool) {
	switch {
	case !in.hdr():
		return color{}, false
	case c == media.CodecAV1:
		return in.tagged(), false
	}
	return sdr709, true
}

// rangeOf is the dynamic range codec c's renditions of the plan carry.
func (pl plan) rangeOf(c media.Codec) media.DynamicRange {
	out, _ := pl.color.output(c)
	return out.dynamicRange()
}

// setParams states an HDR input's color on its frames, so the filters
// after it read the description its AV1 renditions declare.
func (c color) setParams() string {
	t := c.tagged()
	return fmt.Sprintf("setparams=range=%s:color_primaries=%s:color_trc=%s:colorspace=%s", t.rng, t.primaries, t.transfer, t.matrix)
}

// toneMap converts a tagged HDR picture to BT.709 SDR: linear light with
// 100 cd/m² at 1.0, BT.709 primaries, the Hable curve (its peak the
// frames' MaxCLL or mastering peak, else 1000 cd/m²), BT.709 transfer and
// matrix in limited-range 8-bit 4:2:0.
const toneMap = "zscale=t=linear:npl=100,format=gbrpf32le,zscale=p=bt709,tonemap=tonemap=hable:desat=0,zscale=t=bt709:m=bt709:r=tv,format=yuv420p"

// toSDR is the filter that brings a picture of color c to SDR: none for SDR.
func (c color) toSDR() string {
	if !c.hdr() {
		return ""
	}
	return c.setParams() + "," + toneMap
}

// args are the encoder options an output of color c declares: its
// description and, in SVT-AV1, its HDR10 metadata (svtParams).
func (c color) args(o func(string) string) []string {
	if c.transfer == "" {
		return nil
	}
	return []string{o("color_primaries"), c.primaries, o("color_trc"), c.transfer, o("colorspace"), c.matrix, o("color_range"), c.rng}
}

// svtParams are SVT-AV1's HDR10 options: its mastering display and
// content light level, written as metadata OBUs.
func (c color) svtParams() string {
	if !c.hdr() {
		return ""
	}
	p := ":enable-hdr=1"
	if m := c.mastering; m != nil {
		p += fmt.Sprintf(":mastering-display=G(%.4f,%.4f)B(%.4f,%.4f)R(%.4f,%.4f)WP(%.4f,%.4f)L(%.4f,%.4f)",
			m.g[0], m.g[1], m.b[0], m.b[1], m.r[0], m.r[1], m.white[0], m.white[1], m.max, m.min)
	}
	if l := c.light; l != nil {
		p += fmt.Sprintf(":content-light=%d,%d", l.maxCLL, l.maxFALL)
	}
	return p
}

// pixFmt is an output's pixel format: 10-bit for HDR.
func (c color) pixFmt(encoder string) string {
	switch {
	case !c.hdr():
		return "yuv420p"
	case encoder == "av1_nvenc":
		return "p010le"
	}
	return "yuv420p10le"
}

// streamColor is video stream s's color, with its HDR10 metadata from the
// stream or else its first frame.
func streamColor(s probeStream, frame []sideData) color {
	c := color{transfer: known(s.Transfer), primaries: known(s.Primaries), matrix: known(s.Matrix), rng: known(s.Range)}
	if !c.hdr() {
		return c
	}
	for _, sd := range slices.Concat(s.SideData, frame) {
		switch sd.Type {
		case "Mastering display metadata":
			if m, ok := sd.mastering(); ok && c.mastering == nil {
				c.mastering = &m
			}
		case "Content light level metadata":
			if c.light == nil && (sd.MaxContent > 0 || sd.MaxAverage > 0) && sd.MaxContent <= 65535 && sd.MaxAverage <= 65535 {
				c.light = &light{sd.MaxContent, sd.MaxAverage}
			}
		}
	}
	return c
}

func known(v string) string {
	if v == "unknown" || v == "unspecified" || v == "reserved" {
		return ""
	}
	return v
}

// mastering parses ffprobe's mastering display side data; false when any
// value is absent or out of range.
func (sd sideData) mastering() (mastering, bool) {
	var m mastering
	ok := true
	q := func(v string, hi float64) float64 {
		n, d, found := strings.Cut(v, "/")
		a, err1 := strconv.ParseFloat(n, 64)
		b, err2 := strconv.ParseFloat(d, 64)
		if !found || err1 != nil || err2 != nil || b <= 0 || a < 0 || a/b > hi {
			ok = false
			return 0
		}
		return a / b
	}
	m.r = [2]float64{q(sd.RedX, 1), q(sd.RedY, 1)}
	m.g = [2]float64{q(sd.GreenX, 1), q(sd.GreenY, 1)}
	m.b = [2]float64{q(sd.BlueX, 1), q(sd.BlueY, 1)}
	m.white = [2]float64{q(sd.WhiteX, 1), q(sd.WhiteY, 1)}
	m.min, m.max = q(sd.MinLum, 10000), q(sd.MaxLum, 10000)
	return m, ok && m.max > 0 && m.min < m.max
}

// firstFrame is the side data of stream's first frame.
func firstFrame(ctx context.Context, src string, stream int, opts []string) ([]sideData, error) {
	out, err := command(ctx, "ffprobe", append(append([]string{"-v", "error"}, opts...), "-select_streams", strconv.Itoa(stream),
		"-read_intervals", "%+#1", "-show_frames", "-show_entries", "frame=side_data_list", "-of", "json", src)...)
	if err != nil {
		return nil, err
	}
	var p struct {
		Frames []struct {
			SideData []sideData `json:"side_data_list"`
		} `json:"frames"`
	}
	if err := json.Unmarshal(out, &p); err != nil {
		return nil, fmt.Errorf("ffprobe frames: %w", err)
	}
	var sd []sideData
	for _, f := range p.Frames {
		for _, d := range f.SideData {
			if d.Type == "Mastering display metadata" || d.Type == "Content light level metadata" {
				sd = append(sd, d)
			}
		}
	}
	return sd, nil
}
