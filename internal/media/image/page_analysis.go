package image

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	mediastore "github.com/open-rails/contentkit/internal/mediastore"

	"github.com/davidbyttow/govips/v2/vips"

	"github.com/open-rails/contentkit/media"
)

// Analysis is serialized independently of rendition workers. ToBytes only
// materializes a stripe, never a full decoded page in the Go heap.
var pageAnalysisSlot = make(chan struct{}, 1)

const analysisStripeBytes = 4 << 20

var _ mediastore.PageAnalyzer = (*Processor)(nil)

// AnalyzePage examines original pixels before edits, resizes or encoding. It
// uses the same trusted loaders and source limits as the rendition producer.
func (p *Processor) AnalyzePage(ctx context.Context, src []byte, contentType string) (media.PageAnalysis, error) {
	return analyzePage(ctx, src, contentType, p.rules(media.AnimationAllow))
}

func analyzePage(ctx context.Context, src []byte, contentType string, limits rules) (media.PageAnalysis, error) {
	sum := sha256.Sum256(src)
	a := media.PageAnalysis{Recipe: media.PageAnalysisRecipe, SourceSHA256: hex.EncodeToString(sum[:]),
		Status: "unsupported", Blank: "unknown", Confidence: "unknown"}
	select {
	case pageAnalysisSlot <- struct{}{}:
		defer func() { <-pageAnalysisSlot }()
	case <-ctx.Done():
		return a, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return a, err
	}
	fail := func(status, flag string) (media.PageAnalysis, error) {
		a.Status, a.Flags = status, []string{flag}
		return a, nil
	}
	if err := start(); err != nil {
		return fail("failed", "decoder_unavailable")
	}
	s, err := probe(src, contentType, limits)
	if err != nil {
		return fail("failed", "source_unreadable_or_over_limit")
	}
	a.Width, a.Height, a.Frames = s.w, s.h, s.frames
	if s.frames != 1 {
		return fail("unsupported", "animated_source")
	}
	img, err := open(src, contentType)
	if err != nil {
		return fail("failed", "decode_failed")
	}
	defer img.Close()
	if img.Pages() > 1 {
		return fail("unsupported", "multipage_source")
	}
	if img.BandFormat() != vips.BandFormatUchar ||
		(img.Interpretation() != vips.InterpretationBW && img.Interpretation() != vips.InterpretationSRGB) {
		return fail("unsupported", "pixel_format_unsupported")
	}
	if img.Bands() < 1 || img.Bands() > 4 {
		return fail("unsupported", "pixel_format_unsupported")
	}
	if profile := img.GetICCProfile(); len(profile) > 0 {
		digest := sha256.Sum256(profile)
		a.ProfileSHA256 = hex.EncodeToString(digest[:])
	}
	if img.Interpretation() == vips.InterpretationBW {
		if err := img.ToColorSpace(vips.InterpretationSRGB); err != nil {
			return fail("failed", "pixel_conversion_failed")
		}
	}
	if !img.HasAlpha() {
		if err := img.AddAlpha(); err != nil {
			return fail("failed", "pixel_conversion_failed")
		}
	}
	if img.Bands() != 4 || img.BandFormat() != vips.BandFormatUchar {
		return fail("unsupported", "pixel_format_unsupported")
	}
	rowBytes := img.Width() * 4
	if rowBytes <= 0 || rowBytes > analysisStripeBytes {
		return fail("unsupported", "analysis_row_limit")
	}
	a.PixelFormat = "upright-rgba8-v1"
	hash := sha256.New()
	// Profile identity is part of equality: identical samples with different
	// color profiles must not cause an automatic duplicate exclusion.
	fmt.Fprintf(hash, "%s\x00%d\x00%d\x00%s\x00", a.PixelFormat, img.Width(), img.Height(), a.ProfileSHA256)
	a.Metrics.MinRGB = [3]uint8{255, 255, 255}
	rows := min(256, analysisStripeBytes/rowBytes)
	for y := 0; y < img.Height(); y += rows {
		if err := ctx.Err(); err != nil {
			return a, err
		}
		stripe, err := img.Copy()
		if err != nil {
			return fail("failed", "pixel_read_failed")
		}
		height := min(rows, img.Height()-y)
		if err = stripe.ExtractArea(0, y, img.Width(), height); err != nil {
			stripe.Close()
			return fail("failed", "pixel_read_failed")
		}
		pixels, err := stripe.ToBytes()
		stripe.Close()
		if err != nil || len(pixels) != rowBytes*height {
			return fail("failed", "pixel_read_failed")
		}
		accumulatePagePixels(&a.Metrics, pixels)
		_, _ = hash.Write(pixels)
	}
	a.PixelSHA256 = hex.EncodeToString(hash.Sum(nil))
	a.Status, a.Blank, a.Confidence = "complete", "none", "high"
	classifyPageBlank(&a)
	return a, nil
}

// accumulatePagePixels also zeroes invisible RGB for canonical pixel equality.
func accumulatePagePixels(m *media.PageAnalysisMetrics, pixels []byte) {
	for i := 0; i < len(pixels); i += 4 {
		m.Pixels++
		if pixels[i+3] == 0 {
			pixels[i], pixels[i+1], pixels[i+2] = 0, 0, 0
			continue
		}
		m.VisiblePixels++
		if pixels[i+3] == 255 {
			m.OpaquePixels++
		}
		foreground := false
		for c := range 3 {
			m.MinRGB[c] = min(m.MinRGB[c], pixels[i+c])
			m.MaxRGB[c] = max(m.MaxRGB[c], pixels[i+c])
			foreground = foreground || pixels[i+c] < 248
		}
		if foreground {
			m.ForegroundPixels++
		}
	}
}

func classifyPageBlank(a *media.PageAnalysis) {
	m := a.Metrics
	if m.VisiblePixels == 0 {
		a.Blank, a.BlankReason, a.Confidence = "blank", "fully_transparent", "exact"
		return
	}
	uniform := m.MinRGB == m.MaxRGB && m.OpaquePixels == m.Pixels
	if uniform && m.MinRGB[0] >= 250 && m.MinRGB[1] >= 250 && m.MinRGB[2] >= 250 {
		a.Blank, a.BlankReason, a.Confidence = "blank", "uniform_white", "high"
		return
	}
	// A single visible mark, faint writing or compression noise prevents an
	// automatic blank exclusion. Very sparse pages and uniform dark pages are
	// only suggestions for review and remain in the reader by default.
	if uniform || m.ForegroundPixels*2000 <= m.Pixels {
		a.Blank, a.BlankReason, a.Confidence = "suspected", "low_information", "possible"
		a.Flags = []string{"possible_blank"}
	}
}
