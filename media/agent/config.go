package agent

import (
	"cmp"
	"encoding/hex"
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// ParseHosts parses "media.doujins.ai=doujins,accounts; media.hanime.media=hentai0,accounts".
func ParseHosts(s string) (map[string][]string, error) {
	m := map[string][]string{}
	for _, e := range fields(s, ";") {
		host, list, ok := strings.Cut(e, "=")
		host = strings.ToLower(strings.TrimSpace(host))
		if _, dup := m[host]; !ok || dup {
			return nil, fmt.Errorf("agent: hosts entry %q: want one {host}={namespace},… per host", e)
		}
		m[host] = fields(list, ",")
	}
	return checkHosts(m)
}

// FormatHosts is ParseHosts' inverse, with hosts sorted.
func FormatHosts(m map[string][]string) string {
	var parts []string
	for _, host := range slices.Sorted(maps.Keys(m)) {
		parts = append(parts, host+"="+strings.Join(m[host], ","))
	}
	return strings.Join(parts, "; ")
}

// ParseDefaults parses "doujins/gallery: cover-{w}.webp; hentai0/video: poster-{w}.webp, thumb-{w}.webp".
func ParseDefaults(s string) ([]Default, error) {
	var out []Default
	for _, e := range fields(s, ";") {
		head, names, ok := strings.Cut(e, ":")
		ns, kind, ok2 := strings.Cut(strings.TrimSpace(head), "/")
		if !ok || !ok2 {
			return nil, fmt.Errorf("agent: defaults entry %q: want {namespace}/{kind}: {name},…", e)
		}
		out = append(out, Default{Namespace: ns, Kind: kind, Names: fields(names, ",")})
	}
	if _, err := compileDefaults(out); err != nil {
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

func checkHosts(in map[string][]string) (map[string][]string, error) {
	if len(in) == 0 {
		return nil, fmt.Errorf("agent: Hosts is required")
	}
	out := make(map[string][]string, len(in))
	for host, nss := range in {
		if host == "" || strings.Trim(host, "abcdefghijklmnopqrstuvwxyz0123456789.-") != "" || len(nss) == 0 || !all(nss, validSegment) {
			return nil, fmt.Errorf("agent: host %q needs a lower-case name without port and valid namespaces, got %q", host, nss)
		}
		out[host] = slices.Clone(nss)
	}
	return out, nil
}

var (
	sampleName  = strings.NewReplacer("{w}", "0", "{name}", "n")
	namePattern = strings.NewReplacer(`\{w\}`, "[0-9]+", `\{name\}`, "[A-Za-z0-9._-]+")
)

// compileDefaults turns the name templates into anchored patterns keyed by "{ns}/{kind}".
func compileDefaults(defs []Default) (map[string][]*regexp.Regexp, error) {
	out := map[string][]*regexp.Regexp{}
	for _, d := range defs {
		k := d.Namespace + "/" + d.Kind
		if _, dup := out[k]; dup || !validSegment(d.Namespace) || !validSegment(d.Kind) || len(d.Names) == 0 {
			return nil, fmt.Errorf("agent: default %q needs a valid, unique namespace and kind and at least one name", k)
		}
		for _, n := range d.Names {
			if !validSegment(sampleName.Replace(n)) {
				return nil, fmt.Errorf("agent: default %s: invalid name template %q", k, n)
			}
			out[k] = append(out[k], regexp.MustCompile("^"+namePattern.Replace(regexp.QuoteMeta(n))+"$"))
		}
	}
	return out, nil
}

// validOrigin accepts exactly "scheme://host[:port]".
func validOrigin(o string) error {
	u, err := url.Parse(o)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || strings.Contains(u.Host, "*") ||
		u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || o != u.Scheme+"://"+u.Host {
		return fmt.Errorf("agent: invalid CORS origin %q (want exactly scheme://host[:port])", o)
	}
	return nil
}

// validSegment keeps keys stable ASCII: [A-Za-z0-9._-]{1,128}, no leading dot.
func validSegment(s string) bool {
	return s != "" && len(s) <= 128 && s[0] != '.' &&
		strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-") == ""
}

// validHashName accepts "sha256-{64 lowercase hex}".
func validHashName(name string) bool {
	h, ok := strings.CutPrefix(name, "sha256-")
	_, err := hex.DecodeString(h)
	return ok && len(h) == 64 && strings.ToLower(h) == h && err == nil
}

func all(ss []string, f func(string) bool) bool {
	return !slices.ContainsFunc(ss, func(s string) bool { return !f(s) })
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
