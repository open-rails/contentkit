package critical_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/helpers/eventlog"
)

func TestAccountMediaEventReader(t *testing.T) {
	t.Run("real_avatar_completion_is_shared_within_one_tenant", func(t *testing.T) {
		schema := "ckae_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:20]
		w := newWorld(t, func(c *ck.Config) { c.Accounts = &ck.AccountsConfig{Schema: schema} })
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_, err := w.pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
			must(t, err)
		})
		a, sibling, foreign := w.apps[0], w.apps[1], w.apps[2]
		attachMedia(t, w, a, func(c *ck.Config) { c.Avatars = &ck.AvatarsConfig{} })
		reader, err := a.engine.AccountMediaEvents()
		must(t, err)
		query := eventlog.Query{Types: []string{media.EventProcessingProgressed, media.EventProcessingCompleted}}
		before, err := reader.ListEvents(t.Context(), query)
		must(t, err)
		if len(before.Events) != 0 || before.NextCursor == "" {
			t.Fatalf("fresh account stream is not empty and resumable: %+v", before)
		}
		user, credential := w.person(t, 0)
		client := a.client(credential)
		ref := media.RefBody{Kind: "user", ID: user.ID}
		// Hold the observation transaction while the actual image processor can
		// finish its manifest. Readiness and event visibility are separate commits.
		mediaConfig, err := a.cfg.MediaKinds()
		must(t, err)
		registry, err := media.NewRegistry(mediaConfig, media.Hooks{})
		must(t, err)
		accountRef, err := registry.Ref("user", user.ID)
		must(t, err)
		item, err := registry.Item(accountRef)
		must(t, err)
		storage, _, err := a.cfg.AccountMedia()
		must(t, err)
		barrier, err := pgx.ConnectConfig(t.Context(), w.pool.Config().ConnConfig.Copy())
		must(t, err)
		defer barrier.Close(context.Background())
		key := storage.Schema + "/media-processing/" + item.Prefix()
		_, err = barrier.Exec(t.Context(), "SELECT pg_advisory_lock(hashtextextended($1,0))", key)
		must(t, err)
		_, err = client.IngestMedia(t.Context(), nil, ref, uuid.NewString(), []ck.MediaFile{{Path: "avatar.png", Type: "image/png", Body: bytes.NewReader(imageBytes(t))}})
		must(t, err)
		ready := errors.New("avatar finished")
		watch, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		defer cancel()
		err = client.WatchMedia(watch, ref, "", func(update ck.MediaProgress) error {
			if update.Read != nil && update.Read.Status != nil && update.Read.Status.State == media.StateReady && update.Read.Status.Uploads.Completed == 1 {
				return ready
			}
			return nil
		})
		if !errors.Is(err, ready) {
			t.Fatalf("actual account processor did not finish: %v", err)
		}
		query.After = before.NextCursor
		page, err := reader.ListEvents(t.Context(), query)
		must(t, err)
		if len(page.Events) != 0 {
			t.Fatalf("processing event crossed its held commit barrier: %+v", page)
		}
		waitForWithin(t, 10*time.Second, func() bool {
			var blocked bool
			must(t, w.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, int32(barrier.PgConn().PID())).Scan(&blocked))
			return blocked
		})
		_, err = barrier.Exec(t.Context(), "SELECT pg_advisory_unlock(hashtextextended($1,0))", key)
		must(t, err)
		// The ready snapshot is not an acknowledgement of the observation
		// transaction. Wait for that durable fact before comparing event readers.
		waitForWithin(t, 30*time.Second, func() bool {
			page, err = reader.ListEvents(t.Context(), query)
			must(t, err)
			completed := false
			for _, event := range page.Events {
				var fact media.ProcessingEvent
				must(t, json.Unmarshal(event.Data, &fact))
				if fact.Kind != "user" || fact.ContentID != user.ID || fact.VersionID != "" {
					t.Fatalf("account stream leaked a different item: %+v", fact)
				}
				if event.Type == media.EventProcessingCompleted && fact.State == media.StateReady && fact.Uploads.Completed == 1 {
					completed = true
				}
				if bytes.Contains(event.Data, []byte("http://")) || bytes.Contains(event.Data, []byte(credential)) {
					t.Fatal("retained media fact contains a delivery URL or credential")
				}
			}
			return completed
		})
		shared, err := sibling.engine.AccountMediaEvents()
		must(t, err)
		replayed, err := shared.ListEvents(t.Context(), query)
		must(t, err)
		if !reflect.DeepEqual(page, replayed) {
			t.Fatal("sibling app cannot observe the same immutable account stream")
		}
		foreignReader, err := foreign.engine.AccountMediaEvents()
		must(t, err)
		foreignPage, err := foreignReader.ListEvents(t.Context(), eventlog.Query{Types: query.Types})
		must(t, err)
		if len(foreignPage.Events) != 0 {
			t.Fatal("different tenant observed the account's media facts")
		}
		_, err = foreignReader.ListEvents(t.Context(), query)
		if !errors.Is(err, eventlog.ErrInvalidCursor) {
			t.Fatalf("foreign tenant accepted an account-stream cursor: %v", err)
		}
		appPage, err := a.engine.Client().EventReader().ListEvents(t.Context(), eventlog.Query{Types: query.Types})
		must(t, err)
		if len(appPage.Events) != 0 {
			t.Fatal("account completion was copied into the app event stream")
		}
	})
}
