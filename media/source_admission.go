package media

// SourceAdmission records the verified upload policy accepted for a ZIP source.
// Only server commit code writes it; HTTP operations cannot supply these fields.
// A background importer uses the same owner and ceiling when attaching decoded
// originals. Quota <= 0 is unlimited, as in PGLimits.Quota.
type SourceAdmission struct {
	// Generation is the accepting commit ID. Removing and re-adding a retained
	// ZIP creates fresh work; metadata edits keep the existing generation.
	Generation string `json:"generation"`
	Target     string `json:"target"`
	Accounting bool   `json:"accounting,omitempty"`
	Owner      string `json:"owner,omitempty"`
	Enforce    bool   `json:"enforce,omitempty"`
	Quota      int64  `json:"quota,omitempty"`
}
