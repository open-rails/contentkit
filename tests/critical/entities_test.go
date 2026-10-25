package critical_test

import (
	"fmt"
	"testing"

	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	ck "github.com/open-rails/contentkit"
	ckauth "github.com/open-rails/contentkit/adapters/authkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/entities"
)

func ownedEntity(t *testing.T, w *world, a *application, title string) (authtest.User, string, entities.Entity) {
	t.Helper()
	user := authtest.NewUser(t, a.tenant)
	user.TOTP = authtest.EnrollTOTP(t, a.tenant, user)
	token := authtest.SignIn(t, a.tenant, user).AccessToken
	created, err := a.client(token).PostEntities(t.Context(), []entities.EntityInput{{Kind: "channel", Slug: "channel-" + user.ID, Names: []entities.Name{{Language: "en", Kind: "name", Name: title}}}})
	must(t, err)
	if len(created) != 1 || created[0].OwnerUserID != user.ID || created[0].AuthScope == "" {
		t.Fatalf("owned channel fixture has wrong authority: %+v", created)
	}
	return user, token, created[0]
}

func TestEntityConcurrentOwnershipAndStaleScopes(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	t.Run("CK-J16-S02_concurrent_creation_commits_one_owner_slot", func(t *testing.T) {
		owner := authtest.NewUser(t, a.tenant)
		owner.TOTP = authtest.EnrollTOTP(t, a.tenant, owner)
		token := authtest.SignIn(t, a.tenant, owner).AccessToken
		client := a.client(token)
		type result struct {
			entities []entities.Entity
			err      error
		}
		results, start := make(chan result, 4), make(chan struct{})
		for i := range 4 {
			go func() {
				<-start
				rows, err := client.PostEntities(t.Context(), []entities.EntityInput{{Kind: "channel", Slug: fmt.Sprintf("raced-%s-%d", owner.ID, i), Names: []entities.Name{{Language: "en", Kind: "name", Name: "Concurrent owner slot"}}}})
				results <- result{rows, err}
			}()
		}
		close(start)
		var winner entities.Entity
		successes := 0
		for range 4 {
			r := <-results
			if r.err != nil {
				requireError(t, r.err, 409, "limit_reached")
				continue
			}
			if len(r.entities) != 1 {
				t.Fatalf("unexpected race result: %+v", r.entities)
			}
			successes++
			winner = r.entities[0]
		}
		if successes != 1 {
			t.Fatalf("concurrent max-per-owner admitted %d entities", successes)
		}
		mine, err := client.GetMeEntities(t.Context(), ck.GetMeEntitiesQuery{})
		must(t, err)
		if len(mine.Entities) != 1 || mine.Entities[0].ID != winner.ID {
			t.Fatalf("race left more than the winner: %+v", mine)
		}
		groupID, err := ckauth.EntityScopeID(*a.cfg.Scope, winner.AuthScope)
		must(t, err)
		memberships, err := w.auth.ListMemberships(t.Context(), iam.UserSubject(owner.ID), iam.PageRequest{Limit: 100})
		must(t, err)
		channelCount := 0
		for _, membership := range memberships.Items {
			if membership.Group.Persona.String() == "channel" {
				channelCount++
				if membership.Group.ID != groupID {
					t.Fatalf("rejected creation provisioned an extra group: %+v", membership.Group)
				}
			}
		}
		if channelCount != 1 {
			t.Fatalf("race provisioned %d channel memberships", channelCount)
		}
	})
	t.Run("CK-J03-S06_CK-J16-S03_deleted_and_rebound_scope_refuses_existing_editor", func(t *testing.T) {
		_, _, entity := ownedEntity(t, w, a, "Stale authority")
		writer, token := w.person(t, 0)
		id, err := ckauth.EntityScopeID(*a.cfg.Scope, entity.AuthScope)
		must(t, err)
		group := iam.GroupByID(id)
		authtest.GrantRole(t, w.tenant[0], group, iam.UserSubject(writer.ID), w.channelWriter)
		client := a.client(token)
		input := catalog.ContentInput{Kind: "article", OwnerEntityID: string(entity.ID), Text: map[string]catalog.Text{"en": {Title: "Before scope retirement"}}}
		created, err := client.PostContents(t.Context(), input)
		must(t, err)
		must(t, w.auth.DeleteGroup(t.Context(), group))
		_, err = client.PostContents(t.Context(), input)
		// Current catalog contract reports an unresolved bound scope as
		// unavailable, while ordinary insufficient permissions are forbidden.
		requireError(t, err, 503, "unavailable")
		must(t, w.auth.PurgeGroup(t.Context(), group))
		foreignOwner, _ := w.person(t, 1)
		persona, err := w.auth.Persona("channel")
		must(t, err)
		foreign := iam.UserSubject(foreignOwner.ID)
		foreignTenant, err := iam.ParseTenantID(w.apps[2].cfg.Scope.TenantID.String())
		must(t, err)
		_, err = w.auth.CreateGroup(t.Context(), iam.NewGroup{ID: id, Persona: persona, TenantID: foreignTenant, Owner: &foreign})
		must(t, err)
		_, err = client.PostContents(t.Context(), input)
		requireError(t, err, 503, "unavailable")
		// Even a real grant in the recreated foreign group is not authority
		// over the app's content scope.
		foreignToken := authtest.SignIn(t, w.tenant[1], foreignOwner).AccessToken
		permission, err := w.auth.Permission("channel:content:write")
		must(t, err)
		allowed, err := w.auth.Can(t.Context(), authtest.Identity(t, w.tenant[1], foreignToken), group, permission)
		must(t, err)
		if !allowed {
			t.Fatal("foreign group owner lacked the positive-control permission")
		}
		_, err = a.client(foreignToken).PostContents(t.Context(), input)
		requireError(t, err, 503, "unavailable")
		current, err := a.engine.Client().GetContent(t.Context(), created.ID, ck.GetContentQuery{})
		must(t, err)
		if current.OwnerEntityID != string(entity.ID) || current.Title != "Before scope retirement" {
			t.Fatalf("refused stale writes changed content: %+v", current)
		}
		unchanged, err := a.engine.Client().GetManage(t.Context(), ck.GetManageQuery{Q: "Before scope retirement"})
		must(t, err)
		if len(unchanged) != 1 || unchanged[0].ID != created.ID {
			t.Fatalf("refused stale writes created extra content: %+v", unchanged)
		}
	})
}

