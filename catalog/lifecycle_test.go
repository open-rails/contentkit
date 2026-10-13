package catalog

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/kinds"
)

// A content item goes draft -> scheduled -> live -> deleted -> restored, and
// only its editors see it before and while it is not live; a deleted item
// stays readable to holders of its key, and one never sold is erased after
// the retention, rows and all.
func TestLifecycleIntegration(t *testing.T) {
	f := newFixture(t, nil)
	var events []Event
	f.s.Subscribe(func(_ context.Context, batch []Event) error {
		events = append(events, batch...)
		return nil
	})
	in := gallery("Moonlit Garden", "en", "ja")
	in.Creators = &[]Creator{{UserID: "alice"}}
	id := f.create("staff", in)
	if _, err := f.s.Create(f.ctx, f.as("bob"), []ContentInput{{Kind: "gallery", Text: map[string]Text{"en": {Title: "x"}}, Versions: []VersionInput{{Languages: []string{"en"}}}}}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a person without content:write created content: %v", err)
	}
	// alice edits only through her creator row.
	if err := f.s.Update(f.ctx, f.as("alice"), []Update{{ID: id, ContentPatch: ContentPatch{Listing: ptr(ListingListed)}}}); err != nil {
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
	if live == nil || !live.Live || live.Editor || len(live.Versions) != 2 || live.Title != "Moonlit Garden" || live.Code == "" || live.Path != "/g/"+live.Code+"/moonlit-garden" {
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
		Traits: &[]string{"colored"}, PublishAt: &later}}})
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
	if err := f.s.Delete(f.ctx, f.as("staff"), []string{id}); err != nil {
		t.Fatal(err)
	}
	if got := f.read(anon, "main.test", id)[0]; got != nil {
		t.Fatal("anonymous reads a deleted item")
	}
	// Key holders read it too: adapters/openrails TestCatalogOverOpenRails.
	if got := f.read("staff", "main.test", id)[0]; got == nil || got.State != StateDeleted || got.Live || len(got.Versions) != 3 {
		t.Fatalf("staff read the deleted item: %+v", got)
	}
	if err := f.s.Delete(f.ctx, f.as("staff"), []string{id}); !errors.Is(err, ErrNotFound) {
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

	// Purge: a deleted item never sold is erased after the retention; a sold
	// one stays for its holders.
	sold := f.create("staff", gallery("Sold", "en"))
	f.publish("staff", sold, nil)
	if _, err := f.pool.Exec(f.ctx, `UPDATE `+f.table("content_items")+` SET ever_sold = true WHERE id = $1`, sold); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Delete(f.ctx, f.as("staff"), []string{id, sold}); err != nil {
		t.Fatal(err)
	}
	if n, err := f.s.PurgeDeleted(f.ctx); err != nil || n != 0 {
		t.Fatalf("purge inside the retention: %d %v", n, err)
	}
	f.advance(DefaultDeletedRetention + time.Hour)
	if n, err := f.s.PurgeDeleted(f.ctx); err != nil || n != 1 {
		t.Fatalf("purge: %d %v", n, err)
	}
	var left []string
	rows, _ := f.pool.Query(f.ctx, `SELECT id FROM `+f.table("content_items")+` ORDER BY id`)
	for rows.Next() {
		var x string
		_ = rows.Scan(&x)
		left = append(left, x)
	}
	if !slices.Equal(left, []string{sold}) {
		t.Fatalf("after purge %v, want only the sold item", left)
	}

	// Every change reached the outbox and is delivered in order once.
	n, err := f.s.DeliverEvents(f.ctx)
	if err != nil || n != len(events) {
		t.Fatalf("delivered %d (%v), subscriber saw %d", n, err, len(events))
	}
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
	if n, err := f.s.DeliverEvents(f.ctx); err != nil || n != 0 {
		t.Fatalf("redelivered %d %v", n, err)
	}
}

