package layout

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Default maps logical public names to immutable rendered default objects.
// The gateway uses this deployment-owned selection only when an item is missing.
type Default struct {
	Namespace, Kind string
	Files           map[string]string // logical name -> sha256-{digest}.webp
}

// ParseDefaults parses "doujins/gallery: cover-460.webp=sha256-{digest}.webp".
func ParseDefaults(s string) ([]Default, error) {
	var out []Default
	for _, e := range fields(s, ";") {
		head, names, ok := strings.Cut(e, ":")
		ns, kind, ok2 := strings.Cut(strings.TrimSpace(head), "/")
		if !ok || !ok2 {
			return nil, fmt.Errorf("layout: defaults entry %q: want {namespace}/{kind}: {logical}={immutable},…", e)
		}
		d := Default{Namespace: ns, Kind: kind, Files: map[string]string{}}
		for _, entry := range fields(names, ",") {
			name, target, ok := strings.Cut(entry, "=")
			name, target = strings.TrimSpace(name), strings.TrimSpace(target)
			if _, dup := d.Files[name]; !ok || dup {
				return nil, fmt.Errorf("layout: invalid or duplicate default mapping %q", entry)
			}
			d.Files[name] = target
		}
		out = append(out, d)
	}
	if _, err := CompileDefaults(out); err != nil {
		return nil, err
	}
	return out, nil
}

// FormatDefaults is ParseDefaults' inverse, with entries sorted by namespace and kind.
func FormatDefaults(defs []Default) string {
	defs = slices.Clone(defs)
	slices.SortStableFunc(defs, func(a, b Default) int {
		return cmp.Or(strings.Compare(a.Namespace, b.Namespace), strings.Compare(a.Kind, b.Kind))
	})
	parts := make([]string, len(defs))
	for i, d := range defs {
		var files []string
		for _, name := range slices.Sorted(maps.Keys(d.Files)) {
			files = append(files, name+"="+d.Files[name])
		}
		parts[i] = d.Namespace + "/" + d.Kind + ": " + strings.Join(files, ", ")
	}
	return strings.Join(parts, "; ")
}

// CompileDefaults validates and copies the exact fallback selection.
func CompileDefaults(defs []Default) (map[string]map[string]string, error) {
	out := map[string]map[string]string{}
	for _, d := range defs {
		k := d.Namespace + "/" + d.Kind
		if _, dup := out[k]; dup || !ValidSegment(d.Namespace) || !ValidSegment(d.Kind) || len(d.Files) == 0 {
			return nil, fmt.Errorf("layout: default %q needs a valid, unique namespace and kind and at least one name", k)
		}
		for name, target := range d.Files {
			digest, webp := strings.CutSuffix(target, ".webp")
			if !ValidSegment(name) || !webp || !ValidHashName(digest) {
				return nil, fmt.Errorf("layout: default %s: invalid mapping %q=%q", k, name, target)
			}
		}
		out[k] = maps.Clone(d.Files)
	}
	return out, nil
}

// fields splits s at sep, trimming space and dropping empty fields.
func fields(s, sep string) []string {
	var out []string
	for _, f := range strings.Split(s, sep) {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}
