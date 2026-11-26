package discovery

// HistoryOptions select one page of the signed-in reader's history.
type HistoryOptions struct {
	Reader
	Kind string
	// Status is seen, in_progress or completed; anything they touched by
	// default.
	Status       string
	CardEntities []string
	Cursor       string
	Summary      bool
	// Limit defaults to 24 and is capped at 100.
	Limit int
}

// ContinueOptions select one page of the reader's continue reading and
// watching.
type ContinueOptions struct {
	Reader
	Kind         string
	CardEntities []string
	Cursor       string
	// Limit defaults to 24 and is capped at 100.
	Limit int
}
