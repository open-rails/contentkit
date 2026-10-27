package kinds

import (
	"fmt"
	"slices"
	"time"
)

// PopularityBucket is one host-declared ranking, prepared without reader traffic.
// Select exactly one of Days, From/Until, or All. Dates are UTC [From, Until).
type PopularityBucket struct {
	ID           string            `json:"id" yaml:"id"`
	Label        string            `json:"label,omitempty" yaml:"label"`
	Days         int               `json:"days,omitempty" yaml:"days"`
	From         string            `json:"from,omitempty" yaml:"from"`
	Until        string            `json:"until,omitempty" yaml:"until"`
	All          bool              `json:"all,omitempty" yaml:"all"`
	Scopes       []PopularityScope `json:"scopes,omitempty" yaml:"scopes"`
	RefreshEvery time.Duration     `json:"refresh_every,omitempty" yaml:"refresh_every"`
}

// PopularityScope selects the activity included in a ranking. An empty field
// includes every domain/language; omitted Scopes means one global ranking.
type PopularityScope struct {
	Domain   string `json:"domain,omitempty" yaml:"domain"`
	Language string `json:"language,omitempty" yaml:"language"`
}

// ActivityScopes returns only declared scopes, including the implicit global one.
func (b PopularityBucket) ActivityScopes() []PopularityScope {
	if len(b.Scopes) == 0 {
		return []PopularityScope{{}}
	}
	return b.Scopes
}

// Every is the configured refresh interval, one hour by default.
func (b PopularityBucket) Every() time.Duration {
	if b.RefreshEvery == 0 {
		return time.Hour
	}
	return b.RefreshEvery
}

// Window resolves the bucket's actual UTC bounds; zero bounds mean all time.
func (b PopularityBucket) Window(now time.Time) (from, until time.Time) {
	if b.Days > 0 {
		now = now.UTC()
		today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		return today.AddDate(0, 0, 1-b.Days), today.AddDate(0, 0, 1)
	}
	if b.From != "" {
		from, _ = time.Parse(time.DateOnly, b.From)
		until, _ = time.Parse(time.DateOnly, b.Until)
	}
	return
}

// Scope chooses the most specific declared scope matching the reader.
func (b PopularityBucket) Scope(domain, language string) (PopularityScope, bool) {
	var selected PopularityScope
	best := -1
	for _, scope := range b.ActivityScopes() {
		if scope.Domain != "" && scope.Domain != domain || scope.Language != "" && scope.Language != language {
			continue
		}
		specificity := 0
		if scope.Domain != "" {
			specificity++
		}
		if scope.Language != "" {
			specificity++
		}
		if specificity > best {
			selected, best = scope, specificity
		}
	}
	return selected, best >= 0
}

func (c Config) validatePopularity(kind string, buckets []PopularityBucket) error {
	seen := map[string]bool{}
	for _, b := range buckets {
		invalid := func(reason string) error {
			return fmt.Errorf("%w: kind %s popularity bucket %q: %s", ErrInvalid, kind, b.ID, reason)
		}
		if !routeRE.MatchString(b.ID) || seen[b.ID] {
			return invalid("id must be a unique route name")
		}
		seen[b.ID] = true
		modes := 0
		if b.Days != 0 {
			if b.Days < 1 {
				return invalid("days must be positive")
			}
			modes++
		}
		if b.From != "" || b.Until != "" {
			from, first := time.Parse(time.DateOnly, b.From)
			until, last := time.Parse(time.DateOnly, b.Until)
			if first != nil || last != nil || !from.Before(until) {
				return invalid("from and until must be increasing UTC dates")
			}
			modes++
		}
		if b.All {
			modes++
		}
		if modes != 1 || b.RefreshEvery < 0 {
			return invalid("choose one window and a nonnegative refresh_every")
		}
		scopes := map[PopularityScope]bool{}
		for _, scope := range b.ActivityScopes() {
			if scopes[scope] {
				return invalid("scope is repeated")
			}
			scopes[scope] = true
			if scope.Language != "" && !slices.Contains(c.Languages, scope.Language) {
				return invalid("scope language is not configured")
			}
			if scope.Domain != "" && !slices.ContainsFunc(c.ContentFilters.Domains, func(d Domain) bool { return d.Name == scope.Domain }) {
				return invalid("scope domain is not configured")
			}
		}
	}
	return nil
}
