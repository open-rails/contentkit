package versions

// ReaderMeta is a version's reader metadata, kept in its item's manifest:
// scroll anchors on its pages and audio cues on pages or anchors. Pages use
// original-image hashes; audio files retain their upload names.
type ReaderMeta struct {
	SchemaVersion int             `json:"schema_version"` // 1
	Defaults      *ReaderDefaults `json:"defaults,omitempty"`
	Anchors       []Anchor        `json:"anchors,omitempty"`
	Background    []BackgroundCue `json:"background,omitempty"`
	Foreground    []ForegroundCue `json:"foreground,omitempty"`
}

type ReaderDefaults struct {
	AutoAdvanceSeconds float64 `json:"auto_advance_seconds,omitempty"`
}

// Anchor is a scroll position within a page of the vertical reader.
type Anchor struct {
	ID              string   `json:"id"`
	Page            string   `json:"page"`
	OffsetPercent   float64  `json:"offset_percent"`
	DurationSeconds *float64 `json:"duration_seconds,omitempty"`
}

// BackgroundCue plays a track over a range of pages or anchors.
type BackgroundCue struct {
	File       string  `json:"file"`
	FromPage   *string `json:"from_page,omitempty"`
	ToPage     *string `json:"to_page,omitempty"`
	FromAnchor *string `json:"from_anchor,omitempty"`
	ToAnchor   *string `json:"to_anchor,omitempty"`
	Loop       *bool   `json:"loop,omitempty"`
	Layer      *string `json:"layer,omitempty"`
}

// ForegroundCue plays a whole track at a page or anchor.
type ForegroundCue struct {
	File   string  `json:"file"`
	Page   *string `json:"page,omitempty"`
	Anchor *string `json:"anchor,omitempty"`
}
