package migrations

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"testing"
)

// Existing deployment checksums must survive introduction of a fresh profile.
// These files shipped in the combined lineage; new changes use new migrations.
func TestKeywordProfilePreservesCombinedMigrationIdentity(t *testing.T) {
	for name, want := range map[string]string{
		"0001_schema.up.sql":                "087b205cb4786bd7b1603b019b4ede03eae4b974d9626d29166bed2e3033aae8",
		"0002_search_dirty_revision.up.sql": "1e716a58d3d2d664dea90ace6b023bef67f7a4b24cf86575ec2c09cff9e2f562",
	} {
		data, err := fs.ReadFile(Postgres, name)
		if err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprintf("%x", sha256.Sum256(data)); got != want {
			t.Fatalf("applied migration %s changed: %s", name, got)
		}
	}
	files, err := fs.ReadDir(KeywordPostgres, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if _, err := fs.Stat(Postgres, file.Name()); err == nil {
			t.Fatalf("fresh profile reuses combined migration ID %s", file.Name())
		}
	}
}
