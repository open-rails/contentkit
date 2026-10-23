package catalog

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/open-rails/contentkit/access"
)

func audienceEvents(t *testing.T, f *fixture, id string) []EventRecord {
	t.Helper()
	var out []EventRecord
	for _, event := range readEventLog(t, f) {
		if event.Type == EventAudienceReleased && event.ContentID == id {
			out = append(out, event)
		}
	}
	return out
}

func TestAudienceReleaseCatchupBeforeEdit(t *testing.T) {
	for _, action := range []string{"reschedule", "unpublish", "cancel"} {
		t.Run(action, func(t *testing.T) {
			f := newFixture(t, nil)
			at := f.now().Add(time.Hour)
			in := gallery("Retained release", "en")
			in.Access = &access.Rule{Level: access.PPV, Releases: []access.Release{{At: at, Level: access.Public}}}
			id := f.create("staff", in)
			f.publish("staff", id, nil)
			if action != "cancel" {
				f.advance(2 * time.Hour)
			}
			var err error
			switch action {
			case "unpublish":
				err = f.s.Unpublish(f.ctx, f.as("staff"), []string{id})
			default:
				err = f.s.Update(f.ctx, f.as("staff"), []Update{{ID: id, ContentPatch: ContentPatch{Access: &access.Rule{Level: access.PPV}}}})
			}
			if err != nil {
				t.Fatal(err)
			}
			f.advance(2 * time.Hour)
			for range 2 {
				if _, err := f.s.PublishDue(f.ctx); err != nil {
					t.Fatal(err)
				}
			}
			events := audienceEvents(t, f, id)
			want := 1
			if action == "cancel" {
				want = 0
			}
			if len(events) != want {
				t.Fatalf("events=%+v, want%d", events, want)
			}
			if want == 1 {
				if !events[0].OccurredAt.Equal(at) {
					t.Fatalf("effective time=%s want%s", events[0].OccurredAt, at)
				}
				var payload eventPayload
				if err := json.Unmarshal(events[0].Data, &payload); err != nil || payload.Release == nil || payload.Release.Level != access.Public {
					t.Fatalf("snapshot=%s %v", events[0].Data, err)
				}
			}
		})
	}
}

func TestAudienceReleaseConcurrentRecorderAndRetention(t *testing.T) {
	f := newFixture(t, nil)
	at := f.now().Add(time.Hour)
	in := gallery("Concurrent release", "en")
	in.Access = &access.Rule{Level: access.PPV, Releases: []access.Release{{At: at, Level: access.Public}}}
	id := f.create("staff", in)
	f.publish("staff", id, nil)
	f.advance(2 * time.Hour)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Go(func() { _, err := f.s.PublishDue(f.ctx); errs <- err })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if events := audienceEvents(t, f, id); len(events) != 1 {
		t.Fatalf("events=%+v", events)
	}
	// A retired log is not permission to replay a domain transition.
	if _, err := f.pool.Exec(f.ctx, `DELETE FROM `+f.s.qs+`.eventlog_events WHERE event_type=$1`, EventAudienceReleased); err != nil {
		t.Fatal(err)
	}
	if n, err := f.s.PublishDue(f.ctx); err != nil || n != 0 {
		t.Fatalf("replayed retired transition: %d %v", n, err)
	}
}
