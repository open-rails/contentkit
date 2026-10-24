package access

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/open-rails/contentkit/kinds"
)

// ErrUnavailable wraps a failed billing read. The result it comes with is the
// fail-closed one: only free rules and claimed tiers open, nothing is offered.
var ErrUnavailable = errors.New("access: entitlements unavailable")

// GateConfig configures a Gate from the engine's config.
type GateConfig struct {
	// Tenant is the tenant of every key: [a-z0-9_-]{1,64}.
	Tenant string
	// Kinds supplies the configured tiers, the only bare keys ContentKit
	// interprets, and the token claims answering them without a billing read.
	Kinds kinds.Config
	// Billing answers held keys; nil holds none for anyone, so only free
	// rules and claimed tiers open.
	Billing Billing
	Logger  *slog.Logger
	// Now is the application clock used for audience-release decisions.
	// Nil uses time.Now, matching ordinary production behavior.
	Now func() time.Time
}

// Gate decides access to versions from one billing read per viewer and
// request (see WithMemo).
type Gate struct {
	billing   Billing
	tenant    string
	tiers     []string
	tierSet   map[string]bool
	claimable map[string]bool
	prefixes  []string
	log       *slog.Logger
	anon      *Filter
	now       func() time.Time
}

// NewGate validates c.
func NewGate(c GateConfig) (*Gate, error) {
	if !ValidTenant(c.Tenant) {
		return nil, fmt.Errorf("access: GateConfig.Tenant %q must match [a-z0-9_-]{1,64}", c.Tenant)
	}
	g := &Gate{billing: c.Billing, tenant: c.Tenant, tierSet: map[string]bool{}, claimable: map[string]bool{},
		prefixes: []string{ContentPrefix(c.Tenant), EntityPrefix(c.Tenant)}, log: c.Logger, now: c.Now}
	if g.now == nil {
		g.now = time.Now
	}
	if g.log == nil {
		g.log = slog.Default()
	}
	for _, t := range c.Kinds.Tiers {
		if !validTier(t) || g.tierSet[t] {
			return nil, fmt.Errorf("access: tier %q is invalid or repeated", t)
		}
		g.tierSet[t] = true
		g.tiers = append(g.tiers, t)
	}
	slices.Sort(g.tiers)
	for _, t := range c.Kinds.TokenClaims {
		if !g.tierSet[t] {
			return nil, fmt.Errorf("access: token claim %q is not a tier", t)
		}
		g.claimable[t] = true
	}
	g.anon = &Filter{g: g, anonymous: true, contentsComplete: true, entitiesComplete: true}
	return g, nil
}

// Tenant is the gate's tenant.
func (g *Gate) Tenant() string { return g.tenant }

// Tiers returns the configured tiers, sorted.
func (g *Gate) Tiers() []string { return slices.Clone(g.tiers) }

// Item is one version to decide (or a content item as a whole, Version "").
type Item struct {
	Content string
	Version string
	// Rule is the version's effective rule (Effective).
	Rule Rule
	// Entities are the entities holding the content or version (content_facets
	// entity_ids): a held key of any of them opens it.
	Entities []string
}

// Decision is one item's verdict.
type Decision struct {
	Allowed bool
	// Unknown: the billing read failed, so the item is denied and nothing is
	// offered (the viewer may already hold it).
	Unknown bool
	// Sell are the keys whose purchase opens it, the rule's own first (its
	// tiers or entity), then the version's and the content's;
	// The host resolves these keys through its billing provider.
	Sell []Key
	// Requires is the key a buyer must hold first (EntityPPV), nil when held.
	Requires *Key
	// Preview is the part a viewer may read while it is locked.
	Preview *Preview
	// Releases describe upcoming additional audiences.
	Releases []Release
}

// Filter reads the viewer's access in one billing read: the configured tiers
// a token claim does not answer and the tenant's content and entity
// keyspaces. Within WithMemo it is read once per request. An anonymous viewer
// costs no read. On a failed read Filter returns the fail-closed filter
// (Unknown) and an error wrapping ErrUnavailable: serve that filter or
// answer 503.
func (g *Gate) Filter(ctx context.Context, actor Actor) (*Filter, error) {
	if anonymous(actor) {
		return g.anon, nil
	}
	e := memoFor(ctx, g, accountSubject(actor))
	if f := e.loadFilter(); f != nil {
		return f, f.err
	}
	f := g.read(ctx, actor)
	e.storeFilter(f)
	return f, f.err
}

