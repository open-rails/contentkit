package layout

import (
	"cmp"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// Default declares the public names of {Namespace}/{Kind} that fall back to
// {Namespace}/{Kind}/_default/public/{name} when an item's object is
// missing: templates over [A-Za-z0-9._-] with {w} (digits) and {name}
// placeholders, e.g. "cover-{w}.webp". The access agent serves them; the
// registry derives them (media.AgentConfig).
type Default struct {
	Namespace, Kind string
	Names           []string
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

// ParseDefaults parses "doujins/gallery: cover-{w}.webp; hentai0/video: poster-{w}.webp, thumb-{w}.webp".
func ParseDefaults(s string) ([]Default, error) {
	var out []Default
	for _, e := range fields(s, ";") {
		head, names, ok := strings.Cut(e, ":")
		ns, kind, ok2 := strings.Cut(strings.TrimSpace(head), "/")
		if !ok || !ok2 {
			return nil, fmt.Errorf("layout: defaults entry %q: want {namespace}/{kind}: {name},…", e)
		}
		out = append(out, Default{Namespace: ns, Kind: kind, Names: fields(names, ",")})
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
		parts[i] = d.Namespace + "/" + d.Kind + ": " + strings.Join(d.Names, ", ")
	}
	return strings.Join(parts, "; ")
}

var (
	sampleName  = strings.NewReplacer("{w}", "0", "{name}", "n")
	namePattern = strings.NewReplacer(`\{w\}`, "[0-9]+", `\{name\}`, "[A-Za-z0-9._-]+")
)

// CompileDefaults turns the name templates into anchored patterns keyed by "{ns}/{kind}".
func CompileDefaults(defs []Default) (map[string][]*regexp.Regexp, error) {
	out := map[string][]*regexp.Regexp{}
	for _, d := range defs {
		k := d.Namespace + "/" + d.Kind
		if _, dup := out[k]; dup || !ValidSegment(d.Namespace) || !ValidSegment(d.Kind) || len(d.Names) == 0 {
			return nil, fmt.Errorf("layout: default %q needs a valid, unique namespace and kind and at least one name", k)
		}
		for _, n := range d.Names {
			if !ValidSegment(sampleName.Replace(n)) {
				return nil, fmt.Errorf("layout: default %s: invalid name template %q", k, n)
			}
			out[k] = append(out[k], regexp.MustCompile("^"+namePattern.Replace(regexp.QuoteMeta(n))+"$"))
		}
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
