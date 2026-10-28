package media

// PageIncluded is the planner's persisted effective selection. An ordinary
// upload or an older page without a decision remains included.
func PageIncluded(f File) bool {
	page, err := pageMetadata(f.Meta)
	if err != nil {
		return true
	}
	included, known := page["included"].(bool)
	return !known || included
}
