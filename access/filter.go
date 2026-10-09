package access

import (
	"slices"
	"strings"
)

// Filter is one viewer's access as one billing read left it: the declared
// tiers they hold and their keys under each declared keyspace. It does no I/O:
// listings push it into SQL (Filter.SQL), loops call Allows. A Filter is
// immutable and safe for concurrent use.
type Filter struct {
	g         *Gate
	anonymous bool
	unknown   bool
	err       error
	tiers     map[string]bool            // declared tiers whose answer is known: claimed or read
	held      map[string]map[string]bool // keyspace prefix -> held ids, for every keyspace read
	complete  map[string]bool            // keyspace prefix -> not truncated
}

// Anonymous reports a filter of an anonymous viewer: it holds nothing.
func (f *Filter) Anonymous() bool { return f.anonymous }

// Unknown reports a failed billing read: only public items (and tiers the
// Claims shortcut vouches for) are allowed, and nothing is for sale.
func (f *Filter) Unknown() bool { return f.unknown }

// Tier reports whether the viewer holds the declared tier name.
func (f *Filter) Tier(name string) bool { return f.tiers[name] }

// Members returns the scope ids (uuid text) of kind the viewer is a member of,
// sorted; for `= ANY(@ids::uuid[])`.
func (f *Filter) Members(kind string) []string { return f.ids(MembersPrefix(f.g.tenant, kind)) }

// Owned returns the item ids of kind the viewer owns, sorted.
func (f *Filter) Owned(kind string) []string { return f.ids(OwnPrefix(f.g.tenant, kind)) }

// Complete reports whether Owned and Members of kind are the viewer's whole
// holdings: kind is a declared owned or member kind whose keyspaces were read
// without truncation. An anonymous filter is complete; an unknown one is not.
func (f *Filter) Complete(kind string) bool {
	if f.anonymous {
		return true
	}
	read := false
	for _, p := range [...]string{OwnPrefix(f.g.tenant, kind), MembersPrefix(f.g.tenant, kind)} {
		if _, ok := f.held[p]; ok {
			read = true
			if !f.complete[p] {
				return false
			}
		}
	}
	return read
}

// Allows decides it from the filter alone. known is false when the answer
// needs a key the filter did not read in full (a truncated or undeclared
// keyspace, a failed read); Gate.Settle and Gate.Decide resolve those.
// Allows and Filter.SQL agree: the SQL admits exactly the rows Allows allows
// or does not know, and on an Unknown filter only the allowed ones.
func (f *Filter) Allows(it Item) (allowed, known bool) {
	r := it.Rule
	switch r.Level {
	case Public:
		return true, true
	case TierLevel, MembersLevel, PPV, MembersPPV:
	default:
		return false, true
	}
	ownHeld, ownKnown := f.has(OwnPrefix(it.Ref.TenantID, it.Ref.ContentKind), it.Ref.ContentID)
	if ownHeld {
		return true, true
	}
	switch r.Level {
	case TierLevel:
		known = true
		for _, t := range r.Tiers {
			h, k := f.tier(t)
			if h {
				return true, true
			}
			known = known && k
		}
	case MembersLevel:
		h, k := f.has(MembersPrefix(r.Scope.TenantID, r.Scope.ContentKind), r.Scope.ContentID)
		if h {
			return true, true
		}
		known = k
	default:
		known = true
	}
	return false, known && ownKnown
}

// has answers one own or members key from the keyspace it falls in.
func (f *Filter) has(prefix, id string) (held, known bool) {
	if f.anonymous {
		return false, true
	}
	set, read := f.held[prefix]
	if !read {
		return false, false
	}
	if set[id] {
		return true, true
	}
	return false, f.complete[prefix]
}

// tier answers one tier: undeclared tiers are never held.
func (f *Filter) tier(name string) (held, known bool) {
	if f.anonymous {
		return false, true
	}
	if h, ok := f.tiers[name]; ok {
		return h, true
	}
	return false, !f.g.tierSet[name]
}

func (f *Filter) ids(prefix string) []string {
	out := make([]string, 0, len(f.held[prefix]))
	for id := range f.held[prefix] {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

// load fills f from an answer to q: every requested key and prefix must be
// answered; keys under a prefix that are not that keyspace's own or members
// keys are ignored.
func (f *Filter) load(ans Answer, q Query) error {
	for _, k := range q.Keys {
		f.tiers[k] = ans.Keys[k]
	}
	for _, p := range q.Prefixes {
		h, ok := ans.Held[p]
		if !ok {
			return errMissingPrefix(p)
		}
		set := make(map[string]bool, len(h.Keys))
		for _, s := range h.Keys {
			id, ok := strings.CutPrefix(s, p)
			if !ok {
				continue
			}
			if k, err := ParseKey(s); err == nil && k.ref.ContentID == id {
				set[id] = true
			}
		}
		f.held[p], f.complete[p] = set, !h.Truncated
	}
	return nil
}
