package media

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"

	"github.com/open-rails/contentkit/media/layout"
)

// PageAnalyzer examines original bytes, before any edit or rendition. A failed
// analysis is evidence to keep/flag a page, never an upload failure.
// Only cancellation is returned as an error so workers can retry interrupted work.
type PageAnalyzer interface {
	AnalyzePage(context.Context, []byte, string) (PageAnalysis, error)
}

// PageAnalysisRecipe versions the evidence independently of image renditions.
const PageAnalysisRecipe = "page-analysis-v1"

const pageAnalysisBytes = 1024

// PageAnalysis is immutable evidence, not the reader's include/exclude decision.
// The planner owns duplicate keepers, ordering and manual overrides.
type PageAnalysis struct {
	Recipe        string              `json:"recipe"`
	SourceSHA256  string              `json:"source_sha256"`
	Status        string              `json:"status"` // complete | unsupported | failed
	Width         int                 `json:"width,omitempty"`
	Height        int                 `json:"height,omitempty"`
	Frames        int                 `json:"frames,omitempty"`
	PixelSHA256   string              `json:"pixel_sha256,omitempty"`
	PixelFormat   string              `json:"pixel_format,omitempty"`
	ProfileSHA256 string              `json:"profile_sha256,omitempty"`
	Blank         string              `json:"blank"` // none | blank | suspected | unknown
	BlankReason   string              `json:"blank_reason,omitempty"`
	Confidence    string              `json:"confidence"` // exact | high | possible | unknown
	Metrics       PageAnalysisMetrics `json:"metrics,omitzero"`
	Flags         []string            `json:"flags,omitempty"`
}

type PageAnalysisMetrics struct {
	Pixels           uint64   `json:"pixels"`
	VisiblePixels    uint64   `json:"visible_pixels"`
	OpaquePixels     uint64   `json:"opaque_pixels"`
	ForegroundPixels uint64   `json:"foreground_pixels"`
	MinRGB           [3]uint8 `json:"min_rgb"`
	MaxRGB           [3]uint8 `json:"max_rgb"`
}

// IsPageUpload restricts analysis to the original upload pattern of a
// versioned reader kind. Covers, avatars and derived images are never pages.
func (k *RegisteredKind) IsPageUpload(f File) bool {
	if k.Publish == nil || !f.IsUpload() {
		return false
	}
	p, ok := parsePattern(k.Publish.Pages)
	stem, _ := splitExt(f.Path)
	_, matched := p.match(stem)
	return ok && matched
}

func pageMetadata(meta map[string]any) (map[string]any, error) {
	value, exists := meta["page"]
	if !exists || value == nil {
		return map[string]any{}, nil
	}
	if page, ok := value.(map[string]any); ok {
		return maps.Clone(page), nil
	}
	b, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var page map[string]any
	if err := json.Unmarshal(b, &page); err != nil || page == nil {
		return nil, errors.New("media: page metadata must be an object")
	}
	return page, nil
}

// ReadPageAnalysis accepts both newly typed values and JSON-decoded metadata.
func ReadPageAnalysis(meta map[string]any) (PageAnalysis, bool) {
	page, err := pageMetadata(meta)
	if err != nil || page["analysis"] == nil {
		return PageAnalysis{}, false
	}
	if a, ok := page["analysis"].(PageAnalysis); ok {
		return a, true
	}
	b, err := json.Marshal(page["analysis"])
	if err != nil {
		return PageAnalysis{}, false
	}
	var a PageAnalysis
	if json.Unmarshal(b, &a) != nil {
		return PageAnalysis{}, false
	}
	return a, true
}

func (f File) HasPageAnalysis(recipe string) bool {
	a, ok := ReadPageAnalysis(f.Meta)
	digest, valid := layout.BlobDigest(f.Blob)
	return ok && valid && a.Recipe == recipe && a.SourceSHA256 == hex.EncodeToString(digest) &&
		(a.Status == "complete" || a.Status == "unsupported" || a.Status == "failed")
}

// PageAnalysisFits reserves a bounded analysis record without displacing user
// metadata. A full or malformed metadata object simply cannot receive analysis.
func PageAnalysisFits(meta map[string]any) bool {
	page, err := pageMetadata(meta)
	if err != nil {
		return false
	}
	delete(page, "analysis")
	out := maps.Clone(meta)
	if out == nil {
		out = map[string]any{}
	}
	out["page"] = page
	return metaBytes(out)+pageAnalysisBytes <= MaxMetaBytes
}

// PageAnalysisCapacity reserves evidence only after existing rendition and
// failure records. Optional analysis must never make an item unpublishable.
func (k *RegisteredKind) PageAnalysisCapacity(m *Manifest) int {
	if m.Full {
		return 0
	}
	return int(max(0, int64(editLimit)-m.size-k.Unwritten(m)) / pageAnalysisBytes)
}

// WithPageAnalysis replaces only page.analysis. Manual choices and unknown
// fields survive reruns and schema evolution; the caller's maps are not mutated.
func WithPageAnalysis(meta map[string]any, a PageAnalysis) (map[string]any, error) {
	b, err := json.Marshal(a)
	if err != nil || len(b)+32 > pageAnalysisBytes {
		return nil, errors.New("media: page analysis exceeds its reserved space")
	}
	page, err := pageMetadata(meta)
	if err != nil {
		return nil, err
	}
	page["analysis"] = a
	out := maps.Clone(meta)
	if out == nil {
		out = map[string]any{}
	}
	out["page"] = page
	if metaBytes(out) > MaxMetaBytes {
		return nil, errors.New("media: page analysis exceeds upload metadata limit")
	}
	return out, nil
}
