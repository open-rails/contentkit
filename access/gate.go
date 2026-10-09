package access

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/open-rails/contentkit/contentref"
)

// ErrUnavailable wraps a failed billing read. The result it comes with is the
// fail-closed one: public items only, nothing for sale.
var ErrUnavailable = errors.New("access: entitlements unavailable")

func errMissingPrefix(p string) error {
	return fmt.Errorf("entitlements answer lacks prefix %q", p)
}

// GateConfig configures a Gate.
type GateConfig struct {
	Entitlements Entitlements // required
	Tenant       string       // required: the tenant of every item and scope
	// Tiers are the declared tier names, the only bare keys ContentKit
	// interprets. Rules may name no other.
	Tiers []string
	// MemberKinds are the scope kinds listings filter on ("channel") and
	// OwnedKinds the item kinds ("post"): Filter reads each keyspace.
	MemberKinds []string
	OwnedKinds  []string
	// HeldLimit is the keys read per keyspace (default DefaultHeldLimit, at
	// most MaxHeldLimit); a viewer holding more gets an incomplete Filter.
	HeldLimit int
	// Claims, optional, returns the verified token's entitlement claims. A
	// declared tier among them is held without a billing read; an absent one
	// is read live. It is never consulted for own or members keys. A refund
	// then keeps tier access until the token expires.
	Claims func(context.Context) []string
	Logger *slog.Logger
}

// Gate decides access to content from one billing read per viewer and request.
type Gate struct {
	port     Entitlements
	tenant   string
	tiers    []string
	tierSet  map[string]bool
	members  map[string]bool
	owned    map[string]bool
	prefixes []string
	limit    int
	claims   func(context.Context) []string
	log      *slog.Logger
	anon     *Filter
}

// NewGate validates c.
func NewGate(c GateConfig) (*Gate, error) {
	if c.Entitlements == nil {
		return nil, errors.New("access: GateConfig.Entitlements is required")
	}
	if !validComponent(c.Tenant) {
		return nil, fmt.Errorf("access: GateConfig.Tenant %q must match [a-z0-9_-]{1,64}", c.Tenant)
	}
	g := &Gate{port: c.Entitlements, tenant: c.Tenant, tierSet: map[string]bool{}, members: map[string]bool{},
		owned: map[string]bool{}, limit: c.HeldLimit, claims: c.Claims, log: c.Logger}
	if g.log == nil {
		g.log = slog.Default()
	}
	if g.limit == 0 {
		g.limit = DefaultHeldLimit
	}
	if g.limit < 1 || g.limit > MaxHeldLimit {
		return nil, fmt.Errorf("access: GateConfig.HeldLimit %d is outside 1..%d", c.HeldLimit, MaxHeldLimit)
	}
	for _, t := range c.Tiers {
		if !validTier(t) || g.tierSet[t] {
			return nil, fmt.Errorf("access: GateConfig.Tiers: %q is invalid or repeated", t)
		}
		g.tierSet[t] = true
		g.tiers = append(g.tiers, t)
	}
	slices.Sort(g.tiers)
	for _, kinds := range []struct {
		names  []string
		set    map[string]bool
		prefix func(string, string) string
	}{{c.MemberKinds, g.members, MembersPrefix}, {c.OwnedKinds, g.owned, OwnPrefix}} {
		for _, k := range kinds.names {
			if !validComponent(k) || kinds.set[k] {
				return nil, fmt.Errorf("access: GateConfig kind %q is invalid or repeated", k)
			}
			kinds.set[k] = true
			g.prefixes = append(g.prefixes, kinds.prefix(g.tenant, k))
		}
	}
	if len(g.prefixes) > MaxPrefixes {
		return nil, fmt.Errorf("access: at most %d member and owned kinds", MaxPrefixes)
	}
	g.anon = &Filter{g: g, anonymous: true}
	return g, nil
}

// TierKeys returns the declared tiers, sorted: the allowlist of tier names a
// host may show from token claims.
func (g *Gate) TierKeys() []string { return slices.Clone(g.tiers) }

