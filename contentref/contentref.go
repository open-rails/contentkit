// Package contentref defines the reference vocabulary every ContentKit package
// and port shares: a tenant-scoped reference to host-owned content and the
// identity of a generic taxonomy record.
package contentref

import (
	"fmt"
	"strings"
)

// ContentRef identifies host-owned content: the work (a gallery, a video, a
// listing) or, when ContentVersionID is set, one selectable version of it. The
// host owns the meaning of ContentKind and the ids; ContentKit stores them
// opaquely, except that ContentID must be a canonical UUIDv7 (ValidateID). A
// language is never part of the reference.
type ContentRef struct {
	TenantID         string  `json:"tenant_id"`
	ContentKind      string  `json:"content_kind"`
	ContentID        string  `json:"content_id"`
	ContentVersionID *string `json:"content_version_id,omitempty"`
}

// New returns a reference to the work itself. Every ContentKit entry point
// validates it; Parse validates it at construction.
func New(tenantID, contentKind, contentID string) ContentRef {
	return ContentRef{TenantID: tenantID, ContentKind: contentKind, ContentID: contentID}
}

// Parse returns a validated reference to the work.
func Parse(tenantID, contentKind, contentID string) (ContentRef, error) {
	r := New(tenantID, contentKind, contentID)
	return r, r.Validate()
}

// NewVersion returns a reference to one version of the work.
func NewVersion(tenantID, contentKind, contentID, contentVersionID string) ContentRef {
	return New(tenantID, contentKind, contentID).WithVersion(contentVersionID)
}

// WithVersion returns the reference scoped to contentVersionID ("" = the work).
func (r ContentRef) WithVersion(contentVersionID string) ContentRef {
	if contentVersionID == "" {
		r.ContentVersionID = nil
		return r
	}
	r.ContentVersionID = &contentVersionID
	return r
}

// Content returns the work-level reference.
func (r ContentRef) Content() ContentRef {
	r.ContentVersionID = nil
	return r
}

// Version returns the version id, "" for the work itself.
func (r ContentRef) Version() string {
	if r.ContentVersionID == nil {
		return ""
	}
	return *r.ContentVersionID
}

// Key returns the comparable form used for map keys and equality.
func (r ContentRef) Key() ContentKey {
	return ContentKey{TenantID: r.TenantID, ContentKind: r.ContentKind, ContentID: r.ContentID, ContentVersionID: r.Version()}
}

// Equal reports whether both references name the same content and version.
func (r ContentRef) Equal(o ContentRef) bool { return r.Key() == o.Key() }

// Validate requires a tenant, kind and a UUIDv7 id (ErrInvalidID); a set
// version must be non-empty.
func (r ContentRef) Validate() error {
	if strings.TrimSpace(r.TenantID) == "" || strings.TrimSpace(r.ContentKind) == "" {
		return fmt.Errorf("contentref: TenantID and ContentKind are required")
	}
	if err := ValidateID(r.ContentID); err != nil {
		return err
	}
	if r.ContentVersionID != nil && strings.TrimSpace(*r.ContentVersionID) == "" {
		return fmt.Errorf("contentref: ContentVersionID must be nil or non-empty")
	}
	return nil
}

func (r ContentRef) String() string {
	s := r.TenantID + "/" + r.ContentKind + "/" + r.ContentID
	if v := r.Version(); v != "" {
		s += "@" + v
	}
	return s
}

// ContentKey is the comparable form of a ContentRef; ContentVersionID is ""
// for the work. Storage keys use the same encoding.
type ContentKey struct {
	TenantID         string
	ContentKind      string
	ContentID        string
	ContentVersionID string
}

// Ref converts the key back into a reference.
func (k ContentKey) Ref() ContentRef {
	return New(k.TenantID, k.ContentKind, k.ContentID).WithVersion(k.ContentVersionID)
}

// TaxonomyID identifies a generic ContentKit catalog record (tag, artist,
// series, creator, character, voice actor). Until the taxonomy module lands,
// taxonomy records are indexed and referenced through ContentRef with the
// taxonomy kind as ContentKind and the TaxonomyID as ContentID.
type TaxonomyID string
