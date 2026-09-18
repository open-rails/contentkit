// Package contentkit is the deterministic content library: tenant-scoped
// keyword search over host content, the ClickHouse signal plane, and the
// discovery reads over both. The DocumentSink port publishes
// document changes to optional external consumers.
package contentkit

import (
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/search"
)

// ContentRef is the tenant-scoped reference to host-owned content (the work or
// one of its versions). See contentref.
type ContentRef = contentref.ContentRef

// ContentKey is the comparable form of a ContentRef.
type ContentKey = contentref.ContentKey

// TaxonomyID identifies a generic ContentKit catalog record (tag, artist,
// series, creator, character, voice actor).
type TaxonomyID = contentref.TaxonomyID

// DocumentKey identifies one keyword document: a ContentRef in one language.
type DocumentKey = search.DocumentKey

// KeywordDocument is the host's canonical search input for one document.
type KeywordDocument = search.KeywordDocument

// Eligibility is the host's per-document eligibility join; see search.Eligibility.
type Eligibility = search.Eligibility

// PublishedDocument is one keyword document as delivered to a DocumentSink.
type PublishedDocument = search.PublishedDocument

// DocumentSink is the optional document port (see search.DocumentSink): the
// worker delivers every published keyword document to it at least once.
type DocumentSink = search.DocumentSink