// Filter reads the viewer's access in one billing read: the declared tiers
// and every declared keyspace. Within WithMemo it is read once per request.
// An anonymous viewer costs no read, nor does a viewer whose every declared
// tier is claimed when no keyspace is declared. On a failed read Filter
// returns the fail-closed filter (Unknown) and an error wrapping
// ErrUnavailable: serve that filter (public rows only) or answer 503.
func (g *Gate) Filter(ctx context.Context, actor Actor) (*Filter, error) {
	if anonymous(actor) {
		return g.anon, nil
	}
	e := memoFor(ctx, g, actor.ID)
	if f := e.loadFilter(); f != nil {
		return f, f.err
	}
	f := g.read(ctx, actor.ID)
	e.storeFilter(f)
	return f, f.err
}

func (g *Gate) read(ctx context.Context, subject string) *Filter {
	f := &Filter{g: g, tiers: map[string]bool{}, held: map[string]map[string]bool{}, complete: map[string]bool{}}
	claimed := g.claimed(ctx)
	q := Query{Prefixes: g.prefixes, Limit: g.limit}
	for _, t := range g.tiers {
		if claimed[t] {
			f.tiers[t] = true
		} else {
			q.Keys = append(q.Keys, t)
		}
	}
	if len(q.Keys) == 0 && len(q.Prefixes) == 0 {
		return f
	}
	ans, err := g.port.Held(ctx, subject, q)
	if err == nil {
		err = f.load(ans, q)
	}
	if err != nil {
		g.log.ErrorContext(ctx, "access: entitlements read failed; failing closed", "subject", subject, "err", err)
		f = &Filter{g: g, unknown: true, err: fmt.Errorf("%w: %w", ErrUnavailable, err), tiers: map[string]bool{}}
		for t := range claimed {
			f.tiers[t] = true
		}
	}
	return f
}

// claimed is the declared tiers among the Claims shortcut's names.
func (g *Gate) claimed(ctx context.Context) map[string]bool {
	if g.claims == nil {
		return nil
	}
	out := map[string]bool{}
	for _, c := range g.claims(ctx) {
		if g.tierSet[c] {
			out[c] = true
		}
	}
	return out
}

// Decision is one item's verdict.
type Decision struct {
	Allowed bool
	// Unknown: the billing read failed, so the item is denied and nothing is
	// offered (the viewer may already own it).
	Unknown bool
	// Sell is what a paywall offers when not allowed (Rule.Sells).
	Sell []Key
	// Requires is the key a buyer must hold first (MembersPPV without the
	// membership); nil when held or not needed.
	Requires *Key
}

// Decide decides items for actor in order. It answers from the request's
// memoized Filter and keys where they suffice and reads every other key in
// one billing read (at most Own, Members and the tiers per item). A
// failed read denies the items it would have decided (Unknown) and returns
// them with an error wrapping ErrUnavailable; an invalid item (another tenant,
// a versioned ref, an invalid rule or undeclared tier) fails the call.
func (g *Gate) Decide(ctx context.Context, actor Actor, items []Item) ([]Decision, error) {
	plans, err := g.plan(items)
	if err != nil {
		return nil, err
	}
	return g.decide(ctx, actor, nil, plans)
}

// Settle completes f's verdicts for a page: items f decides keep its answer,
// the rest (an incomplete keyspace's candidates) are decided in one exact
// read. keep[i] reports items[i] accessible; short reports a dropped item, so
// a hide-mode page may hold fewer rows than asked. It costs no read when f
// decides every item, so call it on every hide-mode page. A failed read drops
// the undecided items and returns an error wrapping ErrUnavailable.
func (g *Gate) Settle(ctx context.Context, actor Actor, f *Filter, items []Item) (keep []bool, short bool, err error) {
	if f == nil || f.g != g {
		return nil, false, errors.New("access: Settle needs a Filter of this gate")
	}
	plans, err := g.plan(items)
	if err != nil {
		return nil, false, err
	}
	keep = make([]bool, len(items))
	var pending []itemPlan
	var at []int
	for i, it := range items {
		if allowed, known := f.Allows(it); known {
			keep[i] = allowed
		} else {
			pending, at = append(pending, plans[i]), append(at, i)
		}
	}
	if len(pending) > 0 {
		var ds []Decision
		ds, err = g.decide(ctx, actor, f, pending)
		for j, d := range ds {
			keep[at[j]] = d.Allowed
		}
	}
	return keep, slices.Contains(keep, false), err
}

