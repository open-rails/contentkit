package discovery

// DateCount is one day or month of the index.
type DateCount struct {
	// Date is YYYY-MM-DD or YYYY-MM in the filter's time zone.
	Date string `json:"date"`
	// Count is the contents released then; Free those open to anyone.
	Count int `json:"count"`
	Free  int `json:"free"`
}

// ReleaseDates is one page of the index, newest first.
type ReleaseDates struct {
	Dates      []DateCount `json:"dates"`
	NextCursor string      `json:"next_cursor,omitempty"`
}
