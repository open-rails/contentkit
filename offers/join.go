package offers

import (
	"context"
	"errors"
	"fmt"

	"github.com/open-rails/contentkit/access"
)

// GroupJoin is a viewer's free membership of a group.
type GroupJoin struct {
	GroupID string     `json:"group_id"`
	Key     access.Key `json:"key"`
	Member  bool       `json:"member"`
}

// Join grants actor each group's key free, through Billing: a sellable group
// whose active offer is free (amount 0). Internal entities cannot be joined. A viewer already holding the key
// costs no grant. A group without a free offer is ErrNotJoinable, and so is
// one whose offer changes during the join: its grant is taken back.
func (s *Service) Join(ctx context.Context, actor access.Actor, groups []string) ([]GroupJoin, error) {
	targets, err := s.groupTargets(ctx, actor, groups)
	if err != nil {
		return nil, err
	}
	offers, err := s.free(ctx, targets)
	if err != nil {
		return nil, err
	}
	var unsynced []Target
	for _, o := range offers {
		if !o.Synced || o.ProductID == "" {
			unsynced = append(unsynced, o.Target)
		}
	}
	if len(unsynced) > 0 {
		if err := s.Sync(ctx, unsynced); err != nil {
			return nil, err
		}
		// A Put during the sync leaves its new terms unsynced.
		if offers, err = s.free(ctx, targets); err != nil {
			return nil, err
		}
		for _, o := range offers {
			if !o.Synced || o.ProductID == "" {
				return nil, fmt.Errorf("%w: group %s changed while joining", ErrNotJoinable, o.Target.ID)
			}
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
	var granted []int // offers index of each grant
	out := make([]GroupJoin, len(offers))
	for i, o := range offers {
		out[i] = GroupJoin{GroupID: o.Target.ID, Key: o.Key, Member: true}
		if !held[o.Target.ID] {
			grants = append(grants, access.Grant{Subject: actor.AccountID(), ProductID: o.ProductID})
			granted = append(granted, i)
		}
	}
	if len(grants) == 0 {
		return out, nil
	}
	gerr := s.billing.Grant(ctx, grants)
	// A Put or Withdraw may have committed since the read: take back the
	// grant of each offer no longer free on the product granted, even when
	// the grant's answer was lost, and even when the caller has gone.
	ctx = context.WithoutCancel(ctx)
	now, err := s.active(ctx, targets)
	var revoke []access.Grant
	for j, i := range granted {
		if err != nil || now[i].Terms.Amount != 0 || now[i].ProductID != grants[j].ProductID {
			revoke = append(revoke, grants[j])
		}
	}
	if len(revoke) > 0 {
		if rerr := s.billing.Revoke(ctx, revoke); rerr != nil {
			return nil, errors.Join(gerr, err, rerr)
		}
		if err == nil || errors.Is(err, ErrNotFound) {
			err = fmt.Errorf("%w: an offer changed while joining", ErrNotJoinable)
		}
	}
	if err := errors.Join(gerr, err); err != nil {
		return nil, err
	}
	return out, nil
}

// free reads targets' active offers, each of which must be free.
func (s *Service) free(ctx context.Context, targets []Target) ([]Offer, error) {
	offers, err := s.active(ctx, targets)
	if errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("%w: %v", ErrNotJoinable, err)
	}
	if err != nil {
		return nil, err
	}
	for _, o := range offers {
		if o.Terms.Amount != 0 {
			return nil, fmt.Errorf("%w: group %s", ErrNotJoinable, o.Target.ID)
		}
	}
	return offers, nil
}

// Leave takes back each group's free grant from actor; a bought or
// subscribed membership stays.
func (s *Service) Leave(ctx context.Context, actor access.Actor, groups []string) ([]GroupJoin, error) {
	targets, err := s.groupTargets(ctx, actor, groups)
	if err != nil {
		return nil, err
	}
	keys := make([]string, len(targets))
	out := make([]GroupJoin, len(targets))
	for i, t := range targets {
		k, err := s.key(t)
		if err != nil {
			return nil, err
		}
		keys[i], out[i] = k.String(), GroupJoin{GroupID: t.ID, Key: k}
	}
	products, err := s.billing.Products(ctx, keys)
	if err != nil {
		return nil, err
	}
	var grants []access.Grant
	for _, k := range keys {
		if p, ok := products[k]; ok {
			grants = append(grants, access.Grant{Subject: actor.AccountID(), ProductID: p.ID})
		}
	}
	if len(grants) == 0 {
		return out, nil
	}
	return out, s.billing.Revoke(ctx, grants)
}

func (s *Service) groupTargets(ctx context.Context, actor access.Actor, groups []string) ([]Target, error) {
	if !actor.Member() {
		return nil, ErrForbidden
	}
	targets := make([]Target, len(groups))
	for i, g := range groups {
		targets[i] = Target{Kind: GroupTarget, ID: g}
	}
	if err := s.resolve(ctx, s.pool, targets); err != nil {
		return nil, err
	}
	visible, err := s.publicGroups(ctx, groups)
	if err != nil {
		return nil, err
	}
	for _, id := range groups {
		if !visible[id] {
			return nil, ErrNotFound
		}
	}
	return targets, nil
}
