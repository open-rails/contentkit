package contentref

import (
	"testing"
	"time"
)

// LegacyID must not change: re-running an import maps each legacy row to the
// content id, and media folder, it was given before.
func TestLegacyIDIsStable(t *testing.T) {
	for _, c := range []struct {
		got, want string
	}{
		{LegacyID("doujins", "gallery", 42, time.Date(2015, 3, 4, 5, 6, 7, 8_000_000, time.UTC)), "014be32d-3a20-7420-a991-5d69a1463348"},
		{LegacyID("hentai0", "video", 7, time.Time{}), "0125e72e-7807-7a88-8e40-7cb5e4ff2452"},
	} {
		if c.got != c.want {
			t.Errorf("LegacyID = %s, want %s", c.got, c.want)
		}
	}
	if err := ValidateID(IDAt(time.Now())); err != nil {
		t.Fatal(err)
	}
}
