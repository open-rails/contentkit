package media

import (
	"github.com/google/uuid"
	"github.com/open-rails/contentkit/media/layout"
	"testing"
)

func TestScopedFolderRejectsLegacyPartitionAlias(t *testing.T) {
	reg, err := NewRegistry(Config{Scope: layout.Scope{TenantID: uuid.NewString(), AppID: uuid.NewString()}, Kinds: []Kind{{Name: "user", Account: true, Uploads: []Upload{{Path: "avatar", Types: []string{"image/png"}, MaxBytes: 100}}}}})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := reg.Ref("user", "01920000-0000-7000-8000-000000000001")
	if err != nil {
		t.Fatal(err)
	}
	item, _ := reg.Item(ref)
	if _, err = reg.folderItem(item.Prefix()); err != nil {
		t.Fatal(err)
	}
	alias := layout.Prefix(ref.TenantID, ref.ContentKind, ref.ContentID, "")
	if _, err = reg.folderItem(alias); err == nil {
		t.Fatal("legacy partition alias reached scoped item")
	}
}
