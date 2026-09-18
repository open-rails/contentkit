// Package contentkit is the deterministic content library: tenant-scoped
// keyword search over host content, the ClickHouse signal plane, and the
// discovery reads over both. It needs no model provider; probabilistic
// features plug into the optional ports defined here.
package contentkit

import (
	"context"

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

// SemanticRequest is one language's request for semantic candidates.
type SemanticRequest struct {
	Tenant   string
	Query    string // normalized user text
	Language string
	// ContentKinds bounds the candidate kinds (never empty).
	ContentKinds []string
	// Limit is the candidate window ContentKit will fuse.
	Limit int
	// Eligibility, FilterSQL and FilterArgs are the request's host constraints
	// as the keyword routes see them. A ranker may apply them itself or return
	// a superset: ContentKit re-verifies every candidate through the same join
	// before fusion, so a ranker never widens what a host allows.
	Eligibility *Eligibility
	FilterSQL   string
	FilterArgs  map[string]any
}

// SemanticCandidate is one document a ranker proposes with its own score domain.
type SemanticCandidate = search.Candidate

// SemanticRanker is the optional semantic candidate source. ContentKit calls it
// only when a request sets SearchOptions.Semantic and a ranker is registered,
// fuses its candidates with the keyword ranking (RRF), then groups and pages
// per content item exactly as keyword-only requests are. An absent ranker or
// a failing call degrades to keyword-only and is never a user-facing error.
type SemanticRanker interface {
	Rank(ctx context.Context, req SemanticRequest) ([]SemanticCandidate, error)
}
