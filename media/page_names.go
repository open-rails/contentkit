package media

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/open-rails/contentkit/media/layout"
)

func withPageSource(meta map[string]any, name, source string) map[string]any {
	page, err := pageMetadata(meta)
	if err != nil {
		return meta
	}
	if page["schema_version"] == nil {
		page["schema_version"] = 1
	}
	if page["source_name"] == nil {
		page["source_name"] = path.Base(source)
	}
	if page["name"] == nil {
		page["name"] = name
	}
	if _, from, to, ok := CanonicalPageName(source); ok {
		if page["span"] == nil {
			page["span"] = map[string]int{"from": from, "to": to}
		}
	} else if page["flags"] == nil {
		page["flags"] = []string{"unusual_source_name"}
	}
	out := maps.Clone(meta)
	if out == nil {
		out = map[string]any{}
	}
	out["page"] = page
	return out
}

// PageID is the SHA-256 of a reader page's stored original bytes, independent
// of virtual filename, reading position, edits and generated renditions.
func (k *RegisteredKind) PageID(f File) string {
	if !k.IsPageUpload(f) {
		return ""
	}
	digest, ok := layout.BlobDigest(f.Blob)
	if !ok {
		return "" // a staged source has no verified stored digest yet
	}
	return hex.EncodeToString(digest)
}

func pageNameUsed(k *RegisteredKind, m *Manifest, name string) bool {
	for _, f := range m.Files {
		if !k.IsPageUpload(f) {
			continue
		}
		id := k.NameOf(f.Path)
		if page, err := pageMetadata(f.Meta); err == nil {
			if value, ok := page["name"].(string); ok && value != "" {
				id = value
			}
		}
		if strings.EqualFold(id, name) {
			return true
		}
	}
	return false
}

