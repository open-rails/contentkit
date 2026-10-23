// Package hub is the engine's search and signal planes for one tenant:
// keyword search and typeahead, and the ClickHouse signal plane. The
// discovery routes and the engine's jobs use it; hosts reach it only through
// ContentKit's API.
package hub

import (
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/search"
)

// ContentRef is the tenant-scoped reference to a content item or one of its
// versions. See contentref.
type ContentRef = contentref.ContentRef

// ContentKey is the comparable form of a ContentRef.
type ContentKey = contentref.ContentKey

// DocumentKey identifies one keyword document: a ContentRef in one language.
type DocumentKey = search.DocumentKey

// KeywordDocument is the host's canonical search input for one document.
type KeywordDocument = search.KeywordDocument

// Eligibility is the per-document visibility join; see search.Eligibility.
type Eligibility = search.Eligibility
