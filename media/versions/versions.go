// Package versions publishes content versions and binds their optional media.
// A kind's media items are its versions, {kind, content, version}; a content has none of
// its own. This package answers media's
// hooks from the content model (who uploads, who reads, the facts a
// version's media yields), records immutable ready-file snapshots for viewers
// (chapter metadata, signed previews and playback), and serves the reader plan,
// reader metadata and downloads. Each version receives its own explicit uploads.
//
// Whether a viewer sees, opens or edits a content or a version is the
// catalog's decision over the access gate, ContentKit's own
// resolve.Resolver; this package adds segments' release and the rule's
// preview to it. The listing projection is refreshed through Facets.
package versions

// Target names a content (Version "") or one of its versions.
type Target struct {
	Content string `json:"content"`
	Version string `json:"version,omitempty"`
}