// CanonicalPageName parses printed numbers and ranges into a comparable form.
// It is an analysis helper; filenames are not renamed to its padded result.
func CanonicalPageName(name string) (string, int, int, bool) {
	stem, _ := splitExt(path.Base(name))
	parts := strings.Split(stem, "-")
	if len(parts) < 1 || len(parts) > 2 {
		return "", 0, 0, false
	}
	numbers := make([]int, len(parts))
	for i, part := range parts {
		if part == "" || digits(part) != len(part) {
			return "", 0, 0, false
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 || n > 1_000_000_000 {
			return "", 0, 0, false
		}
		numbers[i] = n
	}
	from, to := numbers[0], numbers[0]
	if len(numbers) == 2 {
		to = numbers[1]
		if to <= from {
			return "", 0, 0, false
		}
		return fmt.Sprintf("%03d-%03d", from, to), from, to, true
	}
	return fmt.Sprintf("%03d", from), from, to, true
}

// PositionalName names the page at position (1-based) of count by its place
// alone: zero-padded to at least two digits, keeping ext.
func PositionalName(position, count int, ext string) string {
	return fmt.Sprintf("%0*d%s", max(2, len(strconv.Itoa(count))), position, ext)
}

// PageIncluded is the planner's persisted effective selection. An ordinary
// upload or an older page without a decision remains included.
func PageIncluded(f File) bool {
	page, err := pageMetadata(f.Meta)
	if err != nil {
		return true
	}
	included, known := page["included"].(bool)
	return !known || included
}

// nextPagePath resolves a real collision inside the locked manifest without
// replacing a meaningful filename with a positional number.
func nextPagePath(k *RegisteredKind, m *Manifest, requested string) string {
	used := map[string]bool{}
	for _, f := range m.Files {
		if !k.IsPageUpload(f) {
			continue
		}
		names := []string{k.NameOf(f.Path)}
		if page, err := pageMetadata(f.Meta); err == nil {
			if value, ok := page["name"].(string); ok && value != "" {
				names = append(names, value)
			}
		}
		for _, name := range names {
			used[name] = true
		}
	}
	_, ext := splitExt(requested)
	p := path.Join(path.Dir(requested), UniquePageName(k.NameOf(requested), used))
	if ext != "" {
		p += "." + ext
	}
	return p
}

// UniquePageName retains a safe stem and adds a readable suffix only on a
// collision. Case-folding avoids ambiguous names when the ZIP is extracted.
func UniquePageName(stem string, taken map[string]bool) string {
	stem = CleanName(stem)
	if stem == "" {
		stem = "page"
	}
	available := func(name string) bool {
		for existing := range taken {
			if strings.EqualFold(existing, name) {
				return false
			}
		}
		return true
	}
	for n := 1; ; n++ {
		name := stem
		if n > 1 {
			suffix := fmt.Sprintf(" (%d)", n)
			base := stem
			for CleanName(base+suffix) != base+suffix {
				_, size := utf8.DecodeLastRuneInString(base)
				base = base[:len(base)-size]
			}
			name = base + suffix
		}
		if available(name) {
			taken[name] = true
			return name
		}
	}
}

var pageAffixes = regexp.MustCompile(`^([^0-9]*)([0-9]+(?:-[0-9]+)?)([^0-9]*)$`)
var repeatedPageNumber = regexp.MustCompile(`^([0-9]+)_([0-9]+)$`)
var numericPageStem = regexp.MustCompile(`^[0-9]+(?:-[0-9]+)?$`)

// CleanPageBatchNames only removes identical numeric wrappers across a whole
// batch and pads inconsistent numeric widths. Custom names, letter suffixes,
// gaps and joined-page ranges remain meaningful names, not positional IDs.
func CleanPageBatchNames(sources []string) []string {
	out := make([]string, len(sources))
	stems, exts := make([]string, len(sources)), make([]string, len(sources))
	for i, source := range sources {
		base := path.Base(source)
		exts[i] = path.Ext(base)
		stems[i] = CleanName(strings.TrimSuffix(base, exts[i]))
		if repeated := repeatedPageNumber.FindStringSubmatch(stems[i]); repeated != nil {
			first, err1 := strconv.ParseUint(repeated[1], 10, 64)
			second, err2 := strconv.ParseUint(repeated[2], 10, 64)
			if err1 == nil && err2 == nil && first == second {
				stems[i] = repeated[1]
			}
		}
	}
	if len(stems) > 1 {
		prefix, suffix, common := "", "", true
		for i, stem := range stems {
			parts := pageAffixes.FindStringSubmatch(stem)
			if parts == nil {
				common = false
				break
			}
			if i == 0 {
				prefix, suffix = parts[1], parts[3]
			} else if prefix != parts[1] || suffix != parts[3] {
				common = false
				break
			}
		}
		// Do not treat custom labels such as 00a/01a as a disposable suffix.
		separator := func(s string) bool { return strings.ContainsAny(s, " _-.") }
		if common {
			for i, stem := range stems {
				if separator(prefix) {
					stem = strings.TrimPrefix(stem, prefix)
				}
				if separator(suffix) {
					stem = strings.TrimSuffix(stem, suffix)
				}
				stems[i] = stem
			}
		}
		minWidth, maxWidth, numeric := int(^uint(0)>>1), 0, 0
		for _, stem := range stems {
			if !numericPageStem.MatchString(stem) {
				continue
			}
			numeric++
			for _, number := range strings.Split(stem, "-") {
				minWidth, maxWidth = min(minWidth, len(number)), max(maxWidth, len(number))
			}
		}
		if numeric > 1 && minWidth != maxWidth {
			for i, stem := range stems {
				if !numericPageStem.MatchString(stem) {
					continue
				}
				parts := strings.Split(stem, "-")
				for j, number := range parts {
					parts[j] = strings.Repeat("0", maxWidth-len(number)) + number
				}
				stems[i] = strings.Join(parts, "-")
			}
		}
	}
	for i := range out {
		out[i] = stems[i] + exts[i]
	}
	return out
}

func sourceName(meta map[string]any, fallback string) string {
	if page, err := pageMetadata(meta); err == nil {
		if name, ok := page["source_name"].(string); ok && name != "" {
			return path.Base(name)
		}
	}
	return path.Base(fallback)
}

// preservePageMeta keeps server-owned provenance and staff choices when an
// ordinary put changes unrelated user metadata.
func preservePageMeta(next, previous map[string]any) map[string]any {
	if page, ok := previous["page"]; ok {
		next = maps.Clone(next)
		if next == nil {
			next = map[string]any{}
		}
		next["page"] = page
	}
	return next
}

func pageCollisionMeta(meta map[string]any, name string) map[string]any {
	page, err := pageMetadata(meta)
	if err != nil {
		return meta
	}
	page["name"] = name
	var flags []string
	if raw, ok := page["flags"]; ok {
		b, _ := json.Marshal(raw)
		_ = json.Unmarshal(b, &flags)
	}
	if !slices.Contains(flags, "duplicate_source_name") {
		flags = append(flags, "duplicate_source_name")
	}
	page["flags"] = flags
	out := maps.Clone(meta)
	out["page"] = page
	return out
}

// These fields are owned by the importer, not by upload clients.
var archiveStateKeys = []string{"archive_complete", "archive_completed_sha256", "archive_complete_sha256", "archive_pages_total"}

func clearArchiveState(meta map[string]any) map[string]any {
	out := maps.Clone(meta)
	for _, key := range archiveStateKeys {
		delete(out, key)
	}
	return out
}

func clientUploadOps(input []Op) []Op {
	ops := slices.Clone(input)
	for i := range ops {
		if ops[i].Meta == nil {
			continue
		}
		meta := clearArchiveState(ops[i].Meta)
		for _, key := range []string{"page"} {
			delete(meta, key)
		}
		if len(meta) == 0 {
			meta = nil
		}
		ops[i].Meta = meta
	}
	return ops
}
