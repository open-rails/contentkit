package catalog

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
)

func TestConfiguredAccessLevelsIntegration(t *testing.T) {
	f := newFixture(t, func(o *Options) {
		for name, k := range o.Kinds.Kinds {
			k.AccessLevels = []string{"public", "tier"}
			o.Kinds.Kinds[name] = k
		}
	})
	id := f.create("staff", gallery("Free", "en"))
	got := f.read("staff", "main.test", id)[0]
	premium := &access.Rule{Level: access.TierLevel, Tiers: []string{"premium"}}
	if err := f.s.Update(f.ctx, f.as("staff"), []Update{{ID: id, ContentPatch: ContentPatch{Access: premium}}}); err != nil {
		t.Fatal(err)
	}
	group := f.group("channel", "studio")
	for _, rule := range []*access.Rule{{Level: access.PPV}, {Level: access.GroupLevel, Group: group}, {Level: access.GroupPPV, Group: group}} {
		t.Run(string(rule.Level), func(t *testing.T) {
			in := gallery("Denied", "en")
			in.Access = rule
			if _, err := f.s.Create(f.ctx, f.as("staff"), []ContentInput{in}); !errors.Is(err, ErrInvalid) {
				t.Fatalf("create: %v", err)
			}
			if err := f.s.Update(f.ctx, f.as("staff"), []Update{{ID: id, ContentPatch: ContentPatch{Access: rule}}}); !errors.Is(err, ErrInvalid) {
				t.Fatalf("update: %v", err)
			}
			if _, err := f.s.PutVersions(f.ctx, f.as("staff"), []VersionPut{{ContentID: id, VersionID: got.Versions[0].ID, VersionInput: VersionInput{Access: rule}}}); !errors.Is(err, ErrInvalid) {
				t.Fatalf("version update: %v", err)
			}
			in.Access = nil
			in.Versions[0].Access = rule
			if _, err := f.s.Create(f.ctx, f.as("staff"), []ContentInput{in}); !errors.Is(err, ErrInvalid) {
				t.Fatalf("create version override: %v", err)
			}
			for _, imported := range []ImportContent{
				{ID: contentref.NewID(), Kind: "gallery", State: StateDraft, Access: rule},
				{ID: contentref.NewID(), Kind: "gallery", State: StateDraft, Versions: []ImportVersion{{ID: contentref.NewID(), Languages: []string{"en"}, Access: rule}}},
			} {
				err := pgx.BeginFunc(f.ctx, f.pool, func(tx pgx.Tx) error { return f.s.Import(f.ctx, tx, []ImportContent{imported}) })
				if !errors.Is(err, ErrInvalid) {
					t.Fatalf("import: %v", err)
				}
			}
		})
	}
	if final := f.read("staff", "main.test", id)[0]; final.Access.Level != access.TierLevel || final.Versions[0].Access != nil {
		t.Fatalf("denied writes changed content: %+v", final)
	}
}

func TestConfiguredCommentsIntegration(t *testing.T) {
	f := newFixture(t, func(o *Options) {
		k := o.Kinds.Kinds["gallery"]
		k.CommentModes = []string{"open"}
		o.Kinds.Kinds["gallery"] = k
	})
	id := f.create("staff", gallery("Open comments", "en"))
	for _, mode := range []string{"closed", "hidden"} {
		in := gallery("Invalid comments", "en")
		in.Comments = mode
		if _, err := f.s.Create(f.ctx, f.as("staff"), []ContentInput{in}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("create %s: %v", mode, err)
		}
		if err := f.s.Update(f.ctx, f.as("staff"), []Update{{ID: id, ContentPatch: ContentPatch{Comments: &mode}}}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("update %s: %v", mode, err)
		}
		err := pgx.BeginFunc(f.ctx, f.pool, func(tx pgx.Tx) error {
			return f.s.Import(f.ctx, tx, []ImportContent{{ID: contentref.NewID(), Kind: "gallery", State: StateDraft, Comments: mode}})
		})
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("import %s: %v", mode, err)
		}
	}
	if got := f.read("staff", "main.test", id)[0].Comments; got != "open" {
		t.Fatalf("comments changed to %s", got)
	}
}

