package access

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/kinds"
)

// Level is a rule's access level. The stored group/group_ppv spellings remain
// stable encodings for entity access, including already recorded releases.
type Level string

const (
	Public      Level = "public"    // anyone
	TierLevel   Level = "tier"      // holders of any of Rule.Tiers
	EntityLevel Level = "group"     // holders of Rule.Entity's key: members-only content
	PPV         Level = "ppv"       // buyers of the content or version
	EntityPPV   Level = "group_ppv" // buyers; buying needs Rule.Entity's key first
)

// ErrRule is a rule outside the grammar or naming an undeclared tier.
var ErrRule = errors.New("access: invalid rule")

// Rule is the access rule stored as JSON on content (content_items.access), on
// a version as its override (content_versions.access) and on a sellable
// entity (content_entities.access). Whatever the level, a version also opens to
// holders of its version key, its content key and the key of any entity
// holding it.
type Rule struct {
	Level  Level    `json:"level"`
	Tiers  []string `json:"tiers,omitempty"`    // TierLevel: any of these configured tiers
	Entity string   `json:"group_id,omitempty"` // EntityLevel, EntityPPV; retained JSON field
	// Preview is the part a viewer who does not open it may still read.
	Preview *Preview `json:"preview,omitempty"`
	// Releases add audiences at their release time; earlier rights remain.
	Releases []Release `json:"releases,omitempty"`
}

// Preview is the free part of locked content (kinds.Preview).
type Preview = kinds.Preview

// UnmarshalJSON refuses unknown fields; Validate checks the rest.
func (r *Rule) UnmarshalJSON(b []byte) error {
	type plain Rule
	var p plain
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return fmt.Errorf("%w: %w", ErrRule, err)
	}
	*r = Rule(p)
	return nil
}

// Validate checks r against cfg: a known level with exactly its own fields,
// configured tiers, a valid entity id, and options only on a rule that locks.
func (r Rule) Validate(cfg kinds.Config) error {
	switch r.Level {
	case Public, PPV:
		if len(r.Tiers) > 0 || r.Entity != "" {
			return fmt.Errorf("%w: level %s takes no tiers or group_id", ErrRule, r.Level)
		}
	case TierLevel:
		if len(r.Tiers) == 0 || r.Entity != "" {
			return fmt.Errorf("%w: level tier needs tiers and no group_id", ErrRule)
		}
		for i, t := range r.Tiers {
			if !slices.Contains(cfg.Tiers, t) || slices.Contains(r.Tiers[:i], t) {
				return fmt.Errorf("%w: tier %q is not configured or repeated", ErrRule, t)
			}
		}
	case EntityLevel, EntityPPV:
		if len(r.Tiers) > 0 {
			return fmt.Errorf("%w: level %s takes no tiers", ErrRule, r.Level)
		}
		if err := contentref.ValidateID(r.Entity); err != nil {
			return fmt.Errorf("%w: level %s group_id: %w", ErrRule, r.Level, err)
		}
	default:
		return fmt.Errorf("%w: unknown level %q", ErrRule, r.Level)
	}
	if r.Level == Public && (r.Preview != nil || len(r.Releases) != 0) {
		return fmt.Errorf("%w: a public rule takes no preview or releases", ErrRule)
	}
	if p := r.Preview; p != nil {
		if err := p.Validate(); err != nil {
			return fmt.Errorf("%w: %v", ErrRule, err)
		}
	}
	return r.validateReleases(cfg)
}

// Free reports whether r opens to anyone at now.
func (r Rule) Free(now time.Time) bool {
	if r.Level == Public {
		return true
	}
	for _, release := range r.Releases {
		if release.Level == Public && !now.Before(release.At) {
			return true
		}
	}
	return false
}

// Effective is a version's rule: its override, else its content's.
func Effective(content Rule, override *Rule) Rule {
	if override != nil {
		return *override
	}
	return content
}

// Previewed is r with its content kind's locked preview when r locks and
// names none of its own.
func (r Rule) Previewed(k kinds.Kind) Rule {
	if p := k.LockedView().Preview; r.Preview == nil && r.Level != Public && p != nil {
		p := *p
		r.Preview = &p
	}
	return r
}

// Facet is a rule as the content_facets listing projection stores it:
// access_level, access_tiers, access_group_id and access_releases.
type Facet struct {
	Level    Level
	Tiers    []string
	EntityID *string
	Releases []Release
}

// Facet projects r onto content_facets' access columns.
func (r Rule) Facet() Facet {
	f := Facet{Level: r.Level, Tiers: slices.Clone(r.Tiers), Releases: slices.Clone(r.Releases)}
	if f.Releases == nil {
		f.Releases = []Release{}
	}
	if f.Tiers == nil {
		f.Tiers = []string{}
	}
	if r.Entity != "" {
		g := r.Entity
		f.EntityID = &g
	}
	return f
}
