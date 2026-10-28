package media

const (
	EventProcessingProgressed = "media.processing_progressed"
)

const (
	EventProcessingCompleted = "media.processing_completed"
)

// ProcessingEvent is a durable observation of media, without delivery URLs or
// credentials. Completion includes settled failures; State distinguishes them.
type ProcessingEvent struct {
	Kind         string          `json:"kind"`
	ContentID    string          `json:"content_id"`
	VersionID    string          `json:"version_id,omitempty"`
	State        string          `json:"state"`
	Uploads      ProcessingCount `json:"uploads"`
	Archives     ArchiveStatus   `json:"archives"`
	Presets      []PresetStatus  `json:"presets"`
	FailureCount int             `json:"failure_count"`
}
