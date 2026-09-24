// Package layout defines media object keys, dependency-free so the access
// worker can classify paths without importing the media runtime:
//
//	{tenant}/{kind}/{content_id}/manifest.json | manifests/{version}.json
//	                            /originals/{sha256-hex | slot | slot.json | i-uuid}
//	                            /staging/{u-uuid}                               multipart uploads until placed; never served
//	                            /blobs/{sha256-hex | u-uuid}                    viewer token
//	                            /editor/{sha256-hex | name.webp | name.mp4}     editor token
//	                            /public/{name}.webp | {name}.mp4 (hover previews)
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
	// AreaEditor holds what only editors may fetch: EditorOnly variant blobs
	// and video posters and hover previews before they are published.
	AreaEditor = "editor"
	// AreaStaging holds multipart uploads (u-{uuid}) until the media worker
	// hashes them and places them in originals/ under their SHA-256.
	AreaStaging = "staging"
)

const (
	SHA256Prefix = "sha256-"
	UploadPrefix = "u-"
	InlinePrefix = "i-"
	PublicExt    = ".webp"
	PublicMP4Ext = ".mp4" // hover previews
)

// Key is a parsed object key.
type Key struct {
	Tenant, Kind, ID string
	Area             string // AreaManifest, AreaOriginals, AreaStaging, AreaBlobs, AreaEditor or AreaPublic
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
	case len(rest) == 2 && rest[0] == AreaStaging && ValidStagedName(rest[1]):
		k.Area, k.Name = AreaStaging, rest[1]
	case len(rest) == 2 && rest[0] == AreaBlobs && ValidBlobName(rest[1]):
		k.Area, k.Name = AreaBlobs, rest[1]
	case len(rest) == 2 && rest[0] == AreaEditor && ValidBlobName(rest[1]):
		k.Area, k.Name = AreaEditor, rest[1]
	case len(rest) == 2 && (rest[0] == AreaPublic || rest[0] == AreaEditor):
		n, ok := strings.CutSuffix(rest[1], PublicExt)
		if !ok {
			n, ok = strings.CutSuffix(rest[1], PublicMP4Ext)
		}
		if !ok || !ValidSegment(n) {
			return Key{}, false
		}
		k.Area, k.Name = rest[0], n
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
	_, ok := ParseSHA256Name(name)
	return ok || ValidStagedName(name)
}

// ValidStagedName accepts "u-{uuid}", an upload whose hash is not yet known.
func ValidStagedName(name string) bool {
	id, ok := strings.CutPrefix(name, UploadPrefix)
	return ok && canonicalUUID(id)
}

// SourceArea is where an uploaded file named name lives: staging/ for a
// "u-{uuid}" not yet placed, originals/ for a "sha256-{hex}".
func SourceArea(name string) string {
	if ValidStagedName(name) {
		return AreaStaging
	}
	return AreaOriginals
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
