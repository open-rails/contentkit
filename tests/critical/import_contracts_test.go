package critical_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	ckauth "github.com/open-rails/contentkit/adapters/authkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/importer"
)

// A bulk import writes what an authored write would, refuses records that
// leave out the truth instead of defaulting it, and reports what it could not
// map.
func TestImportContracts(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	admin, ctx := a.engine.Client(), t.Context()
	app := a.cfg.Scope.AppID.String()
	qs := pgx.Identifier{w.schema}.Sanitize()
	public := &access.Rule{Level: access.Public}
	version := func(external, title, body string) importer.VersionRecord {
		return importer.VersionRecord{ExternalID: external, Languages: []string{"en"}, State: catalog.StatePublished,
			Text: map[string]catalog.VersionText{"en": {Title: title, Body: body}}}
	}
	body := func(t *testing.T, id string) string {
		t.Helper()
		var out string
		must(t, w.pool.QueryRow(ctx, `SELECT t.body FROM `+qs+`.content_version_text t JOIN `+qs+`.content_versions v ON v.tenant_id=t.tenant_id AND v.id=t.version_id
 WHERE v.tenant_id=$1 AND v.content_id=$2 AND t.language='en'`, app, id).Scan(&out))
		return out
	}

	t.Run("CK-J27-S07_import_sanitizes_bodies_as_authored_writes_do", func(t *testing.T) {
		markup := `<p>Hello <b>reader</b></p><img src=x onerror="alert(1)"><script>alert(2)</script>`
		authored, err := admin.PostContents(ctx, catalog.ContentInput{Kind: "article", Versions: []catalog.VersionInput{{Languages: []string{"en"},
			Text: map[string]catalog.VersionText{"en": {Title: "Authored markup", Body: markup}}}}})
		must(t, err)
		res, err := admin.PostImport(ctx, importer.Batch{Source: "sanitize", Contents: []importer.ContentRecord{{ExternalID: "markup", Kind: "article",
			State: catalog.StateDraft, Access: public, Versions: []importer.VersionRecord{version("markup/version", "Imported markup", markup)}}}})
		must(t, err)
		if len(res.Failed) != 0 || res.Written[importer.SectionContents] != 1 {
			t.Fatalf("import refused the markup record: %+v", res)
		}
		imported, written := body(t, res.IDs[importer.RecordContent]["markup"]), body(t, authored.ID)
		if imported != "Hello readeralert(2)" || imported != written {
			t.Fatalf("import stored %q, an authored write %q; both must be the default sanitizer's", imported, written)
		}
	})

	t.Run("CK-J27-S08_a_record_without_access_or_version_state_is_refused", func(t *testing.T) {
		paid := &access.Rule{Level: access.TierLevel, Tiers: []string{"gold"}}
		first, err := admin.PostImport(ctx, importer.Batch{Source: "whole-truth", Contents: []importer.ContentRecord{{ExternalID: "paid", Kind: "article",
			State: catalog.StateDraft, Access: paid, Versions: []importer.VersionRecord{version("paid/version", "Paid", "Paid body")}}}})
		must(t, err)
		if len(first.Failed) != 0 {
			t.Fatalf("paid fixture: %+v", first)
		}
		id := first.IDs[importer.RecordContent]["paid"]
		stateless := version("stateless/version", "Stateless", "")
		stateless.State = ""
		res, err := admin.PostImport(ctx, importer.Batch{Source: "whole-truth", Contents: []importer.ContentRecord{
			{ExternalID: "paid", Kind: "article", State: catalog.StateDraft, Versions: []importer.VersionRecord{version("paid/version", "Paid", "Paid body")}},
			{ExternalID: "stateless", Kind: "article", State: catalog.StateDraft, Access: public, Versions: []importer.VersionRecord{stateless}},
			{ExternalID: "complete", Kind: "article", State: catalog.StateDraft, Access: public, Versions: []importer.VersionRecord{version("complete/version", "Complete", "")}},
		}})
		must(t, err)
		if got := importFailures(res); !reflect.DeepEqual(got, []importFailure{{importer.SectionContents, 0, importer.CodeInvalid}, {importer.SectionContents, 1, importer.CodeInvalid}}) ||
			res.Written[importer.SectionContents] != 1 {
			t.Fatalf("records without access or state were defaulted: %+v", res)
		}
		stored, err := admin.GetContent(ctx, id, ck.GetContentQuery{})
		must(t, err)
		if stored.Access.Level != access.TierLevel || !reflect.DeepEqual(stored.Access.Tiers, []string{"gold"}) {
			t.Fatalf("a replay without access changed the paywall: %+v", stored.Access)
		}
	})

	t.Run("CK-J27-S09_a_delete_of_an_unmapped_external_id_is_reported", func(t *testing.T) {
		kept, err := admin.PostImport(ctx, importer.Batch{Source: "unmapped", Contents: []importer.ContentRecord{{ExternalID: "kept", Kind: "article",
			State: catalog.StateDraft, Access: public, Versions: []importer.VersionRecord{version("kept/version", "Kept", "")}}}})
		must(t, err)
		if len(kept.Failed) != 0 {
			t.Fatalf("delete fixture: %+v", kept)
		}
		deletes := importer.Batch{Source: "unmapped", Deletes: []importer.DeleteRecord{{Entity: importer.RecordContent, ExternalID: "kept"},
			{Entity: importer.RecordContent, ExternalID: "never"}, {Entity: importer.RecordVersion, ExternalID: "never/version"}}}
		want := []importer.UnmappedDelete{{Index: 1, Entity: importer.RecordContent, ExternalID: "never"}, {Index: 2, Entity: importer.RecordVersion, ExternalID: "never/version"}}
		for range 2 {
			res, err := admin.PostImport(ctx, deletes)
			must(t, err)
			if len(res.Failed) != 0 || res.Written[importer.SectionDeletes] != 1 || !reflect.DeepEqual(res.Unmapped, want) {
				t.Fatalf("unmapped deletes were not reported per record: %+v", res)
			}
		}
		var state string
		must(t, w.pool.QueryRow(ctx, `SELECT state FROM `+qs+`.content_items WHERE tenant_id=$1 AND id=$2`, app, kept.IDs[importer.RecordContent]["kept"]).Scan(&state))
		if state != catalog.StateDeleted {
			t.Fatalf("the mapped delete did not delete: %s", state)
		}
	})

	t.Run("CK-J27-S10_imported_account_ids_and_addresses_are_canonical", func(t *testing.T) {
		item := publishedArticle(t, a, "Canonical import target")
		user, token := w.person(t, 0)
		target := importer.TargetRef{Content: &importer.Ref{ID: item.ID}}
		res, err := admin.PostImport(ctx, importer.Batch{Source: "canonical",
			Reactions: []importer.ReactionRecord{{Target: target, IP: "::ffff:192.0.2.7", Value: 1}, {Target: target, IP: "2001:db8:1:2:3:4:5:6", Value: 1},
				{Target: target, IP: "192.0.2.300", Value: 1}},
			Favorites: []importer.FavoriteRecord{{Target: target, UserID: " " + strings.ToUpper(user.ID) + " "}}})
		must(t, err)
		if got := importFailures(res); !reflect.DeepEqual(got, []importFailure{{importer.SectionReactions, 2, importer.CodeInvalid}}) ||
			res.Written[importer.SectionReactions] != 2 || res.Written[importer.SectionFavorites] != 1 {
			t.Fatalf("canonical import: %+v", res)
		}
		state, err := a.client(token).GetContentFavorite(ctx, item.ID, ck.GetContentFavoriteQuery{})
		must(t, err)
		if !state.Favorited {
			t.Fatal("a favorite imported under a non-canonical account id is not the account's")
		}
		// Stored as live anonymous writes key them: unmapped IPv4, IPv6 /64.
		var ips []string
		must(t, w.pool.QueryRow(ctx, `SELECT array_agg(ip ORDER BY ip) FROM `+qs+`.content_reactions WHERE tenant_id=$1 AND content_id=$2 AND user_id IS NULL`, app, item.ID).Scan(&ips))
		if !reflect.DeepEqual(ips, []string{"192.0.2.7", "2001:db8:1:2::/64"}) {
			t.Fatalf("imported addresses stored as %q", ips)
		}
	})
}

