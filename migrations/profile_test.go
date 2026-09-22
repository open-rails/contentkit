package migrations

import (
	"io/fs"
	"testing"

	"github.com/open-rails/migratekit"
)

func TestOneBaselinePerStore(t *testing.T) {
	for name, files := range map[string]fs.FS{"postgres": Postgres, "clickhouse": ClickHouse} {
		t.Run(name, func(t *testing.T) {
			baseline, err := migratekit.Load(files, ".", migratekit.RequireParentLinks())
			if err != nil {
				t.Fatal(err)
			}
			if len(baseline) != 1 || baseline[0].Name != "0001_baseline.up.sql" {
				t.Fatalf("expected one root baseline: %+v", baseline)
			}
		})
	}
}
