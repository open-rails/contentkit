package access

import (
	"errors"
	"fmt"

	"github.com/open-rails/contentkit/contentref"
)

// Level is an item's access level.
type Level string

const (
	Public       Level = "public"      // anyone
	TierLevel    Level = "tier"        // holders of any of Rule.Tiers
	MembersLevel Level = "members"     // members of Rule.Scope
	PPV          Level = "ppv"         // owners of the item
	MembersPPV   Level = "members_ppv" // owners; buying it requires membership of Rule.Scope
)

// ErrRule is a rule with an unknown level or a missing tier or scope.
var ErrRule = errors.New("access: invalid rule")

// Rule is an item's access rule, a host fact. Owning the item (Own) unlocks it
// at every level.
type Rule struct {
	Level Level
	Tiers []string              // TierLevel: any of these declared tiers
	Scope contentref.ContentRef // MembersLevel, MembersPPV: the scope's work reference
}

// Item is one work and its rule.
type Item struct {
	Ref  contentref.ContentRef
	Rule Rule
}

// Unlocks returns the keys any one of which unlocks item under r: Own(item)
// always, plus the rule's tiers or membership. A Public item needs none.
func (r Rule) Unlocks(item contentref.ContentRef) ([]Key, error) {
	k, err := r.keys(item)
	if err != nil {
		return nil, err
	}
	out := append([]Key{k.own}, k.tiers...)
	if r.Level == MembersLevel {
		out = append(out, k.members)
	}
	return out, nil
}

// Sells returns what a paywall offers for item under r: the tiers, the
// membership or the item itself. requires is the key a buyer must hold first
// (MembersPPV: the membership). Public sells nothing.
func (r Rule) Sells(item contentref.ContentRef) (sell []Key, requires *Key, err error) {
	k, err := r.keys(item)
	if err != nil {
		return nil, nil, err
	}
	switch r.Level {
	case TierLevel:
		return k.tiers, nil, nil
	case MembersLevel:
		return []Key{k.members}, nil, nil
	case PPV:
		return []Key{k.own}, nil, nil
	case MembersPPV:
		m := k.members
		return []Key{k.own}, &m, nil
	}
	return nil, nil, nil
}

type ruleKeys struct {
	own     Key
	tiers   []Key
	members Key // MembersLevel, MembersPPV
}

func (r Rule) keys(item contentref.ContentRef) (ruleKeys, error) {
	var k ruleKeys
	var err error
	if k.own, err = MakeOwn(item); err != nil {
		return k, err
	}
	switch r.Level {
	case Public, PPV:
	case TierLevel:
		if len(r.Tiers) == 0 {
			return k, fmt.Errorf("%w: %s: level tier names no tier", ErrRule, item)
		}
		for _, t := range r.Tiers {
			tk, err := MakeTier(t)
			if err != nil {
				return k, fmt.Errorf("%w: %s: %w", ErrRule, item, err)
			}
			k.tiers = append(k.tiers, tk)
		}
	case MembersLevel, MembersPPV:
		if k.members, err = MakeMembers(r.Scope); err != nil {
			return k, fmt.Errorf("%w: %s: level %s scope: %w", ErrRule, item, r.Level, err)
		}
	default:
		return k, fmt.Errorf("%w: %s: unknown level %q", ErrRule, item, r.Level)
	}
	return k, nil
}
