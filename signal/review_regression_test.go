package signal

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// Pause only the network dispatch after the real store has read the fence.
// All SQL, projections, erasures and reads still use disposable ClickHouse.
type reviewPausedInsert struct {
	Conn
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (c *reviewPausedInsert) Exec(ctx context.Context, query string, args ...any) error {
	if strings.HasPrefix(query, "INSERT INTO "+testDB+".events") {
		c.once.Do(func() {
			close(c.entered)
			select {
			case <-c.release:
			case <-ctx.Done():
			}
		})
	}
	return c.Conn.Exec(ctx, query, args...)
}

func TestReviewErasureMustFenceInFlightWriter(t *testing.T) {
	st, conn := freshStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	gate := &reviewPausedInsert{Conn: conn, entered: make(chan struct{}), release: make(chan struct{})}
	writer, err := NewStore(gate, testDB)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	subject := Subject{UserID: "review-erased"}
	ref := EntityRef{EntityType: "gallery", EntityID: "review-1"}
	go func() {
		done <- writer.RecordSignals(ctx, "review", []Signal{{EntityRef: ref, Subject: subject, Type: TypeView, EventID: "pending", OccurredAt: time.Now(), Progress: 1}})
	}()
	select {
	case <-gate.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	report, eraseErr := st.EraseSubjects(ctx, []string{"review"}, []Subject{subject})
	close(gate.release)
	writeErr := <-done
	if eraseErr != nil {
		t.Fatal(eraseErr)
	}
	if !report.Complete() {
		t.Fatalf("erasure did not report completion: %+v", report)
	}
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	state, err := st.States(ctx, "review", subject, []EntityRef{ref})
	if err != nil {
		t.Fatal(err)
	}
	if len(state) != 0 {
		t.Fatalf("completed erasure resurrected a public state after an in-flight write: %+v", state)
	}
}
