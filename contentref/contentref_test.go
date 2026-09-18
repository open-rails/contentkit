package contentref

import (
	"encoding/json"
	"testing"
)

func TestContentRefKeyVersionAndJSON(t *testing.T) {
	work := New("doujins", "gallery", "g1")
	version := NewVersion("doujins", "gallery", "g1", "v1")
	if work.Version() != "" || version.Version() != "v1" || work.Key() == version.Key() || !version.Content().Equal(work) {
		t.Fatalf("work=%s version=%s", work, version)
	}
	if version.WithVersion("").ContentVersionID != nil || version.Key().Ref().Version() != "v1" {
		t.Fatal("version round trip")
	}
	if work.String() != "doujins/gallery/g1" || version.String() != "doujins/gallery/g1@v1" {
		t.Fatalf("%s %s", work, version)
	}
	for _, bad := range []ContentRef{{}, New("", "gallery", "g1"), New("t", "", "g1"), New("t", "gallery", ""), {TenantID: "t", ContentKind: "k", ContentID: "i", ContentVersionID: new(string)}} {
		if bad.Validate() == nil {
			t.Fatalf("%+v must be invalid", bad)
		}
	}
	b, err := json.Marshal([]ContentRef{work, version})
	if err != nil || string(b) != `[{"tenant_id":"doujins","content_kind":"gallery","content_id":"g1"},{"tenant_id":"doujins","content_kind":"gallery","content_id":"g1","content_version_id":"v1"}]` {
		t.Fatalf("json: %s %v", b, err)
	}
	var back []ContentRef
	if err := json.Unmarshal(b, &back); err != nil || !back[1].Equal(version) || !back[0].Equal(work) {
		t.Fatalf("json round trip: %+v %v", back, err)
	}
}
