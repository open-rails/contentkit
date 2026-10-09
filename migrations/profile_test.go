package migrations

import (
	"io/fs"
	"slices"
	"testing"

	"github.com/open-rails/migratekit"
)

func TestMigrationChains(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files fs.FS
		want  []string
	}{
		{"postgres", Postgres, []string{"0001_baseline.up.sql", "0002_search_invalid.up.sql", "0003_media_releases.up.sql", "0004_media_slots.up.sql", "0005_media_slot_backfill.up.sql", "0006_drop_media_slots.up.sql", "0007_inline_image_names.up.sql", "0008_canonical_interaction_ids.up.sql", "0009_comment_bans.up.sql", "0010_content_codes.up.sql"}},
		{"clickhouse", ClickHouse, []string{"0001_baseline.up.sql", "0002_projection_generations.up.sql"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			migrations, err := migratekit.Load(tc.files, ".", migratekit.RequireParentLinks())
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, migration := range migrations {
				got = append(got, migration.Name)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("migration chain = %v, want %v", got, tc.want)
			}
		})
	}
}