func TestConfiguredDefaultsIntegration(t *testing.T) {
	f := newFixture(t, func(o *Options) {
		k := o.Kinds.Kinds["gallery"]
		k.AccessLevels, k.CommentModes = []string{"tier"}, []string{"closed"}
		o.Kinds.Kinds["gallery"] = k
	})
	id := f.create("staff", gallery("Premium by default", "en"))
	got := f.read("staff", "main.test", id)[0]
	if got.Access.Level != access.TierLevel || len(got.Access.Tiers) != 1 || got.Access.Tiers[0] != "premium" || got.Comments != "closed" {
		t.Fatalf("configured defaults: access=%+v comments=%s", got.Access, got.Comments)
	}
	imported := ImportContent{ID: contentref.NewID(), Kind: "gallery", State: StateDraft,
		Versions: []ImportVersion{{ID: contentref.NewID(), Languages: []string{"en"}}}}
	if err := pgx.BeginFunc(f.ctx, f.pool, func(tx pgx.Tx) error {
		return f.s.Import(f.ctx, tx, []ImportContent{imported})
	}); err != nil {
		t.Fatalf("import with configured defaults: %v", err)
	}
	got = f.read("staff", "main.test", imported.ID)[0]
	if got.Access.Level != access.TierLevel || got.Comments != "closed" {
		t.Fatalf("imported defaults: access=%+v comments=%s", got.Access, got.Comments)
	}
}

