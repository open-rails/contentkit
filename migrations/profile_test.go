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
		{"postgres", Postgres, []string{"0001_baseline.up.sql", "0002_search_invalid.up.sql"}},
		{"clickhouse", ClickHouse, []string{"0001_baseline.up.sql"}},
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
