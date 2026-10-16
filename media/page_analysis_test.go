package media

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestPageAnalysisPreservesPlannerAndManualMetadata(t *testing.T) {
	meta := map[string]any{"external": "keep", "page": map[string]any{"logical_name": "007", "manual": "include", "future": map[string]any{"value": 4}}}
	a := PageAnalysis{Recipe: "test", SourceSHA256: strings.Repeat("a", 64), Status: "complete", Blank: "none", Confidence: "exact"}
	out, err := WithPageAnalysis(meta, a)
	if err != nil {
		t.Fatal(err)
	}
	if _, changed := meta["page"].(map[string]any)["analysis"]; changed {
		t.Fatal("the caller's metadata was mutated")
	}
	page := out["page"].(map[string]any)
	if page["manual"] != "include" || page["logical_name"] != "007" || !reflect.DeepEqual(page["future"], meta["page"].(map[string]any)["future"]) {
		t.Fatal("planner/manual metadata changed")
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatal(err)
	}
	got, ok := ReadPageAnalysis(decoded)
	if !ok || !reflect.DeepEqual(got, a) {
		t.Fatalf("round trip: %+v, %v", got, ok)
	}
}

func TestPageAnalysisDoesNotDisplaceFullOrMalformedMetadata(t *testing.T) {
	for _, meta := range []map[string]any{{"page": "future-format"}, {"notes": strings.Repeat("x", MaxMetaBytes)}} {
		if PageAnalysisFits(meta) {
			t.Fatal("analysis would displace user metadata")
		}
	}
}

func TestPageAnalysisOnlyMatchesOriginalPageUploads(t *testing.T) {
	k := Kind{Publish: &Publish{Pages: "originals/{name}"}}
	for _, tc := range []struct {
		file File
		want bool
	}{
		{File{Path: "originals/007.png"}, true},
		{File{Path: "cover.png"}, false},
		{File{Path: "originals/nested/007.png"}, false},
		{File{Path: "originals/007.webp", Preset: "thumb"}, false},
	} {
		if got := k.IsPageUpload(tc.file); got != tc.want {
			t.Fatalf("%+v: got %v, want %v", tc.file, got, tc.want)
		}
	}
	if (&Kind{}).IsPageUpload(File{Path: "originals/007.png"}) {
		t.Fatal("a non-reader kind cannot have page analysis")
	}
}

func TestPageAnalysisCapacityPreservesManifestHeadroom(t *testing.T) {
	k := &Kind{}
	m := &Manifest{size: editLimit - 2*pageAnalysisBytes}
	if got := k.PageAnalysisCapacity(m); got != 2 {
		t.Fatalf("capacity %d, want 2", got)
	}
	m.size = editLimit - pageAnalysisBytes + 1
	if got := k.PageAnalysisCapacity(m); got != 0 {
		t.Fatalf("optional evidence consumes reserved manifest space: %d", got)
	}
	m.size, m.Full = 0, true
	if got := k.PageAnalysisCapacity(m); got != 0 {
		t.Fatalf("full manifest allows evidence growth: %d", got)
	}
}
