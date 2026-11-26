package media

// SourceAdmission records the destination a ZIP source was accepted for.
// Only server commit code writes it; HTTP operations cannot supply it.
type SourceAdmission struct {
	// Generation is the accepting commit ID. Removing and re-adding a retained
	// ZIP creates fresh work; metadata edits keep the existing generation.
	Generation string `json:"generation"`
	Target     string `json:"target"`
}
