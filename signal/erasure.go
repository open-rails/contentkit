package signal

// ErasureReport describes one erasure replay.
type ErasureReport struct {
	// Remaining counts what is still to erase per store after the pass; every
	// value is zero when erasure is complete.
	Remaining map[string]uint64
}

// Complete reports whether nothing remains to erase.
func (r ErasureReport) Complete() bool {
	for _, n := range r.Remaining {
		if n > 0 {
			return false
		}
	}
	return true
}
