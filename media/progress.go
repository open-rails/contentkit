package media

const (
	PhaseQueued = "queued"
)

const (
	PhaseDownloading = "downloading" // fetching the original
)

const (
	PhaseProbing = "probing"
)

const (
	PhaseEncoding = "encoding" // the single ffmpeg pass: ladder, audio, subtitles, sprite
)

const (
	PhaseMuxing = "muxing" // per-quality MP4 downloads
)

const (
	PhaseUploading = "uploading"
)

const (
	PhasePublishing = "publishing" // the manifest edit
)

const (
	PhaseImages = "images" // item-wide, after every file: poster frame from the renditions
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
	// Stage of Stages: each rung is published before the next one starts.
	Stage  int `json:"stage,omitempty"`
	Stages int `json:"stages,omitempty"`
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
