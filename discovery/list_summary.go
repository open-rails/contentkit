package discovery

// ListingSummary describes this query's selection before cursor pagination.
// Only optional entity preferences are relaxed to count filtered contents.
type ListingSummary struct {
	FilteredCount    int                     `json:"filtered_count"`
	FilteredEntities []ReleaseFilteredEntity `json:"filtered_entities"`
	LockedCount      int                     `json:"locked_count"`
	// Total is the contents the list pages through under its access mode.
	Total int `json:"total"`
	// Truncated makes counts lower bounds for bounded keyword or recommendation
	// candidate windows. Ordinary lists have exact selection counts.
	Truncated bool `json:"truncated,omitempty"`
}
