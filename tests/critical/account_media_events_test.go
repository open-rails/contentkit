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
		if !completed {
			t.Fatalf("real avatar processing produced no completion fact: %+v", page)
		}
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
