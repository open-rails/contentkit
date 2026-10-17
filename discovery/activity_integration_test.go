package discovery

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/engagementtest"
	"github.com/open-rails/contentkit/internal/signaltest"
	"github.com/open-rails/contentkit/internal/testauth"
	"github.com/open-rails/contentkit/media/token"
)

type activityTestSigner struct {
	ring token.Ring
	now  time.Time
}

func (s *activityTestSigner) SignTicket(scope string, expires time.Time) string {
	return s.ring.Sign(scope, expires)
}
func (s *activityTestSigner) VerifyTicket(receipt, scope string) error {
	return s.ring.VerifyScope(receipt, scope, s.now)
}

func withActivitySigner(t *testing.T, f *fixture) *activityTestSigner {
	t.Helper()
	ring, err := token.NewRing(token.Key{ID: "test", Secret: bytes.Repeat([]byte{42}, 32)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	signer := &activityTestSigner{ring: ring, now: f.now}
	f.svc.o.ActivitySigner = signer
	f.svc.o.Now = func() time.Time { return signer.now }
	return signer
}

func requestActivity(t *testing.T, f *fixture, user string, target ActivityTarget) ActivityContext {
	t.Helper()
	var out ActivityContexts
	status, code := f.call(http.MethodPost, "main.test", user, "/activity-contexts", ActivityContextsInput{
		Session: "browser", Language: "en", Targets: []ActivityTarget{target}}, &out)
	if status != http.StatusOK || !out.Enabled || len(out.Contexts) != 1 {
		t.Fatalf("contexts: %d %s %+v", status, code, out)
	}
	return out.Contexts[0]
}

func receiptAccess(t *testing.T, receipt ActivityContext) activityReceipt {
	t.Helper()
	body, _, _ := strings.Cut(receipt.Token, ".")
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		t.Fatal(err)
	}
	var out activityReceipt
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestActivityReceiptsPreserveEventTimeAndRejectOtherSubjects(t *testing.T) {
	env := signaltest.FromEnv(t)
	const database = "contentkit_activity_receipts_test"
	conn := env.Fresh(t, database)
	f := newFixture(t, conn, database)
	f.seed(item{n: 1, titles: map[string]string{"en": "Premium"}, rule: `{"level":"tier","tiers":["premium"]}`, versions: []ver{{langs: []string{"en"}, pages: 20}}},
		item{n: 2, titles: map[string]string{"en": "Public"}, versions: []ver{{langs: []string{"en"}, pages: 20}}})
	signer := withActivitySigner(t, f)
	f.svc.o.MeasuredEngagement = true
	target := ActivityTarget{ContentID: f.cid(1), VersionID: f.vid(1, 0)}
	paid := requestActivity(t, f, "fan", target)
	snapshot := receiptAccess(t, paid)
	if snapshot.Access.ContentAccess != "restricted" || snapshot.Access.ViewerAccess != "full" ||
		len(snapshot.Access.EligibleTiers) != 1 || snapshot.Access.Domain != "main" {
		t.Fatalf("paid snapshot: %+v", snapshot.Access)
	}
	// The same subscriber reading free content still belongs in the paid
	// population; population is not limited to people selecting paid cards.
	free := requestActivity(t, f, "fan", ActivityTarget{ContentID: f.cid(2)})
	if x := receiptAccess(t, free).Access; x.ContentAccess != "public" || len(x.EligibleTiers) != 1 {
		t.Fatalf("free browsing lost premium eligibility: %+v", x)
	}
	preview := requestActivity(t, f, "", target)
	if x := receiptAccess(t, preview).Access; x.ViewerAccess != "preview" {
		t.Fatalf("locked reader snapshot: %+v", x)
	}
	staff := testauth.Actor(access.Actor{ID: "staff"}, staffOnly{})
	f.clock = f.now.Add(time.Hour)
	public := access.Rule{Level: access.Public}
	if err := f.cat.Update(f.ctx, staff, []catalog.Update{{ID: f.cid(1), ContentPatch: catalog.ContentPatch{Access: &public}}}); err != nil {
		t.Fatal(err)
	}
	signer.now = f.clock
	measured := func(c ActivityContext, revision uint64) EngagementSnapshot {
		pages := (engagementtest.Observation{ContentRef: contentref.NewVersion(tenant, "gallery", c.ContentID, c.VersionID),
			Type: engagementtest.TypeReading, EventID: "reader-segment", Progress: 20, ProgressMax: 20, DurationS: 200}).Signals()[0].Measurement
		return EngagementSnapshot{ContentID: c.ContentID, VersionID: c.VersionID, VisitID: "reader-visit", SegmentID: "reader-segment",
			Revision: revision, ActivityToken: c.Token, ActiveMS: pages.ActiveMS, Pages: pages.Pages}
	}
	post := func(user, session string, c ActivityContext, revision uint64) (int, string) {
		return f.call(http.MethodPost, "main.test", user, "/engagement", EngagementBatch{Session: session, Snapshots: []EngagementSnapshot{measured(c, revision)}}, nil)
	}
	if status, code := post("fan", "", paid, 1); status != http.StatusNoContent {
		t.Fatalf("measured reading: %d %s", status, code)
	}
	if status, code := post("fan", "", paid, 2); status != http.StatusNoContent {
		t.Fatalf("revision: %d %s", status, code)
	}
	if status, _ := post("other", "", paid, 1); status != http.StatusBadRequest {
		t.Fatalf("other subject: %d", status)
	}
	if status, code := post("", "browser", preview, 1); status != http.StatusNoContent {
		t.Fatalf("preview: %d %s", status, code)
	}
	var events uint64
	if err := conn.QueryRow(context.Background(), `SELECT uniqExact(receipt_id) FROM `+database+`.engagement_segments
 WHERE tenant=? AND content_id=? AND subject=?`, tenant, f.cid(1), "fan").Scan(&events); err != nil || events != 1 {
		t.Fatalf("receipt minted multiple sessions: %d %v", events, err)
	}
	var at time.Time
	var classification string
	if err := conn.QueryRow(f.ctx, `SELECT occurred_at, JSONExtractString(access_context,'content_access')
 FROM `+database+`.engagement_segments WHERE tenant=? AND subject=? ORDER BY revision DESC LIMIT 1`, tenant, "fan").Scan(&at, &classification); err != nil ||
		!at.Equal(paid.OccurredAt) || classification != "restricted" {
		t.Fatalf("late checkpoint changed access/time: %v %q %v", at, classification, err)
	}
	if err := conn.QueryRow(f.ctx, `SELECT count() FROM `+database+`.engagement_segments WHERE tenant=? AND subject<>?`, tenant, "fan").Scan(&events); err != nil || events != 0 {
		t.Fatalf("stolen/preview receipts became measured reading: %d %v", events, err)
	}
	// Expired queue entries are discarded individually, not replayed as
	// untrusted events and not allowed to reject a new entry in the batch.
	signer.now = paid.OccurredAt.Add(25 * time.Hour)
	fresh := requestActivity(t, f, "fan", target)
	signer.now = signer.now.Add(200 * time.Second)
	status, code := f.call(http.MethodPost, "main.test", "fan", "/engagement", EngagementBatch{Snapshots: []EngagementSnapshot{
		measured(paid, 3), measured(fresh, 1),
	}}, nil)
	if status != http.StatusNoContent {
		t.Fatalf("mixed expired queue: %d %s", status, code)
	}
	if err := conn.QueryRow(f.ctx, `SELECT uniqExact(receipt_id) FROM `+database+`.engagement_segments WHERE tenant=? AND subject=?`, tenant, "fan").Scan(&events); err != nil || events != 2 {
		t.Fatalf("expired replay was recorded or fresh measurement was lost: %d %v", events, err)
	}
}

func TestActivityContextsExplicitlyDisableWithoutSigner(t *testing.T) {
	f := newFixture(t, nil, "")
	got, err := f.svc.ActivityContexts(f.ctx, access.Actor{Anonymous: true}, ActivityContextsInput{})
	if err != nil || got.Enabled || len(got.Contexts) != 0 {
		t.Fatalf("unsigned mode: %+v %v", got, err)
	}
}
