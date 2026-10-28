package media

// CopyRequest stages a server-side copy of From, a key of the item's bucket
// outside ContentKit's namespaces, as an upload at Path.
type CopyRequest struct {
	Path string `json:"path"`
	Type string `json:"type"`
	From string `json:"from"`
	Size int64  `json:"size,omitempty"` // checked when > 0
}

// StagedCopy is a staged copy for a commit's put: its path and blob.
type StagedCopy struct {
	Path string `json:"path"`
	Blob string `json:"blob"`
	Size int64  `json:"size"`
}
