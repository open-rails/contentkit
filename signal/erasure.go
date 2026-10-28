package signal

// ErasureReport describes one erasure or enforcement pass.
type ErasureReport struct {
	// Remaining counts rows still attributable to the subjects per table after
	// the pass; every value is zero when erasure is complete.
	Remaining map[string]uint64
}

// Complete reports whether no attributable rows remain.
func (r ErasureReport) Complete() bool {
	for _, n := range r.Remaining {
		if n > 0 {
			return false
		}
	}
	return true
}
