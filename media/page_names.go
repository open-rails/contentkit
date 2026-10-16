package media

import (
	"encoding/json"
	"fmt"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"
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
	if canonical, _, _, ok := CanonicalPageName(name); ok {
		if page["name"] == nil {
			page["name"] = canonical
		}
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

func pageIdentityUsed(k *Kind, m *Manifest, name string) bool {
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
		if id == name {
			return true
		}
	}
	return false
}

// CanonicalPageName preserves printed numbers and ranges, padding to at least
// three digits. Non-numeric source names are allocated by the version planner.
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

// normalizedPagePath changes only numeric page names; ordinary upload groups
// retain their own naming rules.
func normalizedPagePath(k *Kind, p string) string {
	if !k.IsPageUpload(File{Path: p}) {
		return p
	}
	if name, _, _, ok := CanonicalPageName(p); ok {
		_, ext := splitExt(p)
		p = path.Join(path.Dir(p), name)
		if ext != "" {
			p += "." + ext
		}
	}
	return p
}

// nextPagePath allocates inside the locked manifest, so concurrent presigns
// cannot overwrite one another when their source names normalize equally.
func nextPagePath(k *Kind, m *Manifest, requested string) string {
	used := map[string]bool{}
	next := 0
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
			if _, _, to, ok := CanonicalPageName(name); ok && to > next {
				next = to
			}
		}
	}
	_, ext := splitExt(requested)
	for {
		next++
		name := fmt.Sprintf("%03d", next)
		if !used[name] {
			p := path.Join(path.Dir(requested), name)
			if ext != "" {
				p += "." + ext
			}
			return p
		}
	}
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
	if !slices.Contains(flags, "duplicate_printed_number") {
		flags = append(flags, "duplicate_printed_number")
	}
	page["flags"] = flags
	out := maps.Clone(meta)
	out["page"] = page
	return out
}

// These fields are owned by the importer, not by upload clients.
var archiveStateKeys = []string{"archive_complete", "archive_completed_sha256", "archive_complete_sha256"}

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
		for _, key := range []string{"page", "publishing_archives", "publishing_cover_errors"} {
			delete(meta, key)
		}
		if len(meta) == 0 {
			meta = nil
		}
		ops[i].Meta = meta
	}
	return ops
}
