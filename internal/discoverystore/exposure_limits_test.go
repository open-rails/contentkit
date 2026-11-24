package discoverystore

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/discovery"
	"github.com/open-rails/contentkit/media/token"
	"github.com/open-rails/contentkit/signal"
)

// Batches at the signal and exposure limits, every item carrying the
// activity receipt the engine issues, fit the body cap.
func TestBatchesFitBody(t *testing.T) {
	ring, err := token.NewRing(token.Key{ID: "k1", Secret: []byte(strings.Repeat("s", 32))}, nil)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	receipt := func(target discovery.ActivityTarget) string {
		r := activityReceipt{ID: contentref.NewID(), Tenant: contentref.NewID(), Subject: strings.Repeat("a", 64), Target: target, ResolvedVersion: target.VersionID,
			Access: signal.AccessContext{ObservedAt: at, ContentAccess: "restricted", ViewerAccess: "preview", PolicyRevision: strings.Repeat("b", 64),
				Audience: "tier:gold,silver", AudienceTiers: []string{"gold", "silver"}, EligibleTiers: []string{"gold", "silver"}, EligibilityKnown: true, Domain: "main", Language: "en"}}
		body, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		encoded := base64.RawURLEncoding.EncodeToString(body)
		return encoded + "." + ring.Sign("activity:"+encoded, at.Add(activityReceiptTTL))
	}
	exposures := discovery.Exposures{Session: contentref.NewID()}
	for range signal.MaxExposuresPerBatch {
		e := discovery.ExposureInput{RenderID: contentref.NewID() + "-12", Stage: string(signal.StageVisible), Revision: 1 << 40, QueryID: contentref.NewID(),
			SearchQuery: strings.Repeat("q", maxSearchQuery), Surface: signal.SurfaceSearch, Ranker: "default", Language: "en", OccurredAt: &at}
		for p := range signal.MaxShownPerExposure {
			target := discovery.ActivityTarget{ContentID: contentref.NewID(), VersionID: contentref.NewID()}
			e.Shown = append(e.Shown, discovery.Placement{ContentID: target.ContentID, VersionID: target.VersionID, Position: uint32(1_000_000 + p), ActivityToken: receipt(target)})
		}
		exposures.Exposures = append(exposures.Exposures, e)
	}
	signals := discovery.Signals{Session: contentref.NewID()}
	for range signal.MaxSignalsPerBatch {
		target := discovery.ActivityTarget{ContentID: contentref.NewID(), VersionID: contentref.NewID()}
		signals.Signals = append(signals.Signals, discovery.SignalInput{ContentID: target.ContentID, VersionID: target.VersionID, Type: "click", EventID: contentref.NewID(),
			Revision: 1 << 40, OccurredAt: at, RenderID: contentref.NewID() + "-12", Surface: signal.SurfaceSearch, Position: 1_000_000, ActivityToken: receipt(target)})
	}
	for name, batch := range map[string]any{"exposures": exposures, "signals": signals} {
		body, err := json.Marshal(batch)
		if err != nil {
			t.Fatal(err)
		}
		if len(body) > maxBody {
			t.Errorf("%s at the limits are %d bytes; the body cap is %d", name, len(body), maxBody)
		}
		t.Logf("%s at the limits are %d bytes of %d", name, len(body), maxBody)
	}
}