func TestConfiguredPolicyAfterContentExistsIntegration(t *testing.T) {
	f := newFixture(t, nil)
	closed := gallery("Restore comments", "en")
	closed.Comments = "hidden"
	closedID := f.create("staff", closed)
	if err := f.s.Delete(f.ctx, f.as("staff"), []string{closedID}); err != nil {
		t.Fatal(err)
	}
	paid := gallery("Old offer", "en")
	paid.Access = &access.Rule{Level: access.PPV}
	paidID := f.create("staff", paid)
	version := gallery("Old version offer", "en")
	version.Versions[0].Access = &access.Rule{Level: access.PPV}
	versionID := f.create("staff", version)
	vid := f.read("staff", "main.test", versionID)[0].Versions[0].ID

	k := f.s.cfg.Kinds["gallery"]
	k.AccessLevels, k.CommentModes = []string{"public", "tier"}, []string{"open"}
	f.s.cfg.Kinds["gallery"] = k
	for _, id := range []string{paidID, versionID} {
		if err := f.s.Publish(f.ctx, f.as("staff"), []Publish{{ContentID: id}}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("publishing forbidden access %s: %v", id, err)
		}
	}
	label := "Keep old access"
	if _, err := f.s.PutVersions(f.ctx, f.as("staff"), []VersionPut{{ContentID: versionID, VersionID: vid, VersionInput: VersionInput{Label: &label}}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("retaining a forbidden version override: %v", err)
	}
	if err := f.s.Restore(f.ctx, f.as("staff"), []string{closedID}); err != nil {
		t.Fatalf("restore fixed comments: %v", err)
	}
	if got := f.read("staff", "main.test", closedID)[0]; got.State != StateDraft || got.Comments != "open" {
		t.Fatalf("restored state=%s comments=%s", got.State, got.Comments)
	}
	if err := f.s.Delete(f.ctx, f.as("staff"), []string{paidID}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Restore(f.ctx, f.as("staff"), []string{paidID}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("restore forbidden access: %v", err)
	}
}

func TestConfiguredPolicyPreservesImportedTombstonesIntegration(t *testing.T) {
	f := newFixture(t, func(o *Options) {
		k := o.Kinds.Kinds["gallery"]
		k.AccessLevels, k.CommentModes = []string{"public", "tier"}, []string{"open"}
		o.Kinds.Kinds["gallery"] = k
	})
	makeItem := func(state, comments string, level access.Level) ImportContent {
		return ImportContent{ID: contentref.NewID(), Kind: "gallery", State: state, Comments: comments,
			Access: &access.Rule{Level: level}, Text: map[string]Text{"en": {Title: "Historical gallery"}},
			Versions: []ImportVersion{{ID: contentref.NewID(), State: StatePublished, Languages: []string{"en"}}}}
	}
	importOne := func(in ImportContent) error {
		return pgx.BeginFunc(f.ctx, f.pool, func(tx pgx.Tx) error {
			return f.s.Import(f.ctx, tx, []ImportContent{in})
		})
	}
	tombstone := makeItem(StateDeleted, "hidden", access.PPV)
	tombstone.Versions[0].Access = &access.Rule{Level: access.PPV}
	if err := importOne(tombstone); err != nil {
		t.Fatalf("historical tombstone import: %v", err)
	}
	var state, comments, itemLevel, versionLevel string
	if err := f.pool.QueryRow(f.ctx, `SELECT c.state, c.comments, c.access->>'level', v.access->>'level'
 FROM `+f.s.t.items+` c JOIN `+f.s.t.versions+` v ON v.tenant_id=c.tenant_id AND v.content_id=c.id
 WHERE c.tenant_id=$1 AND c.id=$2`, tenant, tombstone.ID).Scan(&state, &comments, &itemLevel, &versionLevel); err != nil {
		t.Fatal(err)
	}
	if state != StateDeleted || comments != "hidden" || itemLevel != "ppv" || versionLevel != "ppv" {
		t.Fatalf("historical fields changed: %s %s %s %s", state, comments, itemLevel, versionLevel)
	}
	if err := f.s.Restore(f.ctx, f.as("staff"), []string{tombstone.ID}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("restored forbidden access: %v", err)
	}
	// A failed restore must not commit even the fixed-mode comment repair.
	if err := f.pool.QueryRow(f.ctx, `SELECT comments FROM `+f.s.t.items+` WHERE tenant_id=$1 AND id=$2`, tenant, tombstone.ID).Scan(&comments); err != nil || comments != "hidden" {
		t.Fatalf("failed restore changed historical comments to %s: %v", comments, err)
	}
	tombstone.Access, tombstone.Versions[0].Access = &access.Rule{Level: access.Public}, &access.Rule{Level: access.Public}
	if err := importOne(tombstone); err != nil {
		t.Fatalf("repair retained access: %v", err)
	}
	if err := f.s.Restore(f.ctx, f.as("staff"), []string{tombstone.ID}); err != nil {
		t.Fatalf("restore repaired tombstone: %v", err)
	}
	if got := f.read("staff", "main.test", tombstone.ID)[0]; got.Comments != "open" || got.State != StateDraft {
		t.Fatalf("restored fields: comments=%s state=%s", got.Comments, got.State)
	}
	for _, in := range []ImportContent{
		makeItem(StateDraft, "hidden", access.Public),
		makeItem(StateDraft, "open", access.PPV),
		makeItem(StateDeleted, "invalid", access.Public),
		makeItem(StateDeleted, "hidden", access.Level("invalid")),
	} {
		if err := importOne(in); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid import state=%s comments=%s access=%s: %v", in.State, in.Comments, in.Access.Level, err)
		}
	}
	item := makeItem(StateDraft, "open", access.Public)
	item.Versions = append(item.Versions, ImportVersion{ID: contentref.NewID(), State: StateDeleted, Languages: []string{"en"}, Access: &access.Rule{Level: access.PPV}})
	if err := importOne(item); err != nil {
		t.Fatalf("deleted version history: %v", err)
	}
	if _, err := f.s.PutVersions(f.ctx, f.as("staff"), []VersionPut{{ContentID: item.ID, VersionID: item.Versions[1].ID, VersionInput: VersionInput{State: StateDraft}}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("restored forbidden version access: %v", err)
	}
	item.ID, item.Versions[0].ID, item.Versions[1].ID = contentref.NewID(), contentref.NewID(), contentref.NewID()
	item.Versions[1].State = StatePublished
	if err := importOne(item); !errors.Is(err, ErrInvalid) {
		t.Fatalf("live version accepted forbidden access: %v", err)
	}
}