func TestOwnedEntityAuthority(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	t.Run("CK-J16-S01_created_scope_belongs_to_exact_tenant_and_app", func(t *testing.T) {
		owner, token, entity := ownedEntity(t, w, a, "one channel")
		id, err := ckauth.EntityScopeID(*a.cfg.Scope, entity.AuthScope)
		must(t, err)
		group, err := w.auth.Group(t.Context(), iam.GroupByID(id))
		must(t, err)
		if group.TenantID.String() != a.cfg.Scope.TenantID.String() || group.Persona.String() != "channel" {
			t.Fatalf("provisioned scope has wrong ownership: %+v", group)
		}
		managed, err := a.engine.Client().GetManageEntities(t.Context(), ck.GetManageEntitiesQuery{Kind: "channel"})
		must(t, err)
		if len(managed.Entities) != 1 || managed.Entities[0].ID != entity.ID {
			t.Fatalf("management list lost its actual entity: %+v", managed)
		}
		_, err = a.client(token).GetManageEntities(t.Context(), ck.GetManageEntitiesQuery{Kind: "channel"})
		requireError(t, err, 403, "forbidden")
		foreign, err := w.apps[1].engine.Client().GetManageEntities(t.Context(), ck.GetManageEntitiesQuery{Kind: "channel"})
		must(t, err)
		if len(foreign.Entities) != 0 {
			t.Fatal("management entity listing crossed app boundary")
		}
		_, err = w.apps[1].client(token).PatchEntity(t.Context(), string(entity.ID), entities.EntityUpdate{Text: []entities.Text{{Language: "en", Description: "cross-app edit"}}})
		requireError(t, err, 404, "not_found")
		current, err := a.client(token).GetEntity(t.Context(), string(entity.ID), ck.GetEntityQuery{})
		must(t, err)
		if current.OwnerUserID != owner.ID {
			t.Fatal("cross-app request changed the entity")
		}
		for _, text := range current.Text {
			if text.Description == "cross-app edit" {
				t.Fatal("cross-app request changed entity description")
			}
		}
	})
	t.Run("CK-J16-S02_creation_limit_is_enforced_without_extra_entity", func(t *testing.T) {
		_, token, entity := ownedEntity(t, w, a, "limited channel")
		_, err := a.client(token).PostEntities(t.Context(), []entities.EntityInput{{Kind: "channel", Slug: "extra-" + entity.OwnerUserID, Names: []entities.Name{{Language: "en", Kind: "name", Name: "second must fail"}}}})
		requireError(t, err, 409, "limit_reached")
		mine, err := a.client(token).GetMeEntities(t.Context(), ck.GetMeEntitiesQuery{})
		must(t, err)
		if len(mine.Entities) != 1 || mine.Entities[0].ID != entity.ID {
			t.Fatalf("failed second creation changed ownership listing: %+v", mine)
		}
	})
	t.Run("CK-J03-S02_entity_writer_does_not_write_another_entity", func(t *testing.T) {
		_, _, first := ownedEntity(t, w, a, "writer target")
		_, _, other := ownedEntity(t, w, a, "other target")
		writer, token := w.person(t, 0)
		group, err := ckauth.EntityScopeID(*a.cfg.Scope, first.AuthScope)
		must(t, err)
		authtest.GrantRole(t, w.tenant[0], iam.GroupByID(group), iam.UserSubject(writer.ID), w.channelWriter)
		input := catalog.ContentInput{Kind: "article", OwnerEntityID: string(first.ID), Text: map[string]catalog.Text{"en": {Title: "Scoped work"}}}
		created, err := a.client(token).PostContents(t.Context(), input)
		must(t, err)
		if created.OwnerEntityID != string(first.ID) {
			t.Fatal("scoped write changed its owner")
		}
		input.OwnerEntityID = string(other.ID)
		_, err = a.client(token).PostContents(t.Context(), input)
		requireError(t, err, 403, "forbidden")
		rows, err := a.engine.Client().GetManage(t.Context(), ck.GetManageQuery{Q: "Scoped work"})
		must(t, err)
		if len(rows) != 1 || rows[0].ID != created.ID || rows[0].OwnerEntityID != string(first.ID) {
			t.Fatalf("refused cross-entity write changed content: %+v", rows)
		}
	})
	t.Run("CK-J16-S04_deletion_retires_scope_and_owned_content", func(t *testing.T) {
		startPublicationRuntime(t, a)
		_, token, entity := ownedEntity(t, w, a, "retirement")
		item, err := a.client(token).PostContents(t.Context(), catalog.ContentInput{Kind: "article", OwnerEntityID: string(entity.ID), Text: map[string]catalog.Text{"en": {Title: "Channel article"}}})
		must(t, err)
		_, err = a.client(token).PostContentPublish(t.Context(), item.ID, catalog.PublishInput{})
		must(t, err)
		awaitEditionPublication(t, a, a.engine.Client(), item.ID, item.Versions[0].ID)
		_, err = a.client("").GetContent(t.Context(), item.ID, ck.GetContentQuery{})
		must(t, err) // deletion must hide content that was actually published
		_, err = a.client(token).DeleteEntity(t.Context(), string(entity.ID))
		must(t, err)
		_, err = a.client("").GetContent(t.Context(), item.ID, ck.GetContentQuery{})
		requireError(t, err, 404, "not_found")
		id, err := ckauth.EntityScopeID(*a.cfg.Scope, entity.AuthScope)
		must(t, err)
		group, err := w.auth.Group(t.Context(), iam.GroupByID(id))
		must(t, err)
		if group.DeletedAt == nil {
			t.Fatal("deleted entity kept its authorization scope live")
		}
	})
	t.Run("CK-J16-S05_restore_does_not_widen_scope_or_change_owner", func(t *testing.T) {
		owner, token, entity := ownedEntity(t, w, a, "restoration authority")
		_, err := a.client(token).DeleteEntity(t.Context(), string(entity.ID))
		must(t, err)
		active := entities.State("active")
		restored, err := a.client(token).PatchEntity(t.Context(), string(entity.ID), entities.EntityUpdate{State: &active})
		must(t, err)
		if restored.OwnerUserID != owner.ID || restored.AuthScope != entity.AuthScope {
			t.Fatalf("restore changed authority: %+v", restored)
		}
		id, err := ckauth.EntityScopeID(*a.cfg.Scope, restored.AuthScope)
		must(t, err)
		group, err := w.auth.Group(t.Context(), iam.GroupByID(id))
		must(t, err)
		if group.DeletedAt != nil || group.TenantID.String() != a.cfg.Scope.TenantID.String() {
			t.Fatalf("restored scope not active in original tenant: %+v", group)
		}
		item, err := a.client(token).PostContents(t.Context(), catalog.ContentInput{Kind: "article", OwnerEntityID: string(entity.ID), Text: map[string]catalog.Text{"en": {Title: "Restored owner's work"}}})
		must(t, err)
		if item.OwnerEntityID != string(entity.ID) {
			t.Fatal("restored owner lost authoring capability")
		}
		_, outsider := w.person(t, 0)
		_, err = a.client(outsider).PatchEntity(t.Context(), string(entity.ID), entities.EntityUpdate{Text: []entities.Text{{Language: "en", Description: "unauthorized"}}})
		requireError(t, err, 403, "forbidden")
	})
}
