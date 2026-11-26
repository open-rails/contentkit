package critical_test

import (
	"bytes"
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/internal/jobhook"
	"github.com/open-rails/contentkit/media"
	"github.com/riverqueue/river"
)

// A version is live from the later of its completed release and its
// scheduled publication: re-scheduling a released item hides it at once,
// while the withdrawal of its earlier release is still queued.
func TestRescheduledPublication(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	attachMedia(t, w, a)
	admin := a.engine.Client()
	gallery := func(title string) media.RefBody {
		t.Helper()
		ref := galleryRef(t, admin, title)
		_, err := admin.IngestMedia(t.Context(), http.DefaultClient, ref, uuid.NewString(), []ck.MediaFile{{Path: "pages/01.png", Type: "image/png", Body: bytes.NewReader(imageBytes(t))}})
		must(t, err)
		awaitMediaReady(t, admin, ref)
		return ref
	}
	rescheduled, scheduled, unpublished := gallery("Re-scheduled gallery"), gallery("Scheduled gallery"), gallery("Unpublished gallery")
	// Redeploy without job consumers: a release runs only when a case runs it.
	a.server.Close()
	must(t, a.engine.Close(t.Context()))
	var err error
	a.engine, err = ck.New(t.Context(), a.cfg, a.deps)
	must(t, err)
	a.server = mountEngine(t, a.engine)
	fleet, err := jobhook.Fleet(t.Context(), a.engine, &river.Config{Schema: publicationRiverSchema(a), Queues: map[string]river.QueueConfig{}})
	must(t, err)
	t.Cleanup(func() { must(t, fleet.Stop(context.Background())) })
	admin = a.engine.Client()
	reader := a.client("")
	base := w.clock.now()
	release := func(t *testing.T) { t.Helper(); runPublicationWorker(t, fleet, a) }
	// shown checks every reader surface: the page, the edition, its card and
	// its media.
	shown := func(t *testing.T, ref media.RefBody, want bool) {
		t.Helper()
		_, page := reader.GetContent(t.Context(), ref.ID, ck.GetContentQuery{})
		_, edition := reader.GetVersion(t.Context(), ref.Version, ck.GetVersionQuery{})
		_, files := reader.GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{})
		for _, surface := range []struct {
			name string
			err  error
		}{{"page", page}, {"edition", edition}, {"media", files}} {
			switch {
			case want && surface.err != nil:
				t.Fatalf("%s hidden at %v: %v", surface.name, w.clock.now(), surface.err)
			case !want && surface.err == nil:
				t.Fatalf("%s readable at %v", surface.name, w.clock.now())
			case !want:
				requireError(t, surface.err, 404, "not_found")
			}
		}
		cards, err := reader.GetContents(t.Context(), ck.GetContentsQuery{Ids: []string{ref.ID}})
		must(t, err)
		if listed := len(cards.Contents) == 1; listed != want {
			t.Fatalf("listed=%t at %v, want %t", listed, w.clock.now(), want)
		}
	}
	status := func(t *testing.T, ref media.RefBody) catalog.VersionStatus {
		t.Helper()
		got, err := admin.GetVersions(t.Context(), ck.GetVersionsQuery{Ids: []string{ref.Version}})
		must(t, err)
		if len(got) != 1 {
			t.Fatalf("version status: %+v", got)
		}
		return got[0]
	}
	released := func(t *testing.T, ref media.RefBody) bool {
		t.Helper()
		var released bool
		must(t, w.pool.QueryRow(t.Context(), `SELECT released FROM `+w.schema+`.content_versions WHERE tenant_id=$1 AND id=$2`, publicationTenant(a), ref.Version).Scan(&released))
		return released
	}

	t.Run("CK-J09-S68_rescheduling_a_released_item_hides_it_before_its_withdrawal_runs", func(t *testing.T) {
		w.clock.set(base)
		ref := rescheduled
		_, err := admin.PostContentPublish(t.Context(), ref.ID, catalog.PublishInput{})
		must(t, err)
		release(t)
		shown(t, ref, true)
		first := status(t, ref).LiveFrom
		now := base.Add(time.Minute)
		w.clock.set(now)
		// Re-accepting an open publication moves nothing.
		_, err = admin.PostContentPublish(t.Context(), ref.ID, catalog.PublishInput{})
		must(t, err)
		if got := status(t, ref).LiveFrom; first == nil || got == nil || !got.Equal(*first) {
			t.Fatalf("re-accepting an open publication moved live_from from %v to %v", first, got)
		}
		_, err = admin.PostContentUnpublish(t.Context(), ref.ID)
		must(t, err)
		shown(t, ref, false)
		at := now.Add(time.Hour)
		_, err = admin.PostContentPublish(t.Context(), ref.ID, catalog.PublishInput{PublishAt: &at})
		must(t, err)
		if job := currentPublicationJob(t, a, ref.Version); job.ID == 0 || !released(t, ref) {
			t.Fatalf("the withdrawal must still be queued behind the first release: job %+v", job)
		}
		for _, clock := range []time.Time{now, at.Add(-time.Microsecond)} {
			w.clock.set(clock)
			shown(t, ref, false)
			if st := status(t, ref); st.Live || st.LiveFrom != nil && !st.LiveFrom.After(clock) {
				t.Fatalf("status before its date: %+v", st)
			}
		}
		w.clock.set(at)
		release(t)
		shown(t, ref, true)
		if st := status(t, ref); !st.Live {
			t.Fatalf("status at its date: %+v", st)
		}
	})
	t.Run("CK-J09-S68_a_first_scheduled_publication_opens_once_released_at_its_date", func(t *testing.T) {
		w.clock.set(base)
		ref := scheduled
		at := base.Add(time.Hour)
		_, err := admin.PostContentPublish(t.Context(), ref.ID, catalog.PublishInput{PublishAt: &at})
		must(t, err)
		release(t)
		for _, clock := range []time.Time{base, at} {
			w.clock.set(clock)
			shown(t, ref, false)
			if st := status(t, ref); st.Live || st.LiveFrom != nil {
				t.Fatalf("unreleased status: %+v", st)
			}
		}
		release(t)
		shown(t, ref, true)
		if st := status(t, ref); !st.Live || st.LiveFrom == nil || st.LiveFrom.Before(at) {
			t.Fatalf("released status: %+v", st)
		}
	})
	t.Run("CK-J09-S68_unpublishing_hides_at_once_and_after_its_withdrawal", func(t *testing.T) {
		w.clock.set(base)
		ref := unpublished
		_, err := admin.PostContentPublish(t.Context(), ref.ID, catalog.PublishInput{})
		must(t, err)
		release(t)
		shown(t, ref, true)
		_, err = admin.PostContentUnpublish(t.Context(), ref.ID)
		must(t, err)
		shown(t, ref, false)
		if st := status(t, ref); st.Live {
			t.Fatalf("unpublished status: %+v", st)
		}
		release(t)
		shown(t, ref, false)
		if st := status(t, ref); st.Live || st.LiveFrom != nil || released(t, ref) {
			t.Fatalf("withdrawn status: %+v", st)
		}
		at := base.Add(time.Hour)
		_, err = admin.PostContentPublish(t.Context(), ref.ID, catalog.PublishInput{PublishAt: &at})
		must(t, err)
		shown(t, ref, false)
	})
}