type itemPlan struct {
	rule Rule
	keys ruleKeys
}

func (g *Gate) plan(items []Item) ([]itemPlan, error) {
	out := make([]itemPlan, len(items))
	for i, it := range items {
		if it.Ref.TenantID != g.tenant {
			return nil, fmt.Errorf("access: item %s is not of tenant %q", it.Ref, g.tenant)
		}
		k, err := it.Rule.keys(it.Ref)
		if err != nil {
			return nil, err
		}
		if (it.Rule.Level == MembersLevel || it.Rule.Level == MembersPPV) && it.Rule.Scope.TenantID != g.tenant {
			return nil, fmt.Errorf("%w: %s: scope %s is not of tenant %q", ErrRule, it.Ref, it.Rule.Scope, g.tenant)
		}
		for _, t := range k.tiers {
			if !g.tierSet[t.tier] {
				return nil, fmt.Errorf("%w: %s: tier %q is not declared in GateConfig.Tiers", ErrRule, it.Ref, t.tier)
			}
		}
		out[i] = itemPlan{rule: it.Rule, keys: k}
	}
	return out, nil
}

// knowledge answers keys from what a request already knows.
type knowledge struct {
	anonymous bool
	claimed   map[string]bool
	filter    *Filter
	memo      *memoEntry
	read      map[string]bool
}

func (kn *knowledge) lookup(k Key) (held, known bool) {
	if kn.anonymous {
		return false, true
	}
	switch k.kind {
	case KeyTier:
		if kn.claimed[k.tier] {
			return true, true
		}
		if kn.filter != nil {
			if h, ok := kn.filter.tier(k.tier); ok {
				return h, true
			}
		}
	case KeyOwn, KeyMembers:
		if kn.filter != nil {
			prefix := OwnPrefix(k.ref.TenantID, k.ref.ContentKind)
			if k.kind == KeyMembers {
				prefix = MembersPrefix(k.ref.TenantID, k.ref.ContentKind)
			}
			if h, ok := kn.filter.has(prefix, k.ref.ContentID); ok {
				return h, true
			}
		}
	}
	s := k.String()
	if h, ok := kn.read[s]; ok {
		return h, true
	}
	return kn.memo.key(s)
}

