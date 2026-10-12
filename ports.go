// Package contentkit is the deterministic content library: tenant-scoped
// keyword search over ContentKit's content, the ClickHouse signal plane, and
// the discovery reads over both.
package contentkit

import (
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/search"
)

// ContentRef is the tenant-scoped reference to a content item or one of its
// versions. See contentref.
type ContentRef = contentref.ContentRef

// ContentKey is the comparable form of a ContentRef.
type ContentKey = contentref.ContentKey

// GroupID identifies a group (tag, artist, series, character, channel, rail).
type GroupID = contentref.GroupID

// DocumentKey identifies one keyword document: a ContentRef in one language.
type DocumentKey = search.DocumentKey

// KeywordDocument is the search input for one document.
type KeywordDocument = search.KeywordDocument
