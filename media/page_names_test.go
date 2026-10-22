package media

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func pageOps(t *testing.T) *opRun {
	t.Helper()
	reg, err := NewRegistry(Config{Namespace: "test", Kinds: []Kind{{Name: "gallery", Versions: true, KeepOriginals: true,
		Uploads: []Upload{{Path: "originals/{name}", Types: []string{"image/png"}, MaxBytes: 1024}},
		Private: []Private{{Name: "read", From: "originals/{name}", To: "read/{name}.webp", Image: &Image{Width: 64}}},
		Publish: &Publish{Pages: "originals/{name}", Renditions: []string{"read"}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	k, err := reg.Kind("gallery")
	if err != nil {
		t.Fatal(err)
	}
	return &opRun{k: k, m: &Manifest{}, objects: map[string]Object{}}
}

func putPage(t *testing.T, o *opRun, name, source, create string) Op {
	t.Helper()
	op := Op{Op: OpPut, Path: name, SourceName: source, Blob: NewStaged(), CreateID: create}
	o.objects[op.Blob] = Object{ContentType: "image/png", Size: 10}
	if err := o.put(op); err != nil {
		t.Fatal(err)
	}
	return op
}

func TestCanonicalPageNamesRetainGapsRangesAndLargeNumbers(t *testing.T) {
	for _, tt := range []struct {
		in, want string
		from, to int
	}{
		{"1.png", "001", 1, 1}, {"003.jpg", "003", 3, 3}, {"10.png", "010", 10, 10},
		{"1000.png", "1000", 1000, 1000}, {"02-3.jpg", "002-003", 2, 3},
	} {
		name, from, to, ok := CanonicalPageName(tt.in)
		if !ok || name != tt.want || from != tt.from || to != tt.to {
			t.Fatalf("%s => %s %d-%d %v", tt.in, name, from, to, ok)
		}
	}
	for _, in := range []string{"4-2.png", "one.png", "01_1.png", "99999999999999999999.png"} {
		if _, _, _, ok := CanonicalPageName(in); ok {
			t.Fatalf("ambiguous name accepted: %s", in)
		}
	}
}

func TestPagePutPreservesNamesAndCreateIdentity(t *testing.T) {
	o := pageOps(t)
	first := putPage(t, o, "originals/1.png", "1.png", "")
	second := putPage(t, o, "originals/001.png", "001.png", "")
	if len(o.m.Files) != 2 || o.m.Files[0].Path != "originals/1.png" || o.m.Files[1].Path != "originals/001.png" {
		t.Fatalf("source overwritten: %+v", o.m.Files)
	}
	if o.m.Files[0].Source() != first.Blob || o.m.Files[1].Source() != second.Blob {
		t.Fatal("source bytes lost")
	}
	page, _ := pageMetadata(o.m.Files[1].Meta)
	if page["name"] != "001" || page["source_name"] != "001.png" {
		t.Fatalf("provenance: %+v", page)
	}
	create := uuid.NewString()
	third := putPage(t, o, "originals/001.png", "1.png", create)
	if len(o.m.Files) != 3 {
		t.Fatal("new create replaced a page")
	}
	if done, err := o.created(third); err != nil || !done {
		t.Fatalf("allocated retry not found: %v %v", done, err)
	}
	// Natural filename order may insert the collision allocation before an
	// existing page; creation identity must survive independently of position.
	for _, f := range o.m.Files {
		if f.CreateID == create {
			if info := uploadInfo(f, o.k); info.CreateID != create || f.Source() != third.Blob {
				t.Fatal("editor reply lost allocation identity")
			}
			return
		}
	}
	t.Fatal("created page was not retained")
}

func TestPageMetadataUpdatePreservesManualSelectionAndAnalysis(t *testing.T) {
	o := pageOps(t)
	op := putPage(t, o, "originals/1.png", "1.png", "")
	f := &o.m.Files[0]
	page, _ := pageMetadata(f.Meta)
	page["mode"] = "exclude"
	page["included"] = false
	page["analysis"] = map[string]any{"recipe": "proof", "source_sha256": "unchanged"}
	f.Meta["page"] = page
	before, _ := json.Marshal(page)
	op.Path = f.Path
	op.SourceName = ""
	op.Meta = map[string]any{"caption": "changed"}
	if err := o.put(op); err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(o.m.Files[0].Meta["page"])
	if string(before) != string(after) || len(o.m.Files) != 1 || o.m.Files[0].Meta["caption"] != "changed" {
		t.Fatalf("metadata edit erased review: %+v", o.m.Files)
	}
}

func TestPageReplacementWithoutSourceNameKeepsItsIdentity(t *testing.T) {
	o := pageOps(t)
	putPage(t, o, "originals/1.png", "1.png", "")
	putPage(t, o, "originals/1.png", "", "")
	if len(o.m.Files) != 1 || sourceName(o.m.Files[0].Meta, "") != "1.png" {
		t.Fatalf("ordinary replacement became an extra page: %+v", o.m.Files)
	}
}

func TestExplicitPageReplacementDoesNotAllocateAnotherPage(t *testing.T) {
	o := pageOps(t)
	putPage(t, o, "originals/1.png", "1.png", "")
	op := Op{Op: OpPut, Path: "originals/1.png", SourceName: "replacement.png", Blob: NewStaged(), Replace: true}
	o.objects[op.Blob] = Object{ContentType: "image/png", Size: 10}
	if err := o.put(op); err != nil {
		t.Fatal(err)
	}
	if len(o.m.Files) != 1 || o.m.Files[0].Path != "originals/1.png" || o.m.Files[0].Source() != op.Blob {
		t.Fatalf("explicit replacement appended a page: %+v", o.m.Files)
	}
	op.CreateID = uuid.NewString()
	if err := op.validate(); err == nil {
		t.Fatal("ambiguous create and replace accepted")
	}
}

func TestLegacyPageReviewKeepsPhysicalAndReaderIdentity(t *testing.T) {
	for _, name := range []string{"01_1", "1"} {
		t.Run(name, func(t *testing.T) {
			o := pageOps(t)
			blob := NewStaged()
			o.objects[blob] = Object{ContentType: "image/png", Size: 10}
			o.m.Files = []File{{Path: "originals/" + name + ".png", Staged: blob, Type: "image/png", Size: 10,
				Meta: map[string]any{"page": map[string]any{"analysis": map[string]any{"recipe": "existing"}}}}}
			o.trusted = true
			err := o.put(Op{Op: OpPut, Path: o.m.Files[0].Path, Blob: blob,
				Meta: map[string]any{"page": map[string]any{"mode": "exclude", "included": false}}})
			if err != nil {
				t.Fatal(err)
			}
			page, _ := pageMetadata(o.m.Files[0].Meta)
			if len(o.m.Files) != 1 || o.m.Files[0].Path != "originals/"+name+".png" || page["name"] != nil || page["mode"] != "exclude" {
				t.Fatalf("legacy reader identity changed: %+v", o.m.Files)
			}
		})
	}
}

func TestPageAllocationReservesRenamedPhysicalPaths(t *testing.T) {
	o := pageOps(t)
	first := putPage(t, o, "originals/1.png", "1.png", "")
	if err := o.rename(o.m.Files[0], "originals/002.png"); err != nil {
		t.Fatal(err)
	}
	second := putPage(t, o, "originals/001.png", "001.png", uuid.NewString())
	renamed, hasRenamed := o.m.Get("originals/002.png")
	added, hasAdded := o.m.Get("originals/001.png")
	if len(o.m.Files) != 2 || !hasRenamed || !hasAdded || renamed.Source() != first.Blob || added.Source() != second.Blob {
		t.Fatalf("renamed original was overwritten: %+v", o.m.Files)
	}
}

func TestCopiedPageGetsIndependentIdentity(t *testing.T) {
	o := pageOps(t)
	putPage(t, o, "originals/1.png", "1.png", "")
	src := o.m.Files[0]
	page, _ := pageMetadata(src.Meta)
	page["mode"] = "exclude"
	page["included"] = false
	src.Meta["page"] = page
	if err := o.copy(Op{Op: OpCopy, To: "originals/2.png", From: &CopyFrom{ID: "source", Path: src.Path}}, []File{src}); err != nil {
		t.Fatal(err)
	}
	copy := o.m.Files[1]
	meta, _ := pageMetadata(copy.Meta)
	if copy.Path != "originals/2.png" || meta["name"] != "2" || meta["mode"] != nil || meta["included"] != nil {
		t.Fatalf("copied stale identity/override: %+v", copy)
	}
}

func TestChangedSourceClearsArchiveCompletionAndChecksAllocatedPermission(t *testing.T) {
	o := pageOps(t)
	putPage(t, o, "originals/1.png", "1.png", "")
	o.m.Files[0].Meta["archive_complete"] = true
	o.m.Files[0].Meta["archive_complete_sha256"] = strings.Repeat("a", 64)
	putPage(t, o, "originals/1.png", "1.png", "")
	for _, key := range archiveStateKeys {
		if _, ok := o.m.Files[0].Meta[key]; ok {
			t.Fatal("new bytes inherited import completion")
		}
	}
	denied := errors.New("path denied")
	o.authorizePath = func(p string) error {
		if p == "originals/1 (2).png" {
			return denied
		}
		return nil
	}
	op := Op{Op: OpPut, Path: "originals/1.png", SourceName: "another-name.png", Blob: NewStaged()}
	o.objects[op.Blob] = Object{ContentType: "image/png", Size: 10}
	if err := o.put(op); !errors.Is(err, denied) || len(o.m.Files) != 1 {
		t.Fatalf("allocation escaped permission: %v %+v", err, o.m.Files)
	}
}

func TestClientMetadataCannotForgePageEvidence(t *testing.T) {
	input := []Op{{Op: OpPut, Meta: map[string]any{"page": map[string]any{"mode": "include"}, "archive_complete": true, "publishing_archives": "forged", "caption": "kept"}}}
	clean := clientUploadOps(input)
	if !reflect.DeepEqual(clean[0].Meta, map[string]any{"caption": "kept"}) {
		t.Fatalf("reserved metadata accepted: %+v", clean)
	}
	if input[0].Meta["page"] == nil {
		t.Fatal("caller input mutated")
	}
}
