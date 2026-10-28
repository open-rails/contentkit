package media

// ProcessingCount counts source files, not individual encoded widths or chunks.
// Completed and Failed are disjoint; the remaining files are still pending.
type ProcessingCount struct {
	Completed int `json:"completed"`
	Total     int `json:"total"`
	Failed    int `json:"failed"`
}

// ArchiveStatus counts source ZIPs and, once validation has determined the
// expected entries, the pages actually committed by the ingest worker.
// Page counts are persisted progress, not a claim that a worker is running.
type ArchiveStatus struct {
	ProcessingCount
	Pages *ProcessingCount `json:"pages,omitempty"`
}

// PresetStatus is progress for one configured rendition preset.
type PresetStatus struct {
	Name string `json:"name"`
	ProcessingCount
}

// ProcessingFailure is an editor-visible failure without internal paths,
// source URLs or the worker's unfiltered error chain.
type ProcessingFailure struct {
	Path    string `json:"path"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// MediaStatus is a read-only editor snapshot of all uploads, including staging.
// Unattached sources count placement and private processing; their public presets
// wait until attachment. This state is separate from publication readiness.
// Public presets count after attachment. An archive may have finished
// placement while still waiting for extraction. Encoding is
// the existing worker checkpoint, when available, never a new job or operation.
type MediaStatus struct {
	State        string              `json:"state"`
	Uploads      ProcessingCount     `json:"uploads"`
	Archives     ArchiveStatus       `json:"archives"`
	Presets      []PresetStatus      `json:"presets"`
	Encoding     *EncodeProgress     `json:"encoding,omitempty"`
	FailureCount int                 `json:"failure_count"`
	Failures     []ProcessingFailure `json:"failures"`
}
