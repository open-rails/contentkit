package discovery

import (
	"testing"
	"time"
)

func TestCardStatusMarksNewerVersionsReactionsAndDeletions(t *testing.T) {
	f := seeded(t)
	const host = "main.test"
	en := []string{"en"}
	f.seed(item{n: 21, titles: map[string]string{"en": "Two chapters"}, age: 10 * 24 * time.Hour, versions: []ver{{langs: en}, {langs: en}}},
		item{n: 22, titles: map[string]string{"en": "Gone"}, age: 5 * 24 * time.Hour, versions: []ver{{langs: en}}},
		item{n: 23, titles: map[string]string{"en": "Disliked"}, age: 5 * 24 * time.Hour, versions: []ver{{langs: en}}})
	later := f.now.Add(-24 * time.Hour).UTC().Truncate(time.Microsecond)
	f.exec(`UPDATE {s}.content_versions SET published_at = $2 WHERE id = $1`, f.vid(21, 1), later)
	f.exec(`INSERT INTO {s}.content_creators (tenant_id, content_id, user_id) VALUES ($1, $2, 'maker')`, tenant, f.cid(22))
	f.exec(`UPDATE {s}.content_items SET state = 'deleted', deleted_at = now() WHERE id = $1`, f.cid(22))
	f.exec(`UPDATE {s}.content_facets SET deleted = true WHERE content_id = $1`, f.cid(22))
	f.react(23, "u1", -1, f.now)
	f.react(21, "u1", 1, f.now)

	cards := func(user string) map[string]Card {
		t.Helper()
		_, page := f.list(host, user, q("/contents", "kind", "gallery", "sort", "newest", "limit", "100"))
		out := map[string]Card{}
		for _, c := range page.Contents {
			out[c.Version.ID] = c
		}
		return out
	}
	reader := cards("u1")
	if c := reader[f.vid(21, 0)]; c.UpdatedAt == nil || !c.UpdatedAt.Equal(later) {
		t.Errorf("older chapter not marked updated by the newer one: %+v", c.UpdatedAt)
	}
	if c := reader[f.vid(21, 1)]; c.UpdatedAt != nil {
		t.Errorf("newest chapter marked updated: %v", c.UpdatedAt)
	}
	if c := reader[f.vid(23, 0)]; c.Viewer == nil || c.Viewer.Reaction != -1 {
		t.Errorf("dislike missing: %+v", c.Viewer)
	}
	if c := reader[f.vid(21, 0)]; c.Viewer == nil || c.Viewer.Reaction != 1 {
		t.Errorf("like missing: %+v", c.Viewer)
	}
	if _, ok := reader[f.vid(22, 0)]; ok {
		t.Error("a reader lists a deleted item")
	}
	for id, c := range reader {
		if c.Deleted {
			t.Errorf("reader told %s is deleted", id)
		}
	}
	if c, ok := cards("maker")[f.vid(22, 0)]; !ok || !c.Deleted {
		t.Errorf("creator's deleted item unmarked: %v %+v", ok, c)
	}
}
