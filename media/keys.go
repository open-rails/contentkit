package media

import (
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/contentkit/contentref"
)

// Folder areas.
const (
	AreaManifest  = "manifest" // manifest.json and manifests/{version}.json
	AreaOriginals = "originals"
	AreaBlobs     = "blobs"
	AreaPublic    = "public"
)

const (
	sha256Prefix = "sha256-"
	uploadPrefix = "u-"
	publicExt    = ".webp"
)

// Item is a validated content item and the keys of its folder:
//
//	{tenant}/{kind}/{content_id}/manifest.json | manifests/{version}.json
//	                            /originals/{sha256-hex | u-uuid | slot}
//	                            /blobs/{sha256-hex | u-uuid}
//	                            /public/{output}.webp
type Item struct {
	ref    contentref.ContentRef
	kind   Kind
	prefix string
}

// Item validates ref against the registry. A version is required to address
// a versioned kind's manifest, and refused for an unversioned kind.
func (r *Registry) Item(ref contentref.ContentRef) (Item, error) {
	k, err := r.Kind(ref.ContentKind)
	if err != nil {
		return Item{}, err
	}
	if !validSegment(ref.TenantID) || !validSegment(ref.ContentID) {
		return Item{}, fmt.Errorf("media: invalid ref %s", ref)
	}
	if v := ref.Version(); v != "" && (!k.Versioned || !validSegment(v)) {
		return Item{}, fmt.Errorf("media: invalid version in ref %s", ref)
	}
	if ref.ContentVersionID != nil && ref.Version() == "" {
		return Item{}, fmt.Errorf("media: empty version in ref %s", ref)
	}
	return Item{ref: ref, kind: k, prefix: ref.TenantID + "/" + ref.ContentKind + "/" + ref.ContentID + "/"}, nil
}

func (i Item) Ref() contentref.ContentRef { return i.ref }
func (i Item) Kind() Kind                 { return i.kind }

// Prefix is the folder, "{tenant}/{kind}/{id}/"; versions share it.
func (i Item) Prefix() string { return i.prefix }

// ManifestKey is manifest.json, or manifests/{version}.json for versioned kinds.
func (i Item) ManifestKey() (string, error) {
	if !i.kind.Versioned {
		return i.prefix + "manifest.json", nil
	}
	if v := i.ref.Version(); v != "" {
		return i.prefix + "manifests/" + v + ".json", nil
	}
	return "", fmt.Errorf("media: kind %q is versioned; ref %s has no version", i.kind.Name, i.ref)
}

// ManifestsPrefix lists every manifest of a versioned kind.
func (i Item) ManifestsPrefix() string { return i.prefix + "manifests/" }
func (i Item) OriginalsPrefix() string { return i.prefix + AreaOriginals + "/" }
func (i Item) BlobsPrefix() string     { return i.prefix + AreaBlobs + "/" }
func (i Item) PublicPrefix() string    { return i.prefix + AreaPublic + "/" }

// Original is the key of an uploaded file or master (never served).
func (i Item) Original(name string) (string, error) {
	if !ValidBlobName(name) {
		return "", fmt.Errorf("media: invalid original name %q", name)
	}
	return i.OriginalsPrefix() + name, nil
}

// Blob is the key of a served, immutable derivative.
func (i Item) Blob(name string) (string, error) {
	if !ValidBlobName(name) {
		return "", fmt.Errorf("media: invalid blob name %q", name)
	}
	return i.BlobsPrefix() + name, nil
}

// SlotOriginal is the fixed, overwritten original of a registered public slot.
func (i Item) SlotOriginal(slot string) (string, error) {
	if _, ok := i.kind.Slots[slot]; !ok {
		return "", fmt.Errorf("media: kind %q has no slot %q", i.kind.Name, slot)
	}
	return i.OriginalsPrefix() + slot, nil
}

// Public is public/{name}.webp: a slot output or a host-chosen inline image id.
func (i Item) Public(name string) (string, error) {
	if !validSegment(name) || isBlobName(name) {
		return "", fmt.Errorf("media: invalid public name %q", name)
	}
	return i.PublicPrefix() + name + publicExt, nil
}

// SHA256Name names content-addressed files: "sha256-{hex}".
func SHA256Name(sum []byte) string { return sha256Prefix + hex.EncodeToString(sum) }

// ParseSHA256Name returns the digest of a "sha256-{hex}" name.
func ParseSHA256Name(name string) ([]byte, bool) {
	h, ok := strings.CutPrefix(name, sha256Prefix)
	if !ok || len(h) != 64 || strings.ToLower(h) != h {
		return nil, false
	}
	sum, err := hex.DecodeString(h)
	return sum, err == nil
}

// NewUploadName names a multipart upload whose hash is unknown: "u-{uuid}".
func NewUploadName() string { return uploadPrefix + uuid.NewString() }

// ValidBlobName accepts "sha256-{64 lowercase hex}" and "u-{uuid}".
func ValidBlobName(name string) bool { return isBlobName(name) }

func isBlobName(name string) bool {
	if _, ok := ParseSHA256Name(name); ok {
		return true
	}
	id, ok := strings.CutPrefix(name, uploadPrefix)
	if !ok {
		return false
	}
	u, err := uuid.Parse(id)
	return err == nil && u.String() == id
}

// Key is a parsed object key.
type Key struct {
	Tenant, Kind, ID string
	Area             string // AreaManifest, AreaOriginals, AreaBlobs or AreaPublic
	Name             string // file name within the area; the version id for manifests/
}

// ParseKey classifies an object key built by this package; ok is false for
// anything else, including keys nested deeper than the layout allows.
func ParseKey(key string) (Key, bool) {
	parts := strings.Split(key, "/")
	if len(parts) < 4 || !validSegment(parts[0]) || !validSegment(parts[1]) || !validSegment(parts[2]) {
		return Key{}, false
	}
	k := Key{Tenant: parts[0], Kind: parts[1], ID: parts[2]}
	rest := parts[3:]
	switch {
	case len(rest) == 1 && rest[0] == "manifest.json":
		k.Area = AreaManifest
	case len(rest) == 2 && rest[0] == "manifests":
		v, ok := strings.CutSuffix(rest[1], ".json")
		if !ok || !validSegment(v) {
			return Key{}, false
		}
		k.Area, k.Name = AreaManifest, v
	case len(rest) == 2 && rest[0] == AreaOriginals && validSegment(rest[1]):
		k.Area, k.Name = AreaOriginals, rest[1]
	case len(rest) == 2 && rest[0] == AreaBlobs && isBlobName(rest[1]):
		k.Area, k.Name = AreaBlobs, rest[1]
	case len(rest) == 2 && rest[0] == AreaPublic:
		n, ok := strings.CutSuffix(rest[1], publicExt)
		if !ok || !validSegment(n) {
			return Key{}, false
		}
		k.Area, k.Name = AreaPublic, n
	default:
		return Key{}, false
	}
	return k, true
}

// validSegment keeps keys stable ASCII: [A-Za-z0-9._-]{1,128}, no leading dot.
func validSegment(s string) bool {
	if s == "" || len(s) > 128 || s[0] == '.' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}
