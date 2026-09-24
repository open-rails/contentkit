// Package layout defines media object keys, dependency-free so the access
// worker can classify paths without importing the media runtime:
//
//	{tenant}/{kind}/{content_id}/manifest.json | manifests/{version}.json
//	                            /originals/{sha256-hex | u-uuid | slot | slot.json | i-uuid}
//	                            /blobs/{sha256-hex | u-uuid}
//	                            /public/{name}.webp
package layout

import (
	"encoding/hex"
	"strings"
)

// Folder areas.
const (
	AreaManifest  = "manifest" // manifest.json and manifests/{version}.json
	AreaOriginals = "originals"
	AreaBlobs     = "blobs"
	AreaPublic    = "public"
)

const (
	SHA256Prefix = "sha256-"
	UploadPrefix = "u-"
	InlinePrefix = "i-"
	PublicExt    = ".webp"
	// VersionParam versions a public URL: served immutable while it matches
	// the object's VersionMeta.
	VersionParam = "v"
	VersionMeta  = "of"
)

// Key is a parsed object key.
type Key struct {
	Tenant, Kind, ID string
	Area             string // AreaManifest, AreaOriginals, AreaBlobs or AreaPublic
	Name             string // file name within the area; the version id for manifests/
}

// Parse classifies an object key; ok is false for anything outside the
// layout, including keys nested deeper than it allows.
func Parse(key string) (Key, bool) {
	parts := strings.Split(key, "/")
	if len(parts) < 4 || !ValidSegment(parts[0]) || !ValidSegment(parts[1]) || !ValidSegment(parts[2]) {
		return Key{}, false
	}
	k := Key{Tenant: parts[0], Kind: parts[1], ID: parts[2]}
	rest := parts[3:]
	switch {
	case len(rest) == 1 && rest[0] == "manifest.json":
		k.Area = AreaManifest
	case len(rest) == 2 && rest[0] == "manifests":
		v, ok := strings.CutSuffix(rest[1], ".json")
		if !ok || !ValidSegment(v) {
			return Key{}, false
		}
		k.Area, k.Name = AreaManifest, v
	case len(rest) == 2 && rest[0] == AreaOriginals && ValidSegment(rest[1]):
		k.Area, k.Name = AreaOriginals, rest[1]
	case len(rest) == 2 && rest[0] == AreaBlobs && ValidBlobName(rest[1]):
		k.Area, k.Name = AreaBlobs, rest[1]
	case len(rest) == 2 && rest[0] == AreaPublic:
		n, ok := strings.CutSuffix(rest[1], PublicExt)
		if !ok || !ValidSegment(n) {
			return Key{}, false
		}
		k.Area, k.Name = AreaPublic, n
	default:
		return Key{}, false
	}
	return k, true
}

// ValidSegment keeps keys stable ASCII: [A-Za-z0-9._-]{1,128}, no leading dot.
func ValidSegment(s string) bool {
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

// ValidBlobName accepts "sha256-{64 lowercase hex}" and "u-{uuid}".
func ValidBlobName(name string) bool {
	if _, ok := ParseSHA256Name(name); ok {
		return true
	}
	id, ok := strings.CutPrefix(name, UploadPrefix)
	if !ok {
		return false
	}
	return canonicalUUID(id)
}

// ValidInlineName accepts "i-{uuid}", an inline image's id.
func ValidInlineName(name string) bool {
	id, ok := strings.CutPrefix(name, InlinePrefix)
	if !ok {
		return false
	}
	return canonicalUUID(id)
}

// canonicalUUID accepts only the lowercase 8-4-4-4-12 hex form.
func canonicalUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// ParseSHA256Name returns the digest of a "sha256-{hex}" name.
func ParseSHA256Name(name string) ([]byte, bool) {
	h, ok := strings.CutPrefix(name, SHA256Prefix)
	if !ok || len(h) != 64 || strings.ToLower(h) != h {
		return nil, false
	}
	sum, err := hex.DecodeString(h)
	return sum, err == nil
}
