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
		{"postgres", Postgres, []string{"0001_baseline.up.sql", "0002_app_bindings.up.sql", "0003_prepare_version_covers.up.sql", "0004_finish_version_covers.up.sql", "0005_erasure_completion.up.sql", "0006_publishing_operations.up.sql", "0007_internal_entities.up.sql", "0008_access_history.up.sql", "0009_page_review.up.sql", "0010_name_prefix_index.up.sql", "0011_search_substrings.up.sql"}},
		{"clickhouse", ClickHouse, []string{"0001_baseline.up.sql", "0002_population.up.sql", "0003_measured_engagement.up.sql", "0004_measured_population.up.sql"}},
		{"accounts", Accounts, []string{"0001_account_bindings.up.sql", "0002_notification_merchants.up.sql", "0003_media.up.sql", "0004_avatar_placement.up.sql", "0005_erasure_completion.up.sql"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			migrations, err := migratekit.Load(tc.files, ".")
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
