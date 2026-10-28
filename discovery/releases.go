package discovery

import (
	"github.com/open-rails/contentkit/entities"
)

type ReleaseDay struct {
	Date     string `json:"date"`
	Contents []Card `json:"contents"`
	// FilteredCount is the unique contents omitted only by optional entity
	// preferences, not by access or hard domain policy.
	FilteredCount int `json:"filtered_count"`
	// Counts overlap, so their sum is not FilteredCount. Hidden public filters
	// with no matches have count zero. Locked entity redaction still applies.
	FilteredEntities []ReleaseFilteredEntity `json:"filtered_entities"`
}

type ReleaseFilteredEntity struct {
	Entity entities.EntityCard `json:"entity"`
	Count  int                 `json:"count"`
}

// DailyReleases contains every visible card in the requested window, without
// a per-day or whole-response item limit. Empty days preserve its extent.
type DailyReleases struct {
	TimeZone    string       `json:"time_zone"`
	FromDate    string       `json:"from_date"`
	ThroughDate string       `json:"through_date"`
	Days        []ReleaseDay `json:"days"`
}
