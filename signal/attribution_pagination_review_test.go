package signal

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// Copied verbatim from reviewer commit 4fa6803 (review/signal-correctness-20260916,
// signal/review_regression_test.go); the erasure lane carries that commit's other test.
func TestReviewAttributionPaginationRetainsUnattributedClicks(t *testing.T) {
	st, _ := freshStore(t)
	ctx := context.Background()
	at := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)
	subject := Subject{UserID: "review-viewer"}
	ref := gallery("review", cid(1))
	for _, id := range []string{"r1", "r2"} {
		if err := st.RecordExposures(ctx, "review", []Exposure{{RenderID: id, Stage: StageRendered, Subject: subject, OccurredAt: at, Shown: []Placement{{ContentRef: ref, Position: 1}}}}); err != nil {
			t.Fatal(err)
		}
	}
	click := Signal{ContentRef: ref, Subject: subject, Type: "click", EventID: "unattributed", OccurredAt: at,
		Payload: map[string]any{PayloadKeyRenderID: "never-rendered", PayloadKeyPosition: 1}}
	if err := st.RecordSignals(ctx, "review", []Signal{click}); err != nil {
		t.Fatal(err)
	}
	full, err := st.Attribution(ctx, "review", AttributionOptions{Stage: StageRendered, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Unattributed) != 1 {
		t.Fatalf("fixture requires one unattributed click, got %+v", full)
	}
	var after string
	count := 0
	for n := 0; n < 4; n++ {
		page, err := st.Attribution(ctx, "review", AttributionOptions{Stage: StageRendered, Limit: 1, After: after})
		if err != nil {
			t.Fatal(err)
		}
		count += len(page.Unattributed)
		if page.Next == "" {
			break
		}
		after = page.Next
	}
	if count != 1 {
		t.Fatalf("paginated export silently dropped unattributed click: full=%d paginated=%d", len(full.Unattributed), count)
	}
}

func TestAttributionCursorCodec(t *testing.T) {
	opts := AttributionOptions{Stage: StageVisible, Surface: " search ", Window: LastDays(7, time.Date(2026, 9, 16, 5, 0, 0, 0, time.UTC))}
	scope := attributionScope(opts)
	key := clickKey{Render: "r1", OccurredAt: time.Date(2026, 9, 16, 1, 2, 3, 0, time.UTC), ContentKind: "gallery", ContentID: cid(1), ContentVersionID: "v2", SubjectKind: "user", Subject: "u1", EventID: "e1"}
	for _, c := range []attributionCursor{
		{Scope: scope, Phase: phaseRenders, Render: "r9"},
		{Scope: scope, Phase: phaseRenders, Render: "r1", Partial: true, Click: &key},
		{Scope: scope, Phase: phaseUnattributed},
		{Scope: scope, Phase: phaseUnattributed, Click: &key},
	} {
		got, err := decodeAttributionCursor(c.encode(), scope)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, c) {
			t.Fatalf("round trip: %+v != %+v", got, c)
		}
	}
	bad := attributionCursor{Scope: scope, Phase: phaseRenders, Render: "r1", Partial: true}
	for _, token := range []string{"", "r9", "!!", bad.encode(), attributionCursor{Scope: scope, Phase: "x"}.encode()} {
		if _, err := decodeAttributionCursor(token, scope); err == nil {
			t.Fatalf("token %q must be rejected", token)
		}
	}
	other := attributionScope(AttributionOptions{Stage: StageVisible, Surface: "search"})
	if _, err := decodeAttributionCursor(attributionCursor{Scope: other, Phase: phaseRenders}.encode(), scope); err == nil {
		t.Fatal("a cursor from another window must be rejected")
	}
}
