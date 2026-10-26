package critical_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/open-rails/authkit/iam"
	ckauth "github.com/open-rails/contentkit/adapters/authkit"
	"github.com/open-rails/contentkit/channels"
	"github.com/open-rails/contentkit/entities"
)

// The wrapper delays only the network-equivalent boundary; every mutation and
// grant still goes through the real AuthKit client and PostgreSQL.
type delayedChannelRetirement struct {
	entities.Scopes
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	loseAck bool
}

func (s *delayedChannelRetirement) Retire(ctx context.Context, ids []string) error {
	s.once.Do(func() { close(s.entered) })
	select {
	case <-s.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := s.Scopes.Retire(ctx, ids); err != nil {
		return err
	}
	if s.loseAck {
		return errors.New("lost retirement acknowledgement after concurrent restore")
	}
	return nil
}

func TestChannelLateRetirementFailsClosed(t *testing.T) {
	for _, loseAck := range []bool{false, true} {
		name := "acknowledged"
		if loseAck {
			name = "lost_acknowledgement"
		}
		t.Run(name, func(t *testing.T) { testChannelLateRetirement(t, loseAck) })
	}
}

func testChannelLateRetirement(t *testing.T, loseAck bool) {
	w := newWorld(t)
	a := w.apps[0]
	_, _, channel := ownedChannel(t, w, a, "Retirement race")
	provider := &delayedChannelRetirement{Scopes: a.deps.Scopes, entered: make(chan struct{}), release: make(chan struct{}), loseAck: loseAck}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(provider.release) }) }
	t.Cleanup(release)
	store, err := channels.New(channels.Options{Pool: w.pool, Schema: w.schema, Tenant: a.cfg.Scope.AppID.String(), Model: a.cfg.Content, Scopes: provider})
	must(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	deleted := make(chan error, 1)
	go func() { deleted <- store.Delete(ctx, channel.ID) }()
	select {
	case <-provider.entered:
	case <-ctx.Done():
		t.Fatal("delete did not reach external retirement")
	}
	active := entities.StateActive
	restored, err := store.Update(ctx, channel.ID, channels.Update{State: &active})
	must(t, err)
	if restored.AuthScope == "" {
		t.Fatal("concurrent restore was not ready before late retirement")
	}
	release()
	select {
	case err := <-deleted:
		if err == nil || !loseAck && !errors.Is(err, entities.ErrConflict) {
			t.Fatalf("stale retirement reported completion: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("stale retirement did not finish")
	}
	pending, err := store.ManagementGet(ctx, channel.ID, "")
	must(t, err)
	if pending.State != entities.StateActive || pending.AuthScope != "" {
		t.Fatalf("late retirement left false readiness: %+v", pending)
	}
	restored, err = store.Update(ctx, channel.ID, channels.Update{State: &active})
	must(t, err)
	if restored.ID != channel.ID || restored.AuthScope != channel.AuthScope {
		t.Fatalf("retry changed channel/group identity: %+v", restored)
	}
	groupID, err := ckauth.ChannelScopeID(*a.cfg.Scope, restored.AuthScope)
	must(t, err)
	group, err := w.auth.Group(ctx, iam.GroupByID(groupID))
	must(t, err)
	if group.DeletedAt != nil {
		t.Fatal("successful retry still points at a retired AuthKit group")
	}
}
