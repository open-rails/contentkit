package discovery

import (
	"github.com/open-rails/contentkit/access"
)

// Reader is who reads, on which site, in which language.
type Reader struct {
	Actor access.Actor
	// Host is the request's host: it picks the site scope.
	Host string
	// Language lists only content with a live version serving it.
	Language string
	// DisplayLanguage is the language of titles and entity names and the
	// preferred language of each card's version; Language, else the site's
	// first.
	DisplayLanguage string
	// Mode is lock (the default) or hide.
	Mode access.Mode
}

const (
	Unseen = "unseen"
)

const (
	InProgress = "in_progress"
)

const (
	Completed = "completed"
)

const (
	SortNewest = "newest"
)

const (
	SortOldest = "oldest"
)

const (
	SortTitle = "title"
)

const (
	SortPopular = "popular"
)

const (
	SortTopRated = "top_rated"
)

const (
	SortMostFavorited = "most_favorited"
)

const (
	SortPosition = "position"
)

const (
	SortRandom = "random"
)
