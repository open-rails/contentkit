package critical_test

import (
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
		_, err = w.apps[1].client(token).PatchEntity(t.Context(), string(entity.ID), entities.EntityUpdate{Text: []entities.Text{{Language: "en", Description: "cross-app edit"}}})
		requireError(t, err, 404, "not_found")
		current, err := a.client(token).GetEntity(t.Context(), string(entity.ID), ck.GetEntityQuery{})
		must(t, err)
		if current.OwnerUserID != owner.ID {
			t.Fatal("cross-app request changed the owner")
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
	})
	t.Run("CK-J16-S04_deletion_retires_scope_and_owned_content", func(t *testing.T) {
		_, token, entity := ownedEntity(t, w, a, "retirement")
		item, err := a.client(token).PostContents(t.Context(), catalog.ContentInput{Kind: "article", OwnerEntityID: string(entity.ID), Text: map[string]catalog.Text{"en": {Title: "Channel article"}}})
		must(t, err)
		_, err = a.client(token).PostContentPublish(t.Context(), item.ID, catalog.PublishInput{})
		must(t, err)
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
		_, outsider := w.person(t, 0)
		_, err = a.client(outsider).PatchEntity(t.Context(), string(entity.ID), entities.EntityUpdate{Text: []entities.Text{{Language: "en", Description: "unauthorized"}}})
		requireError(t, err, 403, "forbidden")
	})
}
