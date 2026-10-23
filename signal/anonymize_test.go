package signal

import (
	"strings"
	"testing"
)

func TestAccountAnonymizationRefusesUnsafeReplicaRoutingBeforeMovingData(t *testing.T) {
	conn := &fakeConn{rowsFor: map[string][][]any{"SELECT max(toUInt32(total_replicas))": {{uint32(2)}}}}
	st, err := NewStore(conn, "signals")
	if err != nil {
		t.Fatal(err)
	}
	err = st.AnonymizeSubject(t.Context(), "tenant", Subject{UserID: "removed"}, Subject{AnonKey: "random"})
	if err == nil || !strings.Contains(err.Error(), "replica-affine") {
		t.Fatalf("unsafe topology was not refused: %v", err)
	}
	if len(conn.execs) != 0 {
		t.Fatalf("unsupported routing fenced or moved data: %+v", conn.execs)
	}
}

func TestAccountAnonymizationRequiresCompleteHealthyCluster(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows [][]any
		ok   bool
	}{
		{"complete", [][]any{{"engagement_segments", "default", "/keeper/segments", "a", uint64(2), uint8(0), uint8(0), []string{"a", "b"}}, {"engagement_segments", "default", "/keeper/segments", "b", uint64(2), uint8(0), uint8(0), []string{"a", "b"}}}, true},
		{"missing replica", [][]any{{"engagement_segments", "default", "/keeper/segments", "a", uint64(2), uint8(0), uint8(0), []string{"a", "b"}}}, false},
		{"duplicate alias", [][]any{{"engagement_segments", "default", "/keeper/segments", "a", uint64(2), uint8(0), uint8(0), []string{"a", "b"}}, {"engagement_segments", "default", "/keeper/segments", "a", uint64(2), uint8(0), uint8(0), []string{"a", "b"}}}, false},
		{"independent shard", [][]any{{"engagement_segments", "default", "/keeper/segments", "a", uint64(2), uint8(0), uint8(0), []string{"a", "b"}}, {"engagement_segments", "default", "/other/segments", "b", uint64(2), uint8(0), uint8(0), []string{"a", "b"}}}, false},
		{"expired session", [][]any{{"engagement_segments", "default", "/keeper/segments", "a", uint64(1), uint8(0), uint8(1), []string{"a"}}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := &fakeConn{rowsFor: map[string][][]any{"clusterAllReplicas": tc.rows}}
			st, err := NewStore(conn, "signals", StoreOptions{Cluster: "contentkit"})
			if err != nil {
				t.Fatal(err)
			}
			_, err = st.prepareAnonymousCopy(t.Context(), []string{"engagement_segments"}, 2)
			if (err == nil) != tc.ok {
				t.Fatalf("preflight error=%v, want success=%v", err, tc.ok)
			}
			if len(conn.execs) != 0 {
				t.Fatal("replica inspection moved data")
			}
		})
	}
}
