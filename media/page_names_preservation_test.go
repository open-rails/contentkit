package media

import (
	"reflect"
	"strings"
	"testing"
)

func TestPageBatchNamesOnlyCleanRedundantConventions(t *testing.T) {
	for _, tc := range []struct {
		name     string
		in, want []string
	}{
		{"numeric widths", []string{"1.png", "2.png", "10.png"}, []string{"01.png", "02.png", "10.png"}},
		{"numeric pages with cover", []string{"cover.png", "1.png", "10.png"}, []string{"cover.png", "01.png", "10.png"}},
		{"shared prefix", []string{"sex_01.png", "sex_02.png"}, []string{"01.png", "02.png"}},
		{"shared suffix", []string{"01_n.png", "02_n.png"}, []string{"01.png", "02.png"}},
		{"repeated numbers", []string{"01_1.png", "02_2.png"}, []string{"01.png", "02.png"}},
		{"custom labels", []string{"00a.png", "00b.png", "06-07.png", "cover.png"}, []string{"00a.png", "00b.png", "06-07.png", "cover.png"}},
		{"variant numbers", []string{"01_2.png", "02_3.png"}, []string{"01_2.png", "02_3.png"}},
		{"dotted labels", []string{"1.1.png", "10.1.png"}, []string{"1.1.png", "10.1.png"}},
		{"no common wrapper", []string{"sex_01.png", "credits.png"}, []string{"sex_01.png", "credits.png"}},
		{"single name", []string{"sex_01.png"}, []string{"sex_01.png"}},
		{"already consistent", []string{"1.png", "2.png"}, []string{"1.png", "2.png"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := CleanPageBatchNames(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPageNamesKeepCustomFilenamesAndHashOnRename(t *testing.T) {
	o := pageOps(t)
	for _, name := range []string{"00a.png", "00b.png", "06-07.png", "Afterword.png"} {
		putPage(t, o, "originals/"+name, name, "")
		if got := o.m.Files[len(o.m.Files)-1].Path; got != "originals/"+name {
			t.Fatalf("renamed %q to %q", name, got)
		}
	}
	order := []string{o.m.Files[3].Path, o.m.Files[0].Path, o.m.Files[1].Path, o.m.Files[2].Path}
	index := 0
	if err := o.apply(0, Op{Op: OpMove, Path: o.m.Files[3].Path, Index: &index}); err != nil {
		t.Fatal(err)
	}
	for i, expected := range order {
		if o.m.Files[i].Path != expected {
			t.Fatalf("reorder renamed a file: %+v", o.m.Files)
		}
	}
	before := o.m.Files[0]
	before.Blob, before.Staged = "sha256-"+strings.Repeat("a", 64), ""
	o.m.Files[0] = before
	id := o.k.PageID(before)
	if id == "" {
		t.Fatal("missing source hash")
	}
	if err := o.rename(before, "originals/Afterword - final.png"); err != nil {
		t.Fatal(err)
	}
	if o.k.PageID(o.m.Files[0]) != id {
		t.Fatal("rename changed the source hash")
	}
	meta, _ := pageMetadata(o.m.Files[0].Meta)
	if meta["name"] != "Afterword - final" || meta["source_name"] != "Afterword.png" {
		t.Fatalf("rename lost current name or original provenance: %+v", meta)
	}
}

func TestDuplicatePageNamesKeepReadableSuffix(t *testing.T) {
	taken := map[string]bool{"Cover": true}
	if got := UniquePageName("cover", taken); got != "cover (2)" {
		t.Fatal(got)
	}
	if got := UniquePageName("cover", taken); got != "cover (3)" {
		t.Fatal(got)
	}
}

func TestPageRenameRefusesCaseInsensitiveCollision(t *testing.T) {
	o := pageOps(t)
	putPage(t, o, "originals/Cover.png", "Cover.png", "")
	putPage(t, o, "originals/other.png", "other.png", "")
	if err := o.rename(o.m.Files[1], "originals/cover.png"); err == nil {
		t.Fatal("rename allowed colliding ZIP filenames")
	}
	if o.m.Files[0].Path != "originals/Cover.png" || o.m.Files[1].Path != "originals/other.png" {
		t.Fatalf("failed rename changed files: %+v", o.m.Files)
	}
	if err := o.rename(o.m.Files[0], "originals/cover.png"); err != nil {
		t.Fatalf("case-only rename of the same file failed: %v", err)
	}
}