// A failing subscriber keeps the batch for the next delivery, in order.
func TestEventsRedeliverAfterFailureIntegration(t *testing.T) {
	f := newFixture(t, nil)
	fail := true
	var seen []string
	f.s.Subscribe(func(_ context.Context, batch []Event) error {
		if fail {
			return errors.New("down")
		}
		for _, e := range batch {
			seen = append(seen, e.Type)
		}
		return nil
	})
	id := f.create("staff", gallery("One", "en"))
	f.publish("staff", id, nil)
	if _, err := f.s.DeliverEvents(f.ctx); err == nil {
		t.Fatal("a failed delivery reported success")
	}
	fail = false
	if n, err := f.s.DeliverEvents(f.ctx); err != nil || n != 3 {
		t.Fatalf("redelivery: %d %v", n, err)
	}
	if !slices.Equal(seen, []string{EventContentCreated, EventVersionCreated, EventContentPublished}) {
		t.Fatalf("seen %v", seen)
	}
}

// Writes validate against the kinds config: languages, traits, attributes by
// type, restrictions, listing and the access rule.
func TestValidationIntegration(t *testing.T) {
	f := newFixture(t, nil)
	channel, unsold := f.group("channel", "studio"), f.group("channel", "quiet")
	f.sell(channel)
	tag := f.group("tag", "red")
	ok := gallery("Valid", "en")
	ok.Attributes = map[string]any{"format": "manga", "volume": float64(3), "complete": true, "released": "2026-01-02T03:04:05Z", "note": "n"}
	ok.Access = &access.Rule{Level: access.GroupLevel, Group: channel}
	ok.Restrictions = []string{"restricted"}
	id := f.create("staff", ok)
	got := f.read("staff", "main.test", id)[0]
	if got.Attributes["volume"] != float64(3) || got.Attributes["format"] != "manga" || got.Access.Group != channel || got.Restrictions[0] != "restricted" {
		t.Fatalf("stored %+v", got)
	}
	for name, in := range map[string]ContentInput{
		"unknown kind":        {Kind: "video", Versions: []VersionInput{{Languages: []string{"en"}}}},
		"no version":          {Kind: "gallery", Text: map[string]Text{"en": {Title: "x"}}},
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
		"listing":             {Kind: "gallery", Listing: "secret", Versions: []VersionInput{{Languages: []string{"en"}}}},
		"tier":                {Kind: "gallery", Access: &access.Rule{Level: access.TierLevel, Tiers: []string{"gold"}}, Versions: []VersionInput{{Languages: []string{"en"}}}},
		"group not sellable":  {Kind: "gallery", Access: &access.Rule{Level: access.GroupLevel, Group: tag}, Versions: []VersionInput{{Languages: []string{"en"}}}},
		"missing group":       {Kind: "gallery", Access: &access.Rule{Level: access.GroupPPV, Group: contentref.NewID()}, Versions: []VersionInput{{Languages: []string{"en"}}}},
		"hidden with version": {Kind: "blog", Versions: []VersionInput{{Languages: []string{"en"}}}},
	} {
		if _, err := f.s.Create(f.ctx, f.as("staff"), []ContentInput{in}); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	// Members-only content needs no membership offer: existing members keep
	// access and the group may sell one later.
	members := &access.Rule{Level: access.GroupPPV, Group: unsold}
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
	segs := []Segment{{Start: 0, End: 5}, {Start: 3, End: 9}}
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

// A kind's draft_ttl expires its drafts that never went live, rows and all;
// a draft unpublished after going live, a fresh one and another kind's stay.
func TestDraftExpiryIntegration(t *testing.T) {
	f := newFixture(t, func(o *Options) {
		blog := o.Kinds.Kinds["blog"]
		blog.DraftTTL = 24 * time.Hour
		o.Kinds.Kinds["blog"] = blog
	})
	var events []Event
	f.s.Subscribe(func(_ context.Context, batch []Event) error {
		events = append(events, batch...)
		return nil
	})
	post := func(title string) string {
		return f.create("staff", ContentInput{Kind: "blog", Text: map[string]Text{"en": {Title: title}}})
	}
	abandoned, unpublished, fresh := post("Abandoned"), post("Unpublished"), post("Fresh")
	f.publish("staff", unpublished, nil)
	if err := f.s.Unpublish(f.ctx, f.as("staff"), []string{unpublished}); err != nil {
		t.Fatal(err)
	}
	old := f.create("staff", gallery("Old gallery", "en"))
	if _, err := f.pool.Exec(f.ctx, `UPDATE `+pgx.Identifier{f.schema, "content_items"}.Sanitize()+` SET created_at = now() - interval '25 hours'
 WHERE id = ANY($1)`, []string{abandoned, unpublished, old}); err != nil {
		t.Fatal(err)
	}
	n, err := f.s.ExpireDrafts(f.ctx)
	if err != nil || n != 1 {
		t.Fatalf("expired %d, %v; want the abandoned draft", n, err)
	}
	var left []string
	rows, err := f.pool.Query(f.ctx, `SELECT id FROM `+pgx.Identifier{f.schema, "content_items"}.Sanitize()+` ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	if left, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		t.Fatal(err)
	}
	if want := slices.Sorted(slices.Values([]string{unpublished, fresh, old})); !slices.Equal(left, want) {
		t.Fatalf("left %v, want %v", left, want)
	}
	if _, err := f.s.DeliverEvents(f.ctx); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(events, func(e Event) bool { return e.Type == EventContentPurged && e.ContentID == abandoned }) {
		t.Fatalf("no purge event for the abandoned draft: %+v", events)
	}
	if n, err := f.s.ExpireDrafts(f.ctx); err != nil || n != 0 {
		t.Fatalf("a second run expired %d, %v", n, err)
	}
	if err := (kinds.Config{Languages: []string{"en"}, Kinds: map[string]kinds.Kind{"blog": {Route: "b", Presentation: "post",
		Versions: kinds.VersionRules{Languages: "one"}, DraftTTL: -time.Hour}}}).Validate(); err == nil {
		t.Fatal("a negative draft_ttl was accepted")
	}
}

// Once a free_after time passes, ReleaseFree runs a write's effects for the
// content once (reads open it from that time on their own): a content update
// event goes out, and a second run finds nothing; moving free_after arms it
// again.
func TestReleaseFreeIntegration(t *testing.T) {
	f := newFixture(t, nil)
	var events []Event
	f.s.Subscribe(func(_ context.Context, batch []Event) error {
		events = append(events, batch...)
		return nil
	})
	free := f.now().Add(time.Hour)
	in := gallery("Soon free", "en")
	in.Access = &access.Rule{Level: access.TierLevel, Tiers: []string{"premium"}, FreeAfter: &free}
	id := f.create("staff", in)
	f.publish("staff", id, nil)
	plain := f.create("staff", gallery("Plain", "en"))
	f.publish("staff", plain, nil)
	applied := func() bool {
		t.Helper()
		var ok bool
		if err := f.pool.QueryRow(f.ctx, `SELECT bool_and(free_applied) FROM `+pgx.Identifier{f.schema, "content_facets"}.Sanitize()+` WHERE content_id = $1`, id).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if n, err := f.s.ReleaseFree(f.ctx); err != nil || n != 0 || applied() {
		t.Fatalf("released %d (%v) before free_after", n, err)
	}
	if d := f.read("", "main.test", id)[0].Versions[0].Decision; d.Allowed {
		t.Fatalf("anonymous opens it before free_after: %+v", d)
	}
	f.advance(2 * time.Hour)
	if n, err := f.s.ReleaseFree(f.ctx); err != nil || n != 1 || !applied() {
		t.Fatalf("released %d (%v) after free_after", n, err)
	}
	if n, err := f.s.ReleaseFree(f.ctx); err != nil || n != 0 {
		t.Fatalf("released again %d (%v)", n, err)
	}
	if _, err := f.s.DeliverEvents(f.ctx); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(events, func(e Event) bool { return e.Type == EventContentUpdated && e.ContentID == id }) {
		t.Fatalf("no update event for the released item: %+v", events)
	}
	later := f.now().Add(time.Hour)
	if err := f.s.Update(f.ctx, f.as("staff"), []Update{{ID: id, ContentPatch: ContentPatch{Access: &access.Rule{Level: access.PPV, FreeAfter: &later}}}}); err != nil {
		t.Fatal(err)
	}
	if applied() {
		t.Fatal("a moved free_after stays released")
	}
}
