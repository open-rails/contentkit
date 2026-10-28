package discovery

import (
	"time"
)

// SearchRetention is how long a recent search is kept after its last use.
const SearchRetention = 180 * 24 * time.Hour

// RecentSearch is one query the reader searched.
type RecentSearch struct {
	Query string `json:"query"`
	// Results is how many cards its first page showed.
	Results    int       `json:"results"`
	SearchedAt time.Time `json:"searched_at"`
}

// RecentSearches is the reader's recent searches, newest first.
type RecentSearches struct {
	Searches []RecentSearch `json:"searches"`
}
