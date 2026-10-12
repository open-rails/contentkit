package offers

import (
	"context"
	"errors"
	"fmt"

	"github.com/open-rails/contentkit/access"
)

// Membership is a viewer's free membership of a group.
type Membership struct {
	GroupID string     `json:"group_id"`
	Key     access.Key `json:"key"`
	Member  bool       `json:"member"`
}

// Join grants actor each group's key free, through Billing: a sellable group
// whose active offer is free (amount 0). A viewer already holding the key
// costs no grant. A group without a free offer is ErrConflict.
func (s *Service) Join(ctx context.Context, actor access.Actor, groups []string) ([]Membership, error) {
	targets, err := s.groupTargets(ctx, actor, groups)
	if err != nil {
		return nil, err
	}
	offers, err := s.active(ctx, targets)
	if errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("%w: a group is not free to join: %v", ErrConflict, err)
	}
	if err != nil {
		return nil, err
	}
	var unsynced []Target
	for _, o := range offers {
		if o.Terms.Amount != 0 {
			return nil, fmt.Errorf("%w: group %s is not free to join", ErrConflict, o.Target.ID)
		}
		if !o.Synced || o.ProductID == "" {
			unsynced = append(unsynced, o.Target)
		}
	}
	if len(unsynced) > 0 {
		if err := s.Sync(ctx, unsynced); err != nil {
			return nil, err
		}
		if offers, err = s.active(ctx, targets); err != nil {
			return nil, err
		}
	}
	f, err := s.gate.Filter(ctx, actor)
	if err != nil {
		return nil, err
	}
	held := map[string]bool{}
	for _, g := range f.Groups() {
		held[g] = true
	}
	var grants []access.Grant
	out := make([]Membership, len(offers))
	for i, o := range offers {
		out[i] = Membership{GroupID: o.Target.ID, Key: o.Key, Member: true}
		if !held[o.Target.ID] {
			grants = append(grants, access.Grant{Subject: actor.ID, ProductID: o.ProductID})
		}
	}
	if len(grants) == 0 {
		return out, nil
	}
	return out, s.billing.Grant(ctx, grants)
}

// Leave takes back each group's free grant from actor; a bought or
// subscribed membership stays.
func (s *Service) Leave(ctx context.Context, actor access.Actor, groups []string) ([]Membership, error) {
	targets, err := s.groupTargets(ctx, actor, groups)
	if err != nil {
		return nil, err
	}
	keys := make([]string, len(targets))
	out := make([]Membership, len(targets))
	for i, t := range targets {
		k, err := s.key(t)
		if err != nil {
			return nil, err
		}
		keys[i], out[i] = k.String(), Membership{GroupID: t.ID, Key: k}
	}
	products, err := s.billing.Products(ctx, keys)
	if err != nil {
		return nil, err
	}
	var grants []access.Grant
	for _, k := range keys {
		if p, ok := products[k]; ok {
			grants = append(grants, access.Grant{Subject: actor.ID, ProductID: p.ID})
		}
	}
	if len(grants) == 0 {
		return out, nil
	}
	return out, s.billing.Revoke(ctx, grants)
}

func (s *Service) groupTargets(ctx context.Context, actor access.Actor, groups []string) ([]Target, error) {
	if !actor.Person() {
		return nil, ErrForbidden
	}
	targets := make([]Target, len(groups))
	for i, g := range groups {
		targets[i] = Target{Kind: GroupTarget, ID: g}
	}
	return targets, s.resolve(ctx, s.pool, targets)
}
