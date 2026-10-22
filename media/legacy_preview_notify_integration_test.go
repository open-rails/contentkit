package media_test

import (
	"github.com/jackc/pgx/v5"
	"testing"
)

// The reader upgrade re-arms only an already-applied source notification.
// Its old storage effects must never be replayed by that notification path.
func TestLegacyPreviewNotificationDoesNotReplayStorageEffects(t *testing.T) {
	c := &committed{}
	f := fixtureWithCommitted(t, c)
	f.visible(1)
	ref := f.ref("gallery", 1)
	f.put(ref, "originals/1.png", "image/png", png(1))
	item, _ := f.reg.Item(ref)
	key, _ := item.Blob(f.fileBlob(ref, "originals/1.png"))
	table := pgx.Identifier{f.env.ContentSchema(), "content_media_commits"}.Sanitize()
	if _, err := f.env.Pool().Exec(t.Context(), `UPDATE `+table+` SET effects=jsonb_set(jsonb_set(effects,'{notify}','true'),'{private}',jsonb_build_array($3::text))
WHERE tenant_id=$1 AND operation_id=(SELECT operation_id FROM `+table+` WHERE tenant_id=$1 AND folder_prefix=$2 AND state='applied' AND effects ? 'notify' ORDER BY updated_at DESC LIMIT 1)`, ref.TenantID, item.Prefix(), key); err != nil {
		t.Fatal(err)
	}
	before := len(c.seen())
	if err := f.ms.RecoverPending(t.Context(), 100); err != nil {
		t.Fatal(err)
	}
	if !f.exists(key) || len(c.seen()) != before+1 {
		t.Fatal("notification replay reapplied storage effects or was not delivered")
	}
	if err := f.ms.RecoverPending(t.Context(), 100); err != nil {
		t.Fatal(err)
	}
	if !f.exists(key) || len(c.seen()) != before+1 {
		t.Fatal("acknowledged upgrade notification replayed")
	}
}
