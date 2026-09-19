package migrations

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

var parentHeader = regexp.MustCompile(`^[ \t]*--[ \t]*parent:`)

// bodyDigest is what a migratekit ledger records: the file without its
// `-- parent:` header line, so relinking never changes an applied identity.
func bodyDigest(content string) string {
	if i := strings.IndexByte(content, '\n'); i >= 0 && parentHeader.MatchString(content[:i]) {
		content = content[i+1:]
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
}

// Applied migrations of every lineage keep their ledger identity; a change to
// any of them is a new migration.
func TestAppliedMigrationsKeepTheirLedgerIdentity(t *testing.T) {
	for _, lineage := range []struct {
		name  string
		files fs.FS
		want  map[string]string
	}{
		{"keyword", Postgres, map[string]string{
			"0001_keyword_schema.up.sql": "68ee97ffe4018b63bfebe954f6fe22eac71f250b861b185f5883d87dea922406",
			"0002_keyword_fields.up.sql": "ef68ec7f801c39594fc54901bf8fe7c762781acf97560bc9f114869179277550",
		}},
		{"social", Social, map[string]string{
			"0001_social_core.up.sql":         "c9323cfddb2da2020ec07bb55aa2ba0aa779abdceb59313859bb95972a89c26c",
			"0002_comments_latest_idx.up.sql": "b431d777d4b77ec4eddaef06ca11553aeff2632dbe043dc1b390ebb7a62d5a46",
		}},
		{"legacy", LegacyPostgres, map[string]string{
			"0001_schema.up.sql":                "087b205cb4786bd7b1603b019b4ede03eae4b974d9626d29166bed2e3033aae8",
			"0002_search_dirty_revision.up.sql": "1e716a58d3d2d664dea90ace6b023bef67f7a4b24cf86575ec2c09cff9e2f562",
			"0003_keyword_fields.up.sql":        "ef68ec7f801c39594fc54901bf8fe7c762781acf97560bc9f114869179277550",
		}},
		{"signal", SignalClickHouse, map[string]string{
			"0001_signal_baseline.up.sql":  "1832a7dc9dac309738fc7998e6cc91f221f4dce1cc70c6e778b183aa0de057c0",
			"0002_canonical_events.up.sql": "eca60e6d64cc50abd2af25c8d2604d54243fe112e36c5f966904c39d5f512f0d",
			"0003_subject_erasure.up.sql":  "dc3984603a0c8aa4e48bbaff99cb80a4e99d94ec4bd12ca3bb1bad63b256a979",
			"0004_exposures.up.sql":        "55f6c6d8d9fcc0aba3895fff14d26404d0f43a9ce11f43eb0305d22af79b11b2",
			"0005_content_refs.up.sql":     "a97b38f508005b8245db2ab8f6a715a57885e3feaf7e714ad8265b851dd82beb",
			"0006_view_recency.up.sql":     "9f4490f0a92f190b989bc3cca030c17bcbc76bbd02f7d98b37028bd1b9bbd225",
		}},
	} {
		for name, want := range lineage.want {
			data, err := fs.ReadFile(lineage.files, name)
			if err != nil {
				t.Fatalf("%s/%s: %v", lineage.name, name, err)
			}
			if got := bodyDigest(string(data)); got != want {
				t.Fatalf("applied migration %s/%s changed: %s", lineage.name, name, got)
			}
		}
	}
	// A migration is identified by its filename within one ledger; the two
	// Postgres lineages must never share one.
	keyword, err := fs.ReadDir(Postgres, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range keyword {
		if _, err := fs.Stat(LegacyPostgres, file.Name()); err == nil {
			t.Fatalf("keyword profile reuses legacy migration ID %s", file.Name())
		}
	}
}
