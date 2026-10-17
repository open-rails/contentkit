package hub

import (
	"context"
	"testing"
	"time"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/engagementtest"
	"github.com/open-rails/contentkit/internal/signaltest"
	"github.com/open-rails/contentkit/signal"
)

const hubSignalTestCHDB = "contentkit_hub_signal_test"

// signalHub is an embedded hub over a real ClickHouse with the migrated signal
// schema and no content model.
func signalHub(t *testing.T, mutate func(*Config)) *Hub {
	t.Helper()
	hub, _ := signalHubConn(t, mutate)
	return hub
}

func signalHubConn(t *testing.T, mutate func(*Config)) (*Hub, signal.Conn) {
	t.Helper()
	env := signaltest.FromEnv(t)
	conn := env.Fresh(t, hubSignalTestCHDB)
	return newTestHub(t, conn, hubSignalTestCHDB, mutate), conn
}

func hubReading(tenant, id, subject string, day int) engagementtest.Observation {
	return engagementtest.Observation{
		ContentRef: contentref.New(tenant, "gallery", id),
		Subject:    signal.Subject{UserID: subject},
		Type:       engagementtest.TypeReading,
		EventID:    id + ":" + subject,
		OccurredAt: time.Date(2026, 6, day, 10, 0, 0, 0, time.UTC),
		Progress:   10, ProgressMax: 10, DurationS: 80,
	}
}

func TestHubEraseSubjectsIsTenantScoped(t *testing.T) {
	h, conn := signalHubConn(t, nil)
	ctx := context.Background()
	other := newTestHub(t, conn, hubSignalTestCHDB, func(c *Config) { c.Tenant = "hentai0" })
	for _, hub := range []*Hub{h, other} {
		if err := hub.RecordSignals(ctx, engagementtest.Native([]engagementtest.Observation{hubReading(hub.Tenant(), cid(1), "gone", 1), hubReading(hub.Tenant(), cid(1), "kept", 1)})); err != nil {
			t.Fatal(err)
		}
	}
	report, err := h.EraseSubjects(ctx, []signal.Subject{{UserID: "gone"}})
	if err != nil || !report.Complete() {
		t.Fatalf("erase: %+v %v", report, err)
	}
	for _, tc := range []struct {
		hub  *Hub
		want int
	}{{h, 0}, {other, 1}} {
		hist, err := tc.hub.History(ctx, signal.Subject{UserID: "gone"}, signal.HistoryOptions{})
		if err != nil || len(hist) != tc.want {
			t.Fatalf("%s history after erasure: %v %v", tc.hub.Tenant(), hist, err)
		}
	}
	if _, err := h.EnforceErasures(ctx); err != nil {
		t.Fatal(err)
	}
	g1 := h.Content("gallery", cid(1))
	m, err := h.Metrics(ctx, []ContentRef{g1}, signal.AllTime())
	if err != nil || m[g1.Key()].Readers != 1 {
		t.Fatalf("metrics after erasure: %+v %v", m, err)
	}
}
