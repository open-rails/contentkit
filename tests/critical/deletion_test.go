package critical_test

import (
	"testing"
	"time"

	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/importer"
)

func TestVersionDeletionDerivesParent(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	startPublicationRuntime(t, a)
	admin := a.engine.Client()
	t.Run("CK-J08-S01_deleting_one_version_preserves_siblings_and_parent", func(t *testing.T) {
		item, err := admin.PostContents(t.Context(), catalog.ContentInput{Kind: "gallery", Versions: []catalog.VersionInput{{Languages: []string{"en"}, Text: map[string]catalog.VersionText{"en": {Title: "Two editions"}}}, {Languages: []string{"de"}, Text: map[string]catalog.VersionText{"en": {Title: "Two editions"}}}}})
		must(t, err)
		if len(item.Versions) != 2 {
			t.Fatal("fixture did not create two versions")
		}
		_, err = admin.PostContentPublish(t.Context(), item.ID, catalog.PublishInput{})
		must(t, err)
		for _, version := range item.Versions {
			awaitEditionPublication(t, a, admin, item.ID, version.ID)
		}
		_, err = a.client("").GetVersion(t.Context(), item.Versions[0].ID, ck.GetVersionQuery{})
		must(t, err)
		must(t, admin.DeleteContentVersion(t.Context(), item.ID, item.Versions[0].ID))
		got, err := admin.GetContent(t.Context(), item.ID, ck.GetContentQuery{})
		must(t, err)
		if got.State == "deleted" {
			t.Fatal("deleting one edition deleted its parent")
		}
		sibling, err := admin.GetContentVersion(t.Context(), item.ID, item.Versions[1].ID, ck.GetContentVersionQuery{})
		must(t, err)
		if sibling.State == "deleted" || sibling.ID != item.Versions[1].ID {
			t.Fatal("sibling edition was deleted or replaced")
		}
		deleted, err := admin.GetContentVersion(t.Context(), item.ID, item.Versions[0].ID, ck.GetContentVersionQuery{})
		must(t, err)
		if deleted.State != "deleted" {
			t.Fatal("requested edition was not actually soft-deleted")
		}
		_, err = a.client("").GetVersion(t.Context(), item.Versions[0].ID, ck.GetVersionQuery{})
		requireError(t, err, 404, "not_found")
		_, err = a.client("").GetVersion(t.Context(), item.Versions[1].ID, ck.GetVersionQuery{})
		must(t, err)
	})
	t.Run("CK-J08-S02_last_version_also_deletes_parent", func(t *testing.T) {
		item := publishedArticle(t, a, "last edition")
		if len(item.Versions) != 1 {
			t.Fatal("hidden-version fixture is not one edition")
		}
		must(t, admin.DeleteContentVersion(t.Context(), item.ID, item.Versions[0].ID))
		_, err := a.client("").GetContent(t.Context(), item.ID, ck.GetContentQuery{})
		requireError(t, err, 404, "not_found")
		deleted, err := admin.GetManage(t.Context(), ck.GetManageQuery{State: "deleted", Ids: []string{item.ID}})
		must(t, err)
		if len(deleted) != 1 || deleted[0].State != "deleted" || deleted[0].DeletedAt == nil {
			t.Fatalf("last version did not tombstone its parent in management: %+v", deleted)
		}
	})
	t.Run("CK-J08-S03_restore_version_recovers_only_derived_parent", func(t *testing.T) {
		item := publishedArticle(t, a, "restore last edition")
		version := item.Versions[0].ID
		must(t, admin.DeleteContentVersion(t.Context(), item.ID, version))
		_, err := admin.PatchContentVersion(t.Context(), item.ID, version, catalog.VersionInput{State: "published"})
		must(t, err)
		awaitEditionPublication(t, a, admin, item.ID, version)
		got, err := a.client("").GetContent(t.Context(), item.ID, ck.GetContentQuery{})
		must(t, err)
		if !got.Live || got.DeletedAt != nil || got.ID != item.ID || got.SelectedVersionID != version {
			t.Fatalf("edition restore did not recover its derived parent: %+v", got)
		}
	})
	t.Run("legacy_parent_restore_does_not_bypass_last_version_deletion", func(t *testing.T) {
		prior := time.Now().Add(-time.Hour)
		imported, err := admin.PostImport(t.Context(), importer.Batch{Source: "go-restore-contract", Contents: []importer.ContentRecord{importer.ContentRecord{ExternalID: "historical-parent", Kind: "article", State: "deleted", PublishedAt: &prior, PublishAt: &prior, DeletedAt: &prior, Versions: []importer.VersionRecord{{Languages: []string{"en"}, ExternalID: "historical-parent" + "/version", Text: map[string]catalog.VersionText{"en": {Title: "Historical restore", Body: "Preserved imported manuscript"}}}}}}})
		must(t, err)
		if len(imported.Failed) != 0 {
			t.Fatalf("historical import failed: %+v", imported.Failed)
		}
		id := imported.IDs["content"]["historical-parent"]
		if id == "" {
			t.Fatal("historical content identity missing")
		}
		_, err = w.apps[1].engine.Client().PostContentRestore(t.Context(), id)
		requireError(t, err, 404, "not_found")
		_, err = a.client("").GetContent(t.Context(), id, ck.GetContentQuery{})
		requireError(t, err, 404, "not_found")
		restored, err := admin.PostContentRestore(t.Context(), id)
		must(t, err)
		for _, version := range restored.Versions {
			awaitEditionPublication(t, a, admin, id, version.ID)
		}
		visible, err := a.client("").GetContent(t.Context(), id, ck.GetContentQuery{})
		must(t, err)
		if restored.ID != id || !visible.Live || visible.Body != "Preserved imported manuscript" {
			t.Fatalf("parent restore lost public content: %+v", visible)
		}
		derived := publishedArticle(t, a, "Last edition requires edition restore")
		must(t, admin.DeleteContentVersion(t.Context(), derived.ID, derived.Versions[0].ID))
		_, err = admin.PostContentRestore(t.Context(), derived.ID)
		requireError(t, err, 409, "conflict")
		_, err = a.client("").GetContent(t.Context(), derived.ID, ck.GetContentQuery{})
		requireError(t, err, 404, "not_found")
	})
}
