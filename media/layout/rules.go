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

// ParseHosts parses "media.doujins.ai=doujins,accounts; media.hanime.media=hentai0,accounts".
func ParseHosts(s string) (map[string][]string, error) {
	m := map[string][]string{}
	for _, e := range fields(s, ";") {
		host, list, ok := strings.Cut(e, "=")
		host = strings.ToLower(strings.TrimSpace(host))
		if _, dup := m[host]; !ok || dup {
			return nil, fmt.Errorf("layout: hosts entry %q: want one {host}={namespace},… per host", e)
		}
		m[host] = fields(list, ",")
	}
	return CheckHosts(m)
}

// FormatHosts is ParseHosts' inverse, with hosts sorted.
func FormatHosts(m map[string][]string) string {
	var parts []string
	for _, host := range slices.Sorted(maps.Keys(m)) {
		parts = append(parts, host+"="+strings.Join(m[host], ","))
	}
	return strings.Join(parts, "; ")
}

// CheckHosts validates a host map: lower-case names without a port, each
// with valid namespaces. It returns a copy.
func CheckHosts(in map[string][]string) (map[string][]string, error) {
	if len(in) == 0 {
		return nil, fmt.Errorf("layout: hosts are required")
	}
	out := make(map[string][]string, len(in))
	for host, nss := range in {
		if host == "" || strings.Trim(host, "abcdefghijklmnopqrstuvwxyz0123456789.-") != "" || len(nss) == 0 || slices.ContainsFunc(nss, func(s string) bool { return !ValidSegment(s) }) {
			return nil, fmt.Errorf("layout: host %q needs a lower-case name without port and valid namespaces, got %q", host, nss)
		}
		out[host] = slices.Clone(nss)
	}
	return out, nil
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
