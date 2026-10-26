package critical_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	ck "github.com/open-rails/contentkit"
	ckauth "github.com/open-rails/contentkit/adapters/authkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/channels"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/importer"
)

// Only acknowledgements are lost; the group operations use real AuthKit.
type lostChannelReplies struct {
	entities.Scopes
	scope             ck.Scope
	provision, retire atomic.Bool
}

func (s *lostChannelReplies) ContentKitScope() (string, string) {
	return s.scope.TenantID.String(), s.scope.AppID.String()
}
func (s *lostChannelReplies) Provision(ctx context.Context, in []entities.Scope) error {
	if err := s.Scopes.Provision(ctx, in); err != nil {
		return err
	}
	if s.provision.Swap(false) {
		return errors.New("test: group committed but response lost")
	}
	return nil
}
func (s *lostChannelReplies) Retire(ctx context.Context, ids []string) error {
	if err := s.Scopes.Retire(ctx, ids); err != nil {
		return err
	}
	if s.retire.Swap(false) {
		return errors.New("test: retirement committed but response lost")
	}
	return nil
}

// Retain these journey identities while replacing the retired worker contract
// with the owner-approved direct-call and idempotent caller-retry contract.
func TestEntityLifecycleWithoutReconciliation(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	provider := &lostChannelReplies{Scopes: a.deps.Scopes, scope: *a.cfg.Scope}
	a.server.Close()
	must(t, a.engine.Close(t.Context()))
	a.deps.Scopes = provider
	var err error
	a.engine, err = ck.New(t.Context(), a.cfg, a.deps)
	must(t, err)
	a.server = mountEngine(t, a.engine)
	var channel channels.Channel
	var owner authtest.User
	var token string
	noLifecycleJobs := func() {
		t.Helper()
		var n int
		must(t, w.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+pgx.Identifier{w.schema + "_river", "river_job"}.Sanitize()+` WHERE kind IN ('contentkit_entity_sync','contentkit_channel_lifecycle')`).Scan(&n))
		if n != 0 {
			t.Fatalf("direct lifecycle created %d retired jobs", n)
		}
	}
	t.Run("CK-J16-S01_create_retries_same_channel_after_lost_acknowledgement", func(t *testing.T) {
		owner = authtest.NewUser(t, a.tenant)
		owner.TOTP = authtest.EnrollTOTP(t, a.tenant, owner)
		token = authtest.SignIn(t, a.tenant, owner).AccessToken
		in := channels.Input{Slug: "retry-" + owner.ID, Names: []entities.Name{{Language: "en", Kind: "name", Name: "Retry one channel"}}}
		provider.provision.Store(true)
		if _, err := a.client(token).PostChannels(t.Context(), in); err == nil {
			t.Fatal("lost acknowledgement reported success")
		}
		var reserved string
		must(t, w.pool.QueryRow(t.Context(), `SELECT id FROM `+pgx.Identifier{w.schema, "content_entities"}.Sanitize()+` WHERE tenant_id=$1 AND slug=$2`, a.cfg.Scope.AppID.String(), in.Slug).Scan(&reserved))
		_, err := a.client(token).PostContents(t.Context(), catalog.ContentInput{Kind: "article", ChannelID: reserved})
		if err == nil {
			t.Fatal("pending channel granted authority")
		}
		channel, err = a.client(token).PostChannels(t.Context(), in)
		must(t, err)
		if channel.ID != reserved || channel.AuthScope == "" {
			t.Fatal("retry changed identity or failed to complete group")
		}
		again, err := a.client(token).PostChannels(t.Context(), in)
		must(t, err)
		if again.ID != channel.ID {
			t.Fatal("successful replay duplicated channel")
		}
		noLifecycleJobs()
	})
	var post catalog.Content
	t.Run("CK-J16-S04_delete_returns_error_and_retries_retirement_inline", func(t *testing.T) {
		post = authoredArticle(t, a, a.client(token), channel.ID)
		provider.retire.Store(true)
		if err := a.client(token).DeleteChannel(t.Context(), channel.ID); err == nil {
			t.Fatal("lost retirement acknowledgement reported success")
		}
		_, err := a.client("").GetContent(t.Context(), post.ID, ck.GetContentQuery{})
		requireError(t, err, 404, "not_found")
		must(t, a.client(token).DeleteChannel(t.Context(), channel.ID))
		groupID, err := ckauth.ChannelScopeID(*a.cfg.Scope, channel.ID)
		must(t, err)
		group, err := w.auth.Group(t.Context(), iam.GroupByID(groupID))
		must(t, err)
		if group.DeletedAt == nil {
			t.Fatal("channel deletion returned before group retirement")
		}
		noLifecycleJobs()
	})
	t.Run("CK-J16-S05_restore_and_import_finish_without_lifecycle_jobs", func(t *testing.T) {
		active := entities.StateActive
		_, err := a.engine.Client().PatchChannel(t.Context(), channel.ID, channels.Update{State: &active})
		must(t, err)
		restored, err := a.client(token).GetContent(t.Context(), post.ID, ck.GetContentQuery{})
		must(t, err)
		if restored.ChannelID != channel.ID {
			t.Fatal("restore changed primary channel")
		}
		batch := importer.Batch{Source: "direct-channel-import", ProvisionScopes: true, Channels: []importer.ChannelRecord{{ExternalID: "retained", ID: channel.ID, Slug: channel.Slug, OwnerUserID: owner.ID, State: "active"}}}
		for range 2 {
			result, err := a.engine.Client().PostImport(t.Context(), batch)
			must(t, err)
			if len(result.Failed) != 0 || result.Written[importer.SectionChannels] != 1 || result.IDs[importer.RecordChannel]["retained"] != channel.ID {
				t.Fatalf("idempotent import failed: %+v", result)
			}
		}
		noLifecycleJobs()
	})
}
