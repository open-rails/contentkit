package media

import (
	"context"

	"github.com/open-rails/contentkit/contentref"
)

// Encode phases, in order. Queued covers a job waiting for a worker and a
// file waiting behind another file of the same job.
const (
	PhaseQueued      = "queued"
	PhaseDownloading = "downloading" // fetching the original
	PhaseProbing     = "probing"
	PhaseEncoding    = "encoding" // the single ffmpeg pass: ladder, audio, subtitles, sprite
	PhaseMuxing      = "muxing"   // per-quality MP4 downloads
	PhaseUploading   = "uploading"
	PhasePublishing  = "publishing" // the manifest edit
	PhaseImages      = "images"     // item-wide, after every file: poster frame and hover preview from the renditions
)

// EncodePhases lists every phase in order.
var EncodePhases = []string{PhaseQueued, PhaseDownloading, PhaseProbing, PhaseEncoding, PhaseMuxing, PhaseUploading, PhasePublishing, PhaseImages}

// EncodeProgress is a pending video file's live encode state. Segments
// count HLS segments of the source's duration; Speed is the encode's
// smoothed ×realtime; ETA is seconds from At to publish, including uploads.
// Percent never decreases within a run. Stalled marks a report older than a
// minute from a running job (a dead worker until River rescues the job).
type EncodeProgress struct {
	Phase         string  `json:"phase"`
	QueuePosition int     `json:"queue_position,omitempty"` // 1 = next; 0 = unknown or behind a file of the same job
	SegmentsDone  int     `json:"segments_done,omitempty"`
	SegmentsTotal int     `json:"segments_total,omitempty"`
	Percent       float64 `json:"percent"`
	Speed         float64 `json:"speed,omitempty"`
	ETA           float64 `json:"eta,omitempty"`
	At            int64   `json:"at"` // unix milliseconds of the measurement
	Stalled       bool    `json:"stalled,omitempty"`
}

// EncodeStatus is an item's encode progress: Files from the running job,
// Queued for pending files it does not cover (a waiting job), Item for the
// job's item-wide step (PhaseImages).
type EncodeStatus struct {
	Files  map[string]EncodeProgress
	Queued *EncodeProgress
	Item   *EncodeProgress
}

// ItemProgressKey carries Item in a job's reported map.
const ItemProgressKey = ""

// Current is the item's most advanced step: the item-wide one, a running
// file, else the wait.
func (s EncodeStatus) Current() *EncodeProgress {
	if s.Item != nil {
		return s.Item
	}
	var best *EncodeProgress
	for _, p := range s.Files {
		if p.Phase != PhaseQueued && (best == nil || p.Percent > best.Percent) {
			best = &p
		}
	}
	if best == nil {
		return s.Queued
	}
	return best
}

// ProgressSource reads encode progress (video.NewProgressSource). The read
// API asks only when a visible video file is pending.
type ProgressSource interface {
	EncodeProgress(ctx context.Context, ref contentref.ContentRef) (EncodeStatus, error)
}
