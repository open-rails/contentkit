// Package layout defines media object keys, dependency-free so the access
// worker can classify paths without importing the media runtime:
//
//	{tenant}/{kind}/{content_id}/manifest.json         the item's one manifest; never served
//	                            /originals/sha256-{hex} uploads; never served
//	                            /temp/u-{uuid}          staged multipart uploads until placed; never served
//	                            /temp/e-{hex}           editor views; editor token only
//	                            /private/sha256-{hex}   every rendition; token-gated
//	                            /public/sha256-{hex}    copies of the exposed renditions; anyone
//
// originals/, private/ and public/ names are the SHA-256 of the object, so
// objects are immutable: a change writes a new name. temp/ is discardable:
// nothing a viewer needs lives there, and the sweep wipes it by age.
package layout

import (
	"encoding/hex"
	"strings"
)

// Folder areas.
const (
	AreaManifest  = "manifest"
	AreaOriginals = "originals"
	AreaTemp      = "temp"
	AreaPrivate   = "private"
	AreaPublic    = "public"
)

// ManifestName is the manifest's key within the folder.
const ManifestName = "manifest.json"

const (
	SHA256Prefix = "sha256-"
	UploadPrefix = "u-"
	InlinePrefix = "i-"
	EditorPrefix = "e-"
)

// Key is a parsed object key.
type Key struct {
	Tenant, Kind, ID string
	Area             string
	Name             string // file name within the area; "" for the manifest
}

// Parse classifies an object key; ok is false for anything outside the
// layout.
func Parse(key string) (Key, bool) {
	parts := strings.Split(key, "/")
	if len(parts) < 4 || len(parts) > 5 || !ValidSegment(parts[0]) || !ValidSegment(parts[1]) || !ValidSegment(parts[2]) {
		return Key{}, false
	}
	k := Key{Tenant: parts[0], Kind: parts[1], ID: parts[2]}
	rest := parts[3:]
	switch {
	case len(rest) == 1 && rest[0] == ManifestName:
		k.Area = AreaManifest
	case len(rest) == 2 && rest[0] == AreaTemp && (ValidStagedName(rest[1]) || ValidEditorName(rest[1])):
		k.Area, k.Name = AreaTemp, rest[1]
	case len(rest) == 2 && (rest[0] == AreaOriginals || rest[0] == AreaPrivate || rest[0] == AreaPublic) && ValidHashName(rest[1]):
		k.Area, k.Name = rest[0], rest[1]
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

// ValidHashName accepts "sha256-{64 lowercase hex}".
func ValidHashName(name string) bool {
	_, ok := ParseSHA256Name(name)
	return ok
}

// ValidSourceName accepts an uploaded file's name: a hash, or "u-{uuid}"
// until the worker places it.
func ValidSourceName(name string) bool { return ValidHashName(name) || ValidStagedName(name) }

// ValidStagedName accepts "u-{uuid}", an upload whose hash is not yet known.
func ValidStagedName(name string) bool {
	id, ok := strings.CutPrefix(name, UploadPrefix)
	return ok && canonicalUUID(id)
}

// ValidEditorName accepts "e-{64 lowercase hex}", an editor view.
func ValidEditorName(name string) bool {
	h, ok := strings.CutPrefix(name, EditorPrefix)
	return ok && len(h) == 64 && strings.ToLower(h) == h && validHex(h)
}

func validHex(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil
}

// SourceArea is where an uploaded file named name lives: temp/ for a
// "u-{uuid}" not yet placed, originals/ for a "sha256-{hex}".
func SourceArea(name string) string {
	if ValidStagedName(name) {
		return AreaTemp
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
