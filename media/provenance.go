package media

// ZipRecipe versions the Zip producer.
const ZipRecipe = "zip-source-names-2"

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
	StateFull = "full" // the manifest is Full: work is left, stopped until a commit shrinks it
)

// Readiness is whether an item's media is processed: ready when no attached
// upload is pending (its public presets count only while the item is
// visible) and every zip packs its inputs; processing while any is; failed
// once nothing is processing and an upload failed for its current blob and
// edit; full while work is left on a Full manifest. Processing and Failed
// name upload paths (and zip outputs).
type Readiness struct {
	State      string   `json:"state"`
	Processing []string `json:"processing,omitempty"`
	Failed     []string `json:"failed,omitempty"`
}

func (r Readiness) Ready() bool { return r.State == StateReady }
