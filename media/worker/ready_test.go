package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/workqueue"
)

// The job may have produced its output just before the store fails. Its
// readiness hook must apply the same outage policy as the work itself.
func TestReadyHookSnoozesStoreOutage(t *testing.T) {
	registry, err := media.NewRegistry(media.Config{Namespace: "test", Kinds: []media.Kind{{
		Name: "image", Uploads: []media.Upload{{Path: "original", Types: []string{"image/png"}, MaxBytes: 1024}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	args, err := json.Marshal(workqueue.ImageArgs{Ref: contentref.New("test", "image", "01920000-0000-7000-8000-000000000001")})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name       string
		checkErr   error
		wantSnooze bool
	}{
		{name: "bucket outage", checkErr: media.ErrUnavailable, wantSnooze: true},
		{name: "bad object on healthy bucket"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &readinessStore{checkErr: tc.checkErr}
			manifests, err := media.NewManifests(store, registry, media.ManifestOptions{
				// Get is read-only; neither dependency may be used here.
				Locker: readOnlyLocker{}, Journal: &media.PGJournal{},
			})
			if err != nil {
				t.Fatal(err)
			}
			hook := &readyHook{manifests: manifests}
			err = hook.WorkEnd(t.Context(), &rivertype.JobRow{
				Kind: (workqueue.ImageArgs{}).Kind(), EncodedArgs: args,
			}, nil)
			var snooze *river.JobSnoozeError
			if got := errors.As(err, &snooze); got != tc.wantSnooze {
				t.Fatalf("WorkEnd = %v; snooze = %v, want %v", err, got, tc.wantSnooze)
			}
			if !tc.wantSnooze && !errors.Is(err, media.ErrUnavailable) {
				t.Fatalf("object failure lost: %v", err)
			}
			if store.checks != 1 {
				t.Fatalf("fresh bucket checks = %d, want 1", store.checks)
			}
		})
	}
}

type readinessStore struct {
	media.Store
	checkErr error
	checks   int
}

func (*readinessStore) Get(context.Context, string, media.GetOptions) (io.ReadCloser, media.Object, error) {
	return nil, media.Object{}, media.ErrUnavailable
}

func (s *readinessStore) Check(context.Context) error {
	s.checks++
	return s.checkErr
}

type readOnlyLocker struct{}

func (readOnlyLocker) Lock(context.Context, string) (func(), error) {
	panic("readiness must not acquire a manifest write lock")
}
