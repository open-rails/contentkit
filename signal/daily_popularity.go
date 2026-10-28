package signal

// DailyPopularityPreparation reports actual source preparation, independently
// of the inexpensive bucket read. Missing and recent days are rebuilt once in
// the caller's refresh pass; already prepared closed days are reused.
type DailyPopularityPreparation struct {
	Prepared int
	Reused   int
}