func (g *Gate) decide(ctx context.Context, actor Actor, f *Filter, plans []itemPlan) ([]Decision, error) {
	kn := &knowledge{anonymous: anonymous(actor)}
	if !kn.anonymous {
		kn.claimed = g.claimed(ctx)
		kn.memo = memoFor(ctx, g, actor.ID)
		kn.filter = f
		if kn.filter == nil {
			kn.filter = kn.memo.loadFilter()
		}
	}
	out := make([]Decision, len(plans))
	need := map[string]bool{}
	for i, p := range plans {
		var unknown []Key
		out[i], unknown = verdict(p, kn)
		for _, k := range unknown {
			need[k.String()] = true
		}
	}
	if len(need) == 0 {
		return out, nil
	}
	keys := make([]string, 0, len(need))
	for k := range need {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	ans, err := g.port.Held(ctx, actor.ID, Query{Keys: keys})
	if err != nil {
		g.log.ErrorContext(ctx, "access: entitlements read failed; failing closed", "subject", actor.ID, "err", err)
		return out, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	kn.read = make(map[string]bool, len(keys))
	for _, k := range keys {
		kn.read[k] = ans.Keys[k]
	}
	kn.memo.storeKeys(kn.read)
	for i, p := range plans {
		out[i], _ = verdict(p, kn)
	}
	return out, nil
}

// verdict decides p from what kn knows; an undecided item is Unknown and
// lists the keys that would decide it.
func verdict(p itemPlan, kn *knowledge) (Decision, []Key) {
	if p.rule.Level == Public {
		return Decision{Allowed: true}, nil
	}
	var unknown []Key
	ask := func(k Key) bool {
		h, ok := kn.lookup(k)
		if !ok {
			unknown = append(unknown, k)
		}
		return h
	}
	if ask(p.keys.own) {
		return Decision{Allowed: true}, nil
	}
	member := false
	switch p.rule.Level {
	case TierLevel:
		for _, t := range p.keys.tiers {
			if ask(t) {
				return Decision{Allowed: true}, nil
			}
		}
	case MembersLevel:
		if ask(p.keys.members) {
			return Decision{Allowed: true}, nil
		}
	case MembersPPV:
		member = ask(p.keys.members)
	}
	if len(unknown) > 0 {
		return Decision{Unknown: true}, unknown
	}
	sell, requires, _ := p.rule.Sells(p.keys.own.ref.Ref())
	if member {
		requires = nil
	}
	return Decision{Sell: sell, Requires: requires}, nil
}

func anonymous(a Actor) bool { return a.Anonymous || a.ID == "" }

// Fact is the host's knowledge of one item for one viewer.
type Fact struct {
	// Ref is the canonical reference (see Resolution.Ref); zero keeps the
	// requested one. Access is decided on its work.
	Ref      contentref.ContentRef
	Visible  bool   // published and not deleted
	Editor   bool   // may edit it (its creator, staff)
	Bypass   bool   // host policy grants access regardless of the rule: staff, an operator, a channel reader
	Withheld bool   // not accessible to anyone yet (unreleased, held)
	Owner    string // see Resolution.Owner
	Rule     Rule
}

// Facts answers facts for a batch of refs in one query; an omitted ref denies.
type Facts interface {
	Facts(ctx context.Context, refs []contentref.ContentRef, actor Actor) (map[contentref.ContentKey]Fact, error)
}

// Resolver turns host facts into the ContentResolver content and media use:
// one Facts call and at most one Decide per batch. Accessible is Visible &&
// !Withheld && (Editor || Bypass || allowed). A failed billing read denies the
// items it decides; it does not fail the batch.
func (g *Gate) Resolver(f Facts) ContentResolver { return gateResolver{g: g, facts: f} }

type gateResolver struct {
	g     *Gate
	facts Facts
}

func (r gateResolver) Resolve(ctx context.Context, refs []contentref.ContentRef, actor Actor) (map[contentref.ContentKey]Resolution, error) {
	facts, err := r.facts.Facts(ctx, refs, actor)
	if err != nil {
		return nil, err
	}
	out := make(map[contentref.ContentKey]Resolution, len(refs))
	var items []Item
	var at []contentref.ContentKey
	for _, ref := range refs {
		key := ref.Key()
		fact, ok := facts[key]
		if !ok {
			continue
		}
		if _, seen := out[key]; seen {
			continue
		}
		res := Resolution{Ref: fact.Ref, Visible: fact.Visible, Editor: fact.Editor, Owner: fact.Owner}
		open := fact.Visible && !fact.Withheld
		res.Accessible = open && (fact.Editor || fact.Bypass)
		out[key] = res
		if open && !res.Accessible {
			work := fact.Ref
			if work == (contentref.ContentRef{}) {
				work = ref
			}
			items = append(items, Item{Ref: work.Content(), Rule: fact.Rule})
			at = append(at, key)
		}
	}
	if len(items) == 0 {
		return out, nil
	}
	ds, err := r.g.Decide(ctx, actor, items)
	if ds == nil {
		return nil, err
	}
	for j, d := range ds {
		res := out[at[j]]
		res.Accessible = d.Allowed
		out[at[j]] = res
	}
	return out, nil
}
