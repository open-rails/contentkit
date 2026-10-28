package mediastore

import (
	"context"
	"encoding/json"
	"errors"
	"maps"

	media "github.com/open-rails/contentkit/media"
)

// PageAnalyzer examines original bytes, before any edit or rendition. A failed
// analysis is evidence to keep/flag a page, never an upload failure.
// Only cancellation is returned as an error so workers can retry interrupted work.
type PageAnalyzer interface {
	AnalyzePage(context.Context, []byte, string) (media.PageAnalysis, error)
}

const pageAnalysisBytes = 1024

// IsPageUpload restricts analysis to the original upload pattern of a
// versioned reader kind. Covers, avatars and derived images are never pages.
func (k *RegisteredKind) IsPageUpload(f media.File) bool {
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
	return metaBytes(out)+pageAnalysisBytes <= media.MaxMetaBytes
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
func WithPageAnalysis(meta map[string]any, a media.PageAnalysis) (map[string]any, error) {
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
	if metaBytes(out) > media.MaxMetaBytes {
		return nil, errors.New("media: page analysis exceeds upload metadata limit")
	}
	return out, nil
}
