package critical_test

import (
	"reflect"
	"testing"

	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	ck "github.com/open-rails/contentkit"
	ckauth "github.com/open-rails/contentkit/adapters/authkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/channels"
)

func TestEntityOwnerAssignmentBoundary(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	_, ownerToken, entity := ownedChannel(t, w, a, "Owner assignment")
	foreign, foreignToken := w.person(t, 1)
	groupID, err := ckauth.ChannelScopeID(*a.cfg.Scope, entity.AuthScope)
	must(t, err)
	groupBefore, err := w.auth.Group(t.Context(), iam.GroupByID(groupID))
	must(t, err)
	membershipsBefore, err := w.auth.ListMemberships(t.Context(), iam.UserSubject(foreign.ID), iam.PageRequest{Limit: 100})
	must(t, err)
	t.Run("CK-J03-S07_staff_owner_assignment_does_not_provision_foreign_membership", func(t *testing.T) {
		_, err := a.client(ownerToken).PatchChannel(t.Context(), string(entity.ID), channels.Update{OwnerUserID: &foreign.ID})
		requireError(t, err, 403, "forbidden")
		unchanged, err := a.engine.Client().GetManageChannel(t.Context(), string(entity.ID), ck.GetManageChannelQuery{})
		must(t, err)
		if unchanged.OwnerUserID != entity.OwnerUserID {
			t.Fatal("ordinary owner changed the staff-only ownership field")
		}
		// A trusted host may preserve an imported owner reference. That is not
		// registration in this tenant, nor a grant in its authorization group.
		changed, err := a.engine.Client().PatchChannel(t.Context(), string(entity.ID), channels.Update{OwnerUserID: &foreign.ID})
		must(t, err)
		if changed.OwnerUserID != foreign.ID || changed.AuthScope != entity.AuthScope {
			t.Fatalf("owner assignment changed scope identity: %+v", changed)
		}
		membershipsAfter, err := w.auth.ListMemberships(t.Context(), iam.UserSubject(foreign.ID), iam.PageRequest{Limit: 100})
		must(t, err)
		if !reflect.DeepEqual(membershipsBefore, membershipsAfter) {
			t.Fatal("owner reference fabricated an identity membership")
		}
		groupAfter, err := w.auth.Group(t.Context(), iam.GroupByID(groupID))
		must(t, err)
		if groupAfter.ID != groupBefore.ID || groupAfter.TenantID != groupBefore.TenantID || groupAfter.Persona != groupBefore.Persona {
			t.Fatal("owner assignment rebound the authorization scope")
		}
		_, err = a.client(foreignToken).GetMeChannels(t.Context(), ck.GetMeChannelsQuery{})
		requireError(t, err, 403, "forbidden")
		_, err = a.client(foreignToken).PostContents(t.Context(), catalog.ContentInput{Kind: "article", ChannelID: string(entity.ID)})
		requireError(t, err, 403, "forbidden")
		local, localToken := w.person(t, 0)
		_, err = a.engine.Client().PatchChannel(t.Context(), string(entity.ID), channels.Update{OwnerUserID: &local.ID})
		must(t, err)
		_, err = a.client(localToken).PostContents(t.Context(), catalog.ContentInput{Kind: "article", ChannelID: string(entity.ID)})
		requireError(t, err, 403, "forbidden") // metadata is not a scoped permission grant.
		authtest.GrantRole(t, w.tenant[0], iam.GroupByID(groupID), iam.UserSubject(local.ID), w.channelWriter)
		item, err := a.client(localToken).PostContents(t.Context(), catalog.ContentInput{Kind: "article", ChannelID: string(entity.ID), Versions: []catalog.VersionInput{{Languages: []string{"en"}, Text: map[string]catalog.VersionText{"en": {Title: "Accepted tenant owner"}}}}})
		must(t, err)
		if item.ChannelID != string(entity.ID) {
			t.Fatalf("real tenant owner wrote outside assigned entity: %+v", item)
		}
	})
}
