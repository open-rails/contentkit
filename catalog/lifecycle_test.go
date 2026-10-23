package catalog

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/open-rails/authkit/auth"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/internal/testauth"
)

func TestPublishedVersionReferencesIntegration(t *testing.T) {
	f := newFixture(t, nil)
	r, err := NewVersionReferences(f.schema, tenant)
	if err != nil {
		t.Fatal(err)
	}
	db := stdlib.OpenDB(*f.pool.Config().ConnConfig)
	t.Cleanup(func() { _ = db.Close() })
	in := gallery("Reference", "en", "ja")
	in.Access = &access.Rule{Level: access.TierLevel, Tiers: []string{"premium"}}
	id := f.create("staff", in)
	vid := f.read("staff", "main.test", id)[0].Versions[0].ID
	check := func(kind, language string, want error) {
		t.Helper()
		tx, err := db.BeginTx(f.ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		content, err := r.LockPublishedSQLTx(f.ctx, tx, vid, kind, language)
		if !errors.Is(err, want) || (want == nil && content != id) {
			t.Fatalf("reference %s/%s: content=%q err=%v, want %v", kind, language, content, err, want)
		}
	}
	publication := func(want bool) {
		t.Helper()
		var published bool
		if err := f.pool.QueryRow(f.ctx, `SELECT published FROM (`+r.PublicationSQL()+`) p
 WHERE catalog_key=$1 AND version_id=$2`, tenant, vid).Scan(&published); err != nil || published != want {
			t.Fatalf("publication=%v err=%v, want %v", published, err, want)
		}
	}
	check("gallery", "en", ErrConflict)
	publication(false)
	future := f.now().Add(time.Hour)
	f.publish("staff", id, &future)
	check("gallery", "en", ErrConflict)
	publication(false)
	f.publish("staff", id, nil)
	check("gallery", "en", nil)
	check("gallery", "ja", ErrConflict)
	check("blog", "en", ErrConflict)
	publication(true)
	other, err := NewVersionReferences(f.schema, "other-catalog")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(f.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.LockPublishedSQLTx(f.ctx, tx, vid, "gallery", "en"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-catalog reference: %v", err)
	}
	_ = tx.Rollback()
	if err := f.s.Unpublish(f.ctx, f.as("staff"), []string{id}); err != nil {
		t.Fatal(err)
	}
	check("gallery", "en", ErrConflict)
	publication(false)
	f.publish("staff", id, nil)
	check("gallery", "en", nil)
	publication(true)
	if err := f.s.DeleteVersions(f.ctx, f.as("staff"), []VersionKey{{ContentID: id, VersionID: vid}}); err != nil {
		t.Fatal(err)
	}
	check("gallery", "en", ErrConflict)
	publication(false)
}

// An item created without versions gets one serving its text's language,
// live with the item; a version added later stays a draft (out of every
// reader's choice) until its editors publish it.
func TestVersionDefaultsIntegration(t *testing.T) {
	f := newFixture(t, nil)
	id := f.create("staff", ContentInput{Kind: "gallery", Text: map[string]Text{"ja": {Title: "月"}}})
	draft := f.read("staff", "main.test", id)[0]
	if len(draft.Versions) != 1 || !slices.Equal(draft.Versions[0].Languages, []string{"ja"}) || draft.Versions[0].State != StatePublished {
		t.Fatalf("the default version: %+v", draft.Versions)
	}
	f.publish("staff", id, nil)
	vids, err := f.s.PutVersions(f.ctx, f.as("staff"), []VersionPut{{ContentID: id, VersionInput: VersionInput{Languages: []string{"en"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.read(anon, "main.test", id)[0]; got == nil || len(got.Versions) != 1 || got.Versions[0].ID == vids[0] {
		t.Fatalf("an added version shows before it is published: %+v", got)
	}
	if got := f.read("staff", "main.test", id)[0]; len(got.Versions) != 2 || got.Versions[1].State != StateDraft || got.Versions[1].Live {
		t.Fatalf("the editors' added version: %+v", got.Versions)
	}
	if _, err := f.s.PutVersions(f.ctx, f.as("staff"), []VersionPut{{ContentID: id, VersionID: vids[0], VersionInput: VersionInput{State: StatePublished}}}); err != nil {
		t.Fatal(err)
	}
	if got := f.read(anon, "main.test", id)[0]; len(got.Versions) != 2 {
		t.Fatalf("the published version: %+v", got.Versions)
	}
}

// A content item goes draft -> scheduled -> live -> deleted -> restored, and
// only its editors see it before and while it is not live; a deleted item
// stays readable to holders of its key until it is erased after
// the retention, rows and all.
func TestLifecycleIntegration(t *testing.T) {
	f := newFixture(t, nil)
	in := gallery("Moonlit Garden", "en", "ja")
	in.Creators = &[]Creator{{UserID: "alice"}}
	id := f.create("staff", in)
	if _, err := f.s.Create(f.ctx, f.as("bob"), []ContentInput{{Kind: "gallery", Text: map[string]Text{"en": {Title: "x"}}, Versions: []VersionInput{{Languages: []string{"en"}}}}}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a person without content:write created content: %v", err)
	}
	// alice edits only through her creator row.
	if err := f.s.Update(f.ctx, f.as("alice"), []Update{{ID: id, ContentPatch: ContentPatch{Comments: ptr("open")}}}); err != nil {
		t.Fatalf("the creator edits: %v", err)
	}
	draft := f.read("alice", "main.test", id)[0]
	if draft == nil || !draft.Editor || draft.State != StateDraft || draft.Live || len(draft.Versions) != 2 || draft.Creators[0].UserID != "alice" {
		t.Fatalf("the creator's draft %+v", draft)
	}
	for _, who := range []string{anon, "bob"} {
		if got := f.read(who, "main.test", id)[0]; got != nil {
			t.Fatalf("%q sees a draft: %+v", who, got)
		}
	}
	if got := f.read("viewer", "main.test", id)[0]; got == nil || !got.Editor {
		t.Fatalf("root:content:view-restricted does not see the draft: %+v", got)
	}

	// Scheduled: published for an hour from now. Visibility flips at that
	// time; PublishDue then stamps it and fires the side effects.
	at := f.now().Add(time.Hour)
	f.publish("staff", id, &at)
	if got := f.read(anon, "main.test", id)[0]; got != nil {
		t.Fatal("a scheduled item shows before its time")
	}
	if got := f.read("staff", "main.test", id)[0]; got.State != StatePublished || got.PublishedAt != nil || !got.PublishAt.Equal(at) {
		t.Fatalf("scheduled %+v", got)
	}
	if n, err := f.s.PublishDue(f.ctx); err != nil || n != 0 {
		t.Fatalf("PublishDue before its time: %d %v", n, err)
	}
	f.advance(time.Hour + time.Minute)
	live := f.read(anon, "main.test", id)[0]
	if live == nil || !live.Live || live.Editor || len(live.Versions) != 2 || live.Title != "Moonlit Garden" || live.Code == "" || live.Path != "/en/g/"+live.Code+"/moonlit-garden" {
		t.Fatalf("live at its time, before PublishDue: %+v", live)
	}
	if n, err := f.s.PublishDue(f.ctx); err != nil || n != 1 {
		t.Fatalf("PublishDue: %d %v", n, err)
	}
	if got := f.read("staff", "main.test", id)[0]; got.PublishedAt == nil || !got.PublishedAt.Equal(at) {
		t.Fatalf("published_at after PublishDue: %+v", got.PublishedAt)
	}

	// A version scheduled later goes live at its own time.
	later := f.now().Add(24 * time.Hour)
	vids, err := f.s.PutVersions(f.ctx, f.as("staff"), []VersionPut{{ContentID: id, VersionInput: VersionInput{Languages: []string{"en"},
		Traits: &[]string{"colored"}, State: StatePublished, PublishAt: &later}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.read(anon, "main.test", id)[0]; len(got.Versions) != 2 {
		t.Fatalf("an unreleased version shows: %+v", got.Versions)
	}
	f.advance(25 * time.Hour)
	if got := f.read(anon, "main.test", id)[0]; len(got.Versions) != 3 || got.Versions[2].ID != vids[0] || !slices.Equal(got.Versions[2].Traits, []string{"colored"}) {
		t.Fatalf("the released version: %+v", got.Versions)
	}
	if n, err := f.s.PublishDue(f.ctx); err != nil || n != 1 {
		t.Fatalf("PublishDue of the version: %d %v", n, err)
	}

	// Deleted: gone for everyone but its editors and holders of its key.
	if err := f.s.DeleteForModeration(f.ctx, f.as("staff"), []string{id}); err != nil {
		t.Fatal(err)
	}
	if got := f.read(anon, "main.test", id)[0]; got != nil {
		t.Fatal("anonymous reads a deleted item")
	}
	// Key holders read it too: adapters/openrails TestCatalogOverOpenRails.
	if got := f.read("staff", "main.test", id)[0]; got == nil || got.State != StateDeleted || got.Live || len(got.Versions) != 3 {
		t.Fatalf("staff read the deleted item: %+v", got)
	}
	if err := f.s.DeleteForModeration(f.ctx, f.as("staff"), []string{id}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleting twice: %v", err)
	}
	if err := f.s.Restore(f.ctx, f.as("staff"), []string{id}); err != nil {
		t.Fatal(err)
	}
	if got := f.read(anon, "main.test", id)[0]; got == nil || !got.Live {
		t.Fatal("restored item is not live")
	}

	// Unpublished: back to a draft.
	if err := f.s.Unpublish(f.ctx, f.as("staff"), []string{id}); err != nil {
		t.Fatal(err)
	}
	if got := f.read(anon, "main.test", id)[0]; got != nil {
		t.Fatal("an unpublished item shows")
	}

	// Purge applies the same retention to sold and never-sold content.
	sold := f.create("staff", gallery("Sold", "en"))
	f.publish("staff", sold, nil)
	if _, err := f.pool.Exec(f.ctx, `UPDATE `+f.table("content_items")+` SET ever_sold = true WHERE id = $1`, sold); err != nil {
		t.Fatal(err)
	}
	if err := f.s.DeleteForModeration(f.ctx, f.as("staff"), []string{id, sold}); err != nil {
		t.Fatal(err)
	}
	if n, err := f.s.PurgeDeleted(f.ctx); err != nil || n != 0 {
		t.Fatalf("purge inside the retention: %d %v", n, err)
	}
	f.advance(DefaultDeletedRetention + time.Hour)
	if n, err := f.s.PurgeDeleted(f.ctx); err != nil || n != 2 {
		t.Fatalf("purge: %d %v", n, err)
	}
	var left []string
	rows, _ := f.pool.Query(f.ctx, `SELECT id FROM `+f.table("content_items")+` ORDER BY id`)
	for rows.Next() {
		var x string
		_ = rows.Scan(&x)
		left = append(left, x)
	}
	if len(left) != 0 {
		t.Fatalf("after purge %v, want no remaining content", left)
	}

	// Every committed change remains independently readable.
	events := readEventLog(t, f)
	var types []string
	for _, e := range events {
		if e.ContentID == id {
			types = append(types, e.Type)
		}
	}
	want := []string{EventContentCreated, EventVersionCreated, EventVersionCreated, EventContentUpdated, EventContentScheduled,
		EventContentPublished, EventVersionCreated, EventVersionPublished, EventContentDeleted, EventContentRestored, EventContentUnpublished,
		EventContentDeleted, EventContentPurged}
	if !slices.Equal(types, want) {
		t.Fatalf("events %v\n want %v", types, want)
	}
	if got := readEventLog(t, f); !reflect.DeepEqual(got, events) {
		t.Fatal("reading changed the event log")
	}
}

// pooledRoles is the test auth reading its answers over the catalog's own
// pool, as AuthKit sharing a host's pool does. moved, when set, runs once
// before the first entity-scope answer.
type pooledRoles struct {
	*roles
	pool  *pgxpool.Pool
	moved func(context.Context) error
}

func (p *pooledRoles) CanIn(ctx context.Context, a access.Actor, scope auth.Scope, perm string) (bool, error) {
	if _, err := p.pool.Exec(ctx, `SELECT 1`); err != nil {
		return false, err
	}
	if move := p.moved; move != nil && scope != testauth.Scope {
		p.moved = nil
		if err := move(ctx); err != nil {
			return false, err
		}
	}
	return p.roles.CanIn(ctx, a, scope, perm)
}

// A write asks the authorization provider before it takes a connection and
// row locks, so a provider sharing a one-connection pool still answers it.
// The owner entity is rechecked under the lock: one that changed after the
// provider was asked refuses rather than granting on the old answer.
func TestWriteAuthorizesBeforeItsTransactionIntegration(t *testing.T) {
	f := newFixture(t, nil)
	studio, other := f.entity("channel", "studio"), f.entity("channel", "other")
	f.editor("studio", "ed")
	in := gallery("Shared", "en", "ja")
	in.OwnerEntityID, in.Creators = studio, &[]Creator{{UserID: "alice"}}
	id := f.create("staff", in)

	one := pgtest.Pool(t, func(c *pgxpool.Config) { c.MaxConns = 1 })
	provider := &pooledRoles{roles: f.roles, pool: one}
	s, err := New(f.ctx, Options{Pool: one, Schema: f.schema, Tenant: tenant, Kinds: config(), Auth: testauth.Auth{Authz: provider},
		Entities: f.entities, Now: f.now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	as := func(id string) access.Actor { return testauth.Actor(access.Actor{ID: id}, provider) }

	t.Run("owner-group editor publishes", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(f.ctx, 5*time.Second)
		defer cancel()
		if err := s.Publish(ctx, as("ed"), []Publish{{ContentID: id}}); err != nil {
			t.Fatalf("the editor publishes: %v", err)
		}
	})

	t.Run("staff, creator and owner-group editor edit", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(f.ctx, 5*time.Second)
		defer cancel()
		for _, who := range []string{"staff", "alice", "ed"} {
			if err := s.Update(ctx, as(who), []Update{{ID: id, ContentPatch: ContentPatch{Comments: ptr("open")}}}); err != nil {
				t.Fatalf("%s updates: %v", who, err)
			}
		}
		ed := as("ed")
		vids, err := s.PutVersions(ctx, ed, []VersionPut{{ContentID: id, VersionInput: VersionInput{Languages: []string{"en"}}}})
		if err != nil {
			t.Fatalf("the editor adds a version: %v", err)
		}
		if err := s.DeleteVersions(ctx, ed, []VersionKey{{ContentID: id, VersionID: vids[0]}}); err != nil {
			t.Fatalf("the editor deletes a version: %v", err)
		}
		if err := s.DeleteForModeration(ctx, ed, []string{id}); err != nil {
			t.Fatalf("the editor deletes: %v", err)
		}
		if err := s.Restore(ctx, ed, []string{id}); err != nil {
			t.Fatalf("the editor restores: %v", err)
		}
	})

	t.Run("owner group moved before the lock", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(f.ctx, 5*time.Second)
		defer cancel()
		provider.moved = func(ctx context.Context) error {
			_, err := one.Exec(ctx, `UPDATE `+f.table("content_items")+` SET owner_entity_id = $3 WHERE tenant_id = $1 AND id = $2`, tenant, id, other)
			return err
		}
		if err := s.DeleteForModeration(ctx, as("ed"), []string{id}); !errors.Is(err, ErrForbidden) {
			t.Fatalf("the studio's editor deleted an item moved to another group: %v", err)
		}
		var state, owner string
		if err := f.pool.QueryRow(f.ctx, `SELECT state, owner_entity_id FROM `+f.table("content_items")+` WHERE tenant_id = $1 AND id = $2`, tenant, id).Scan(&state, &owner); err != nil {
			t.Fatal(err)
		}
		if state == StateDeleted || owner != other {
			t.Fatalf("after the refused delete: state %s, owner %s", state, owner)
		}
	})
}

// Writes validate against the kinds config: languages, traits, attributes by
// type, restrictions, listing and the access rule.
func TestValidationIntegration(t *testing.T) {
	f := newFixture(t, nil)
	channel, unsold := f.entity("channel", "studio"), f.entity("channel", "quiet")
	f.sell(channel)
	tag := f.entity("tag", "red")
	ok := gallery("Valid", "en")
	ok.Attributes = map[string]any{"format": "manga", "volume": float64(3), "complete": true, "released": "2026-01-02T03:04:05Z", "note": "n"}
	ok.Access = &access.Rule{Level: access.EntityLevel, Entity: channel}
	ok.Restrictions = []string{"restricted"}
	id := f.create("staff", ok)
	got := f.read("staff", "main.test", id)[0]
	if got.Attributes["volume"] != float64(3) || got.Attributes["format"] != "manga" || got.Access.Entity != channel || got.Restrictions[0] != "restricted" {
		t.Fatalf("stored %+v", got)
	}
	for name, in := range map[string]ContentInput{
		"unknown kind":        {Kind: "video", Versions: []VersionInput{{Languages: []string{"en"}}}},
		"language":            gallery("x", "de"),
		"two languages":       {Kind: "gallery", Versions: []VersionInput{{Languages: []string{"en", "ja"}}}},
		"trait":               {Kind: "gallery", Versions: []VersionInput{{Languages: []string{"en"}, Traits: &[]string{"dubbed"}}}},
		"text language":       {Kind: "gallery", Text: map[string]Text{"de": {Title: "x"}}, Versions: []VersionInput{{Languages: []string{"en"}}}},
		"blank title":         {Kind: "gallery", Text: map[string]Text{"en": {Title: " "}}, Versions: []VersionInput{{Languages: []string{"en"}}}},
		"undeclared":          {Kind: "gallery", Attributes: map[string]any{"pages": 3}, Versions: []VersionInput{{Languages: []string{"en"}}}},
		"enum value":          {Kind: "gallery", Attributes: map[string]any{"format": "comic"}, Versions: []VersionInput{{Languages: []string{"en"}}}},
		"int":                 {Kind: "gallery", Attributes: map[string]any{"volume": 1.5}, Versions: []VersionInput{{Languages: []string{"en"}}}},
		"time":                {Kind: "gallery", Attributes: map[string]any{"released": "yesterday"}, Versions: []VersionInput{{Languages: []string{"en"}}}},
		"restriction":         {Kind: "gallery", Restrictions: []string{"nsfw"}, Versions: []VersionInput{{Languages: []string{"en"}}}},
		"tier":                {Kind: "gallery", Access: &access.Rule{Level: access.TierLevel, Tiers: []string{"gold"}}, Versions: []VersionInput{{Languages: []string{"en"}}}},
		"group not sellable":  {Kind: "gallery", Access: &access.Rule{Level: access.EntityLevel, Entity: tag}, Versions: []VersionInput{{Languages: []string{"en"}}}},
		"missing group":       {Kind: "gallery", Access: &access.Rule{Level: access.EntityPPV, Entity: contentref.NewID()}, Versions: []VersionInput{{Languages: []string{"en"}}}},
		"hidden with version": {Kind: "blog", Versions: []VersionInput{{Languages: []string{"en"}}}},
	} {
		if _, err := f.s.Create(f.ctx, f.as("staff"), []ContentInput{in}); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	// Members-only content needs no membership offer: existing members keep
	// access and the entity may sell one later.
	members := &access.Rule{Level: access.EntityPPV, Entity: unsold}
	quiet := gallery("Quiet", "en")
	quiet.Access = members
	if _, err := f.s.Create(f.ctx, f.as("staff"), []ContentInput{quiet}); err != nil {
		t.Errorf("members-only on an unsold group: %v", err)
	}
	if err := f.s.Update(f.ctx, f.as("staff"), []Update{{ID: id, ContentPatch: ContentPatch{Access: members}}}); err != nil {
		t.Errorf("switching to an unsold group: %v", err)
	}
	if _, err := f.s.PutVersions(f.ctx, f.as("staff"), []VersionPut{{ContentID: id, VersionID: got.Versions[0].ID, VersionInput: VersionInput{Access: members}}}); err != nil {
		t.Errorf("a version's override on an unsold group: %v", err)
	}
	segs := []SegmentInput{{Start: 0, End: 5}, {Start: 3, End: 9}}
	if _, err := f.s.PutVersions(f.ctx, f.as("staff"), []VersionPut{{ContentID: id, VersionID: got.Versions[0].ID, VersionInput: VersionInput{Segments: &segs}}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("overlapping segments: %v", err)
	}
	blog := f.create("staff", ContentInput{Kind: "blog", Text: map[string]Text{"en": {Title: "Post"}}})
	segs = segs[:1]
	if _, err := f.s.PutVersions(f.ctx, f.as("staff"), []VersionPut{{ContentID: blog, VersionInput: VersionInput{Languages: []string{"en"}}}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("a version of a hidden kind: %v", err)
	}
}

// A kind with one hidden version keeps it serving the text's language.
func TestHiddenVersionFollowsTextIntegration(t *testing.T) {
	f := newFixture(t, nil)
	id := f.create("staff", ContentInput{Kind: "blog", Text: map[string]Text{"ja": {Title: "日記"}}})
	if got := f.read("staff", "main.test", id)[0]; len(got.Versions) != 1 || !slices.Equal(got.Versions[0].Languages, []string{"ja"}) {
		t.Fatalf("hidden version %+v", got.Versions)
	}
	if err := f.s.PutText(f.ctx, f.as("staff"), []TextPut{{ContentID: id, Language: "en", Text: &Text{Title: "Diary"}}}); err != nil {
		t.Fatal(err)
	}
	got := f.read("staff", "main.test", id)[0]
	if !slices.Equal(got.Versions[0].Languages, []string{"en"}) || got.Language != "en" || got.Slug != "diary" {
		t.Fatalf("after English text: %+v", got)
	}
}

// Audience release facts use the same recorder as publication.
func TestAudienceReleaseIntegration(t *testing.T) {
	f := newFixture(t, nil)
	at := f.now().Add(time.Hour)
	in := gallery("Audience release", "en")
	in.Access = &access.Rule{Level: access.TierLevel, Tiers: []string{"premium"}, Releases: []access.Release{{At: at, Level: access.Public}}}
	id := f.create("staff", in)
	f.publish("staff", id, nil)
	if n, err := f.s.PublishDue(f.ctx); err != nil || n != 0 {
		t.Fatalf("before: %d %v", n, err)
	}
	f.advance(2 * time.Hour)
	if n, err := f.s.PublishDue(f.ctx); err != nil || n != 1 {
		t.Fatalf("after: %d %v", n, err)
	}
	if n, err := f.s.PublishDue(f.ctx); err != nil || n != 0 {
		t.Fatalf("retry: %d %v", n, err)
	}
	var got []EventRecord
	for _, event := range readEventLog(t, f) {
		if event.Type == EventAudienceReleased {
			got = append(got, event)
		}
	}
	if len(got) != 1 || got[0].ContentID != id || !got[0].OccurredAt.Equal(at) {
		t.Fatalf("release facts: %+v", got)
	}
}
