package media

const (
	StateReady = "ready"
)

const (
	StateProcessing = "processing"
)

const (
	StateFailed = "failed"
)

const (
	StateMissing = "missing" // a required upload is absent
)

// Readiness is whether an item's media is processed: ready when no attached
// upload is pending; processing while any is; failed once nothing is
// processing and an upload failed for its current blob and edit; missing
// once nothing is processing or failed and a required upload is absent.
// Processing and Failed name upload paths, Missing required upload paths.
type Readiness struct {
	State      string   `json:"state"`
	Processing []string `json:"processing,omitempty"`
	Failed     []string `json:"failed,omitempty"`
	Missing    []string `json:"missing,omitempty"`
}

func (r Readiness) Ready() bool { return r.State == StateReady }