// A channel's authorization group is its own id: the import wire cannot name
// one, an import provisions it, and storage refuses any other.
func TestChannelImportScope(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	admin, ctx := a.engine.Client(), t.Context()
	_, _, existing := ownedChannel(t, w, a, "Existing channel")
	staff, token := w.person(t, 0)
	authtest.GrantRole(t, w.tenant[0], a.group, iam.UserSubject(staff.ID), w.importer)

	t.Run("CK-J16-S13_import_wire_has_no_auth_scope", func(t *testing.T) {
		raw, err := json.Marshal(map[string]any{"source": "scope-wire", "channels": []map[string]any{{"external_id": "borrower", "slug": "borrower",
			"auth_scope": existing.ID}}})
		must(t, err)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.server.URL+"/content/v1/import", bytes.NewReader(raw))
		must(t, err)
		authtest.Authorize(t, w.auth, req, token)
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		must(t, err)
		defer res.Body.Close()
		var refusal struct{ Code string }
		must(t, json.NewDecoder(res.Body).Decode(&refusal))
		if res.StatusCode != http.StatusBadRequest || refusal.Code != "invalid_request" {
			t.Fatalf("an import naming another channel's scope answered %d %s", res.StatusCode, refusal.Code)
		}
	})

	t.Run("CK-J16-S13_import_provisions_the_channels_own_group", func(t *testing.T) {
		owner := authtest.NewUser(t, a.tenant)
		owner.TOTP = authtest.EnrollTOTP(t, a.tenant, owner)
		id := contentref.NewID()
		batch := importer.Batch{Source: "scope-provision", Channels: []importer.ChannelRecord{{ExternalID: "fresh", ID: id, Slug: "imported-" + id[len(id)-12:], OwnerUserID: owner.ID, State: "active"}}}
		for range 2 {
			res, err := admin.PostImport(ctx, batch)
			must(t, err)
			if len(res.Failed) != 0 || res.Written[importer.SectionChannels] != 1 {
				t.Fatalf("channel import did not provision its group: %+v", res)
			}
		}
		got, err := admin.GetManageChannel(ctx, id, ck.GetManageChannelQuery{})
		must(t, err)
		if got.AuthScope != id {
			t.Fatalf("imported channel's scope is %q, not its id", got.AuthScope)
		}
		groupID, err := ckauth.ChannelScopeID(*a.cfg.Scope, id)
		must(t, err)
		group, err := w.auth.Group(ctx, iam.GroupByID(groupID))
		must(t, err)
		if group.DeletedAt != nil || group.Persona.String() != "channel" {
			t.Fatalf("imported channel's group is not live: %+v", group)
		}
	})

	t.Run("CK-J16-S13_storage_refuses_a_scope_other_than_the_channel_id", func(t *testing.T) {
		_, _, other := ownedChannel(t, w, a, "Other channel")
		_, err := w.pool.Exec(ctx, `UPDATE `+pgx.Identifier{w.schema, "content_entities"}.Sanitize()+` SET auth_scope=$3 WHERE tenant_id=$1 AND id=$2`,
			a.cfg.Scope.AppID.String(), other.ID, existing.ID)
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || pg.Code != "23514" {
			t.Fatalf("a channel was rebound to another channel's scope: %v", err)
		}
	})
}