func (g *Gate) read(ctx context.Context, actor Actor) *Filter {
	claimed := g.claimed(actor)
	f := &Filter{g: g, tiers: map[string]bool{}, contents: map[string]bool{}, versions: map[string]string{}, entities: map[string]bool{},
		contentsComplete: true, entitiesComplete: true}
	q := Query{Prefixes: g.prefixes, Limit: HeldLimit}
	for _, t := range g.tiers {
		if claimed[t] {
			f.tiers[t] = true
		} else {
			q.Keys = append(q.Keys, t)
		}
	}
	if g.billing == nil {
		for _, t := range q.Keys {
			f.tiers[t] = false
		}
		return f
	}
	ans, err := g.billing.Held(ctx, accountSubject(actor), q)
	if err == nil {
		err = f.load(ans, q)
	}
	if err != nil {
		g.log.ErrorContext(ctx, "access: entitlements read failed; failing closed", "subject", actor.ID, "err", err)
		f = &Filter{g: g, unknown: true, err: fmt.Errorf("%w: %w", ErrUnavailable, err), tiers: map[string]bool{},
			contentsComplete: true, entitiesComplete: true}
		for t := range claimed {
			f.tiers[t] = true
		}
	}
	return f
}

// claimed is the claimable tiers among the actor's token claims.
func (g *Gate) claimed(actor Actor) map[string]bool {
	out := map[string]bool{}
	for _, c := range actor.claims() {
		if g.claimable[c] {
			out[c] = true
		}
	}
	return out
}

// Decide decides items for actor in order. It answers from the request's
// memoized Filter and keys where they suffice and reads every other key in
// one billing read. A failed read denies what it would have decided
// (Unknown) and returns an error wrapping ErrUnavailable. An item with an
// invalid id or rule is denied with nothing offered and logged.
func (g *Gate) Decide(ctx context.Context, actor Actor, items []Item) ([]Decision, error) {
	return g.decide(ctx, actor, nil, g.plan(ctx, items))
}

