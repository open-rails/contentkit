package access

import (
	"fmt"
	"slices"
	"strings"
)

// Filter is one viewer's access as one billing read left it: the configured
// tiers they hold and their keys in the tenant's content and group
// keyspaces. It does no I/O: listings push it into SQL (Filter.SQL), loops
// call Allows. A Filter is immutable and safe for concurrent use.
type Filter struct {
	g         *Gate
	anonymous bool
	unknown   bool
	err       error
	tiers     map[string]bool   // configured tiers whose answer is known: claimed or read
	contents  map[string]bool   // legacy parent grants, retained only for audit/Holds
	versions  map[string]string // version id -> content id of held version keys
	entities  map[string]bool   // group ids of held group keys
	// contentsComplete and groupsComplete: the keyspace was read in full.
	contentsComplete, entitiesComplete bool
}

// Anonymous reports a filter of an anonymous viewer: it holds nothing.
func (f *Filter) Anonymous() bool { return f.anonymous }

// Unknown reports a failed billing read: only free rules and claimed tiers
// open, and nothing is for sale.
func (f *Filter) Unknown() bool { return f.unknown }

// Tier reports whether the viewer holds the configured tier name.
func (f *Filter) Tier(name string) bool { return f.tiers[name] }

// Contents returns legacy parent grants for audit and explicit migration.
// They do not grant version access and must not be used as audience ownership.
func (f *Filter) Contents() []string { return sortedKeys(f.contents) }

// Versions returns the held version keys: version id -> content id.
func (f *Filter) Versions() map[string]string {
	out := make(map[string]string, len(f.versions))
	for v, c := range f.versions {
		out[v] = c
	}
	return out
}

// Channels returns the channel ids whose membership key the viewer holds, sorted.
func (f *Filter) Channels() []string { return sortedKeys(f.entities) }

// Complete reports whether Contents, Versions and Channels are the viewer's
// whole holdings. An anonymous filter is complete; an unknown one is not.
func (f *Filter) Complete() bool {
	return !f.unknown && f.contentsComplete && f.entitiesComplete
}

// Allows decides it from the filter alone. known is false when the answer
// needs a key the filter did not read in full (a truncated keyspace, a failed
// read); Gate.Settle and Gate.Decide resolve those. Allows and SQL(Hide)
// agree: the SQL admits exactly the rows Allows allows or does not know, and
// on an Unknown filter only the allowed ones.
func (f *Filter) Allows(it Item) (allowed, known bool) {
	p, err := f.g.planOne(it)
	if err != nil {
		return false, true
	}
	d, unknown := verdict(p, &knowledge{filter: f}, f.g.now(), false)
	return d.Allowed, len(unknown) == 0
}

// lookup answers one key from the keyspace it falls in.
func (f *Filter) lookup(k Key) (held, known bool) {
	if f.anonymous || k.kind != KeyTier && k.tenant != f.g.tenant {
		return false, true
	}
	switch k.kind {
	case KeyTier:
		if h, ok := f.tiers[k.id]; ok {
			return h, true
		}
		return false, !f.g.tierSet[k.id]
	case KeyContent:
		return f.contents[k.id], f.contents[k.id] || f.contentsComplete && !f.unknown
	case KeyVersion:
		parent, present := f.versions[k.version]
		h := present && parent == k.id
		return h, h || f.contentsComplete && !f.unknown
	case KeyChannel:
		return f.entities[k.id], f.entities[k.id] || f.entitiesComplete && !f.unknown
	}
	return false, true
}

// load fills f from an answer to q: every requested key and prefix must be
// answered. Keys under a prefix outside the grammar or of another tenant are
// ignored.
func (f *Filter) load(ans Answer, q Query) error {
	for _, k := range q.Keys {
		f.tiers[k] = ans.Keys[k]
	}
	for _, p := range q.Prefixes {
		h, ok := ans.Held[p]
		if !ok {
			return fmt.Errorf("entitlements answer lacks prefix %q", p)
		}
		for _, s := range h.Keys {
			k, err := ParseKey(s)
			if err != nil || !strings.HasPrefix(s, p) || k.tenant != f.g.tenant {
				continue
			}
			switch k.kind {
			case KeyContent:
				f.contents[k.id] = true
			case KeyVersion:
				if parent, exists := f.versions[k.version]; exists && parent != k.id {
					return fmt.Errorf("entitlements contain conflicting parents for version %q", k.version)
				}
				f.versions[k.version] = k.id
			case KeyChannel:
				f.entities[k.id] = true
			}
		}
		if p == ContentPrefix(f.g.tenant) {
			f.contentsComplete = !h.Truncated
		} else {
			f.entitiesComplete = !h.Truncated
		}
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
