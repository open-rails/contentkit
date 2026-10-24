package entities

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/open-rails/contentkit/access"
)

type entityEntitlements struct {
	held        map[string]bool
	unavailable bool
}

func (b *entityEntitlements) Held(_ context.Context, _ string, q access.Query) (access.Answer, error) {
	if b.unavailable {
		return access.Answer{}, errors.New("billing unavailable")
	}
	out := access.Answer{Keys: map[string]bool{}, Held: map[string]access.HeldKeys{}}
	for _, k := range q.Keys {
		out.Keys[k] = b.held[k]
	}
	for _, prefix := range q.Prefixes {
		h := access.HeldKeys{}
		for k, v := range b.held {
			if v && strings.HasPrefix(k, prefix) {
				h.Keys = append(h.Keys, k)
			}
		}
		out.Held[prefix] = h
	}
	return out, nil
}

func TestEntityStandingUsesEntitlementGateWithoutSaleRecords(t *testing.T) {
	f := newFixture(t)
	k := f.s.cfg.EntityKinds["artist"]
	k.Entitled = true
	f.s.cfg.EntityKinds["artist"] = k
	made := f.create(EntityInput{Kind: "artist", Slug: "one"}, EntityInput{Kind: "artist", Slug: "two"})
	key, _ := access.EntityKey(tenant, string(made[0].ID))
	b := &entityEntitlements{held: map[string]bool{key.String(): true}}
	gate, err := access.NewGate(access.GateConfig{Tenant: tenant, Kinds: f.s.cfg, Billing: b})
	if err != nil {
		t.Fatal(err)
	}
	f.s.gate = gate
	// Direct reading as a member uses only the held entitlement, without any
	// ContentKit sale or price record.
	reader := *f.s
	reader.as = &writer{actor: access.Actor{ID: "reader"}}
	page, err := reader.List(f.ctx, ListOptions{Kind: "artist"})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range page.Entities {
		if it.Access == nil || it.Access.Unknown || it.Access.Member != (it.ID == made[0].ID) {
			t.Fatalf("standing=%+v", it)
		}
	}
	b.unavailable = true
	page, err = reader.List(f.ctx, ListOptions{Kind: "artist"})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range page.Entities {
		if it.Access == nil || !it.Access.Unknown || it.Access.Member {
			t.Fatalf("unavailable standing=%+v", it)
		}
	}
	// Merging these identities would silently change granted access.
	if _, err = f.s.Merge(f.ctx, []Merge{{From: made[0].ID, Into: made[1].ID}}); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("entitlement merge=%v", err)
	}
}
