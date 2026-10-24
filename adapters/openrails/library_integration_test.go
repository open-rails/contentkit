package openrails_test

import (
	"slices"
	"testing"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/discovery"
)

// The purchased library is a projection of OpenRails grants over ContentKit
// content. It needs no local product, price or sale record.
func TestLibraryReadsOpenRailsEntitlements(t *testing.T) {
	s := newShop(t)
	ctx := t.Context()
	buyer, stranger := s.customer(), s.customer()
	content := s.post(catalog.ContentInput{Access: &access.Rule{Level: access.PPV}, Creators: &[]catalog.Creator{}, Text: map[string]catalog.Text{"en": {Title: "Purchased collection"}}, Versions: []catalog.VersionInput{{Languages: []string{"en"}}, {Languages: []string{"ja"}}}})
	oneVersion := s.post(catalog.ContentInput{Access: &access.Rule{Level: access.PPV}, Creators: &[]catalog.Creator{}, Text: map[string]catalog.Text{"en": {Title: "One edition"}}})
	var version string
	if err := s.pool.QueryRow(ctx, `SELECT id FROM `+s.table("content_versions")+` WHERE tenant_id=$1 AND content_id=$2`, tenant, oneVersion).Scan(&version); err != nil {
		t.Fatal(err)
	}
	var contentVersions []string
	if err := s.pool.QueryRow(ctx, `SELECT array_agg(id::text ORDER BY id::text) FROM `+s.table("content_versions")+` WHERE tenant_id=$1 AND content_id=$2`, tenant, content).Scan(&contentVersions); err != nil {
		t.Fatal(err)
	}
	s.holds(buyer, ck(content), vk(oneVersion, version))
	var library discovery.LibraryPage
	s.get(s.discoveryAPI, stranger, "/me/library", &library)
	if len(library.Items) != 0 || !library.Complete {
		t.Fatalf("stranger library: %+v", library)
	}
	s.get(s.discoveryAPI, buyer, "/me/library", &library)
	if len(library.Items) != 2 || !library.Complete {
		t.Fatalf("buyer library: %+v", library)
	}
	for _, item := range library.Items {
		if item.ContentID == content {
			if !item.Whole || !slices.Equal(item.VersionIDs, contentVersions) {
				t.Fatalf("whole-content grant: %+v", item)
			}
		} else if item.ContentID == oneVersion {
			if item.Whole || !slices.Equal(item.VersionIDs, []string{version}) {
				t.Fatalf("edition grant: %+v", item)
			}
		} else {
			t.Fatalf("unexpected library item: %+v", item)
		}
	}
}