// Holds reports, per key in order, whether actor holds it: from the
// request's memoized Filter and keys where they suffice, the rest in one
// billing read. An anonymous actor holds nothing and costs no read. A failed
// read answers false for what it would have decided and returns an error
// wrapping ErrUnavailable.
func (g *Gate) Holds(ctx context.Context, actor Actor, keys []Key) ([]bool, error) {
	out := make([]bool, len(keys))
	if anonymous(actor) {
		return out, nil
	}
	kn := &knowledge{claimed: g.claimed(actor), memo: memoFor(ctx, g, accountSubject(actor))}
	kn.filter = kn.memo.loadFilter()
	need := map[string]bool{}
	for i, k := range keys {
		held, known := kn.lookup(k)
		out[i] = held
		if !known {
			need[k.String()] = true
		}
	}
	if len(need) == 0 {
		return out, nil
	}
	ask := slices.Sorted(maps.Keys(need))
	var ans Answer
	var err error
	if g.billing != nil {
		ans, err = g.billing.Held(ctx, accountSubject(actor), Query{Keys: ask})
	}
	if err != nil {
		g.log.ErrorContext(ctx, "access: entitlements read failed; failing closed", "subject", actor.ID, "err", err)
		return out, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	read := make(map[string]bool, len(ask))
	for _, k := range ask {
		read[k] = ans.Keys[k]
	}
	kn.memo.storeKeys(read)
	for i, k := range keys {
		if h, ok := read[k.String()]; ok {
			out[i] = h
		}
	}
	return out, nil
}

// Settle completes f's verdicts for a hide-mode page: items f decides keep
// its answer, the rest (candidates of a truncated keyspace) are decided in
// one exact read. keep[i] reports items[i] open; short reports a dropped
// item, so the page may hold fewer rows than asked. It costs no read when f
// decides every item. A failed read drops the undecided items and returns an
// error wrapping ErrUnavailable.
func (g *Gate) Settle(ctx context.Context, actor Actor, f *Filter, items []Item) (keep []bool, short bool, err error) {
	if f == nil || f.g != g {
		return nil, false, errors.New("access: Settle needs a Filter of this gate")
	}
	plans := g.plan(ctx, items)
	keep = make([]bool, len(items))
	var pending []itemPlan
	var at []int
	now := g.now()
	for i, p := range plans {
		d, unknown := verdict(p, &knowledge{filter: f}, now, false)
		if len(unknown) == 0 {
			keep[i] = d.Allowed
		} else {
			pending, at = append(pending, p), append(at, i)
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

type releasePlan struct {
	at   time.Time
	keys []Key
}

type itemPlan struct {
	invalid  bool
	rule     Rule
	version  *Key
	content  Key
	entities []Key // Item.Entities, and the rule's entity for EntityLevel
	tiers    []Key // TierLevel: the configured ones among the rule's
	requires *Key  // EntityPPV
	releases []releasePlan
}

func (g *Gate) plan(ctx context.Context, items []Item) []itemPlan {
	out := make([]itemPlan, len(items))
	for i, it := range items {
		p, err := g.planOne(it)
		if err != nil {
			g.log.ErrorContext(ctx, "access: denying an invalid item", "content", it.Content, "version", it.Version, "err", err)
			p = itemPlan{invalid: true}
		}
		out[i] = p
	}
	return out
}

func (g *Gate) planOne(it Item) (itemPlan, error) {
	p := itemPlan{rule: it.Rule}
	if err := it.Rule.validateReleases(kinds.Config{Tiers: g.tiers}); err != nil {
		return p, err
	}
	var err error
	if p.content, err = ContentKey(g.tenant, it.Content); err != nil {
		return p, err
	}
	if it.Version != "" {
		v, err := VersionKey(g.tenant, it.Content, it.Version)
		if err != nil {
			return p, err
		}
		p.version = &v
	}
	for _, id := range it.Entities {
		k, err := EntityKey(g.tenant, id)
		if err != nil {
			return p, err
		}
		p.entities = append(p.entities, k)
	}
	switch it.Rule.Level {
	case Public, PPV:
	case TierLevel:
		for _, t := range it.Rule.Tiers {
			if g.tierSet[t] {
				p.tiers = append(p.tiers, Key{kind: KeyTier, id: t})
			}
		}
	case EntityLevel, EntityPPV:
		k, err := EntityKey(g.tenant, it.Rule.Entity)
		if err != nil {
			return p, fmt.Errorf("%w: %w", ErrRule, err)
		}
		if it.Rule.Level == EntityLevel {
			p.entities = append(p.entities, k)
		} else {
			p.requires = &k
		}
	default:
		return p, fmt.Errorf("%w: unknown level %q", ErrRule, it.Rule.Level)
	}
	for _, release := range it.Rule.Releases {
		rp := releasePlan{at: release.At}
		switch release.Level {
		case Public:
		case TierLevel:
			for _, tier := range release.Tiers {
				if !g.tierSet[tier] {
					return p, ErrRule
				}
				rp.keys = append(rp.keys, Key{kind: KeyTier, id: tier})
			}
		case EntityLevel:
			key, err := EntityKey(g.tenant, release.Entity)
			if err != nil {
				return p, err
			}
			rp.keys = append(rp.keys, key)
		default:
			return p, ErrRule
		}
		p.releases = append(p.releases, rp)
	}
	return p, nil
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
	if k.kind == KeyTier && kn.claimed[k.id] {
		return true, true
	}
	if kn.filter != nil {
		if h, ok := kn.filter.lookup(k); ok {
			return h, true
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
		kn.claimed = g.claimed(actor)
		kn.memo = memoFor(ctx, g, accountSubject(actor))
		kn.filter = f
		if kn.filter == nil {
			kn.filter = kn.memo.loadFilter()
		}
	}
	now := g.now()
	out := make([]Decision, len(plans))
	need := map[string]bool{}
	for i, p := range plans {
		var unknown []Key
		out[i], unknown = verdict(p, kn, now, true)
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
	var ans Answer
	var err error
	if g.billing != nil {
		ans, err = g.billing.Held(ctx, accountSubject(actor), Query{Keys: keys})
	}
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
		out[i], _ = verdict(p, kn, now, true)
	}
	return out, nil
}

// verdict decides p from what kn knows; an undecided item is Unknown and
// lists the keys that would decide it. Without paywall a denied item's
// Requires is not asked.
func verdict(p itemPlan, kn *knowledge, now time.Time, paywall bool) (Decision, []Key) {
	if p.invalid {
		return Decision{}, nil
	}
	if p.rule.Free(now) {
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
	opens := slices.Concat(p.tiers, p.entities, []Key{p.content})
	if p.version != nil {
		opens = append(opens, *p.version)
	}
	for _, release := range p.releases {
		if !now.Before(release.at) {
			opens = append(opens, release.keys...)
		}
	}
	for _, k := range opens {
		if ask(k) {
			return Decision{Allowed: true}, nil
		}
	}
	if len(unknown) > 0 {
		return Decision{Unknown: true}, unknown
	}
	requiresHeld := p.requires == nil || !paywall || ask(*p.requires)
	if len(unknown) > 0 {
		return Decision{Unknown: true}, unknown
	}
	d := Decision{Preview: p.rule.Preview}
	switch p.rule.Level {
	case TierLevel:
		d.Sell = slices.Clone(p.tiers)
	case EntityLevel:
		d.Sell = []Key{p.entities[len(p.entities)-1]}
	}
	if p.version != nil {
		d.Sell = append(d.Sell, *p.version)
	}
	d.Sell = append(d.Sell, p.content)
	if !requiresHeld {
		d.Requires = p.requires
	}
	d.Releases = slices.Clone(p.rule.Releases)
	return d, nil
}

func anonymous(a Actor) bool {
	return a.Anonymous || a.ID == "" || a.Account != nil && a.Account.ID == ""
}

// accountSubject retains legacy application behavior while using the verified
// tenant membership for scoped audience entitlements.
func accountSubject(a Actor) string {
	if a.Account != nil {
		return a.Account.ID
	}
	return a.ID
}
