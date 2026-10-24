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

func TestAudienceReleaseRestoredAfterDue(t *testing.T) {
	f := newFixture(t, nil)
	at := f.now().Add(time.Hour)
	in := gallery("Restore release", "en")
	in.Access = &access.Rule{Level: access.PPV, Releases: []access.Release{{At: at, Level: access.Public}}}
	id := f.create("staff", in)
	f.publish("staff", id, nil)
	if err := f.s.DeleteForModeration(f.ctx, f.as("staff"), []string{id}); err != nil {
		t.Fatal(err)
	}
	f.advance(2 * time.Hour)
	restoredAt := f.now()
	if err := f.s.Restore(f.ctx, f.as("staff"), []string{id}); err != nil {
		t.Fatal(err)
	}
	events := audienceEvents(t, f, id)
	if len(events) != 1 || !events[0].OccurredAt.Equal(restoredAt) {
		t.Fatalf("restored audience release=%+v want%s", events, restoredAt)
	}
	if _, err := f.s.PublishDue(f.ctx); err != nil {
		t.Fatal(err)
	}
	if len(audienceEvents(t, f, id)) != 1 {
		t.Fatal("restore replayed release")
	}
}

func TestAudienceReleaseBackdatedEditOccursWhenGranted(t *testing.T) {
	f := newFixture(t, nil)
	in := gallery("Backdated grant", "en")
	in.Access = &access.Rule{Level: access.PPV}
	id := f.create("staff", in)
	f.publish("staff", id, nil)
	f.advance(2 * time.Hour)
	grantedAt := f.now()
	scheduledAt := grantedAt.Add(-time.Hour)
	rule := &access.Rule{Level: access.PPV, Releases: []access.Release{{At: scheduledAt, Level: access.Public}}}
	if err := f.s.Update(f.ctx, f.as("staff"), []Update{{ID: id, ContentPatch: ContentPatch{Access: rule}}}); err != nil {
		t.Fatal(err)
	}
	events := audienceEvents(t, f, id)
	if len(events) != 1 || !events[0].OccurredAt.Equal(grantedAt) {
		t.Fatalf("backdated grant=%+v want%s", events, grantedAt)
	}
}

func TestPublishDueDrainsBeyondOneBatch(t *testing.T) {
	f := newFixture(t, nil)
	_, err := f.pool.Exec(f.ctx, `WITH seeded AS (
 INSERT INTO `+f.s.t.items+`(tenant_id,id,kind,state,publish_at)
 SELECT $1,gen_random_uuid()::text,'gallery','published',$2::timestamptz FROM generate_series(1,501) RETURNING tenant_id,id
 ) INSERT INTO `+f.s.t.versions+`(tenant_id,id,content_id,languages,state,publish_at)
 SELECT tenant_id,gen_random_uuid()::text,id,ARRAY['en'],'published',$2 FROM seeded`, tenant, f.now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n, err := f.s.PublishDue(f.ctx); err != nil || n != 1002 {
		t.Fatalf("drain=%d %v want1002", n, err)
	}
	events := readEventLog(t, f)
	published := map[string]bool{}
	for _, event := range events {
		if event.Type == EventContentPublished {
			published[event.ContentID] = true
		}
		if event.Type == EventVersionPublished && !published[event.ContentID] {
			t.Fatalf("version event preceded its content: %+v", event)
		}
	}
	if len(published) != 501 {
		t.Fatalf("published %d items", len(published))
	}
	if n, err := f.s.PublishDue(f.ctx); err != nil || n != 0 {
		t.Fatalf("replay=%d %v", n, err)
	}
}
