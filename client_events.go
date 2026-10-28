package contentkit

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/helpers/eventlog"
)

// ListEvents reads a retained page without registering or acknowledging a reader.
func (c *Client) ListEvents(ctx context.Context, query catalog.EventQuery) (catalog.EventPage, error) {
	return c.GetEvents(ctx, GetEventsQuery{After: query.After, Limit: query.Limit, Types: query.Types, Type: query.Types})
}

// EventReader reads the shared envelope through the client's existing transport
// and authorization. Cursors remain the catalog's legacy opaque cursors.
func (c *Client) EventReader() eventlog.Reader {
	return eventlog.ReaderFunc(func(ctx context.Context, q eventlog.Query) (eventlog.Page, error) {
		q, err := eventlog.Normalize(q)
		if err != nil {
			return eventlog.Page{}, err
		}
		page, err := c.ListEvents(ctx, catalog.EventQuery{After: q.After, Limit: q.Limit, Types: q.Types})
		if err != nil {
			var refusal *Error
			if errors.As(err, &refusal) {
				switch refusal.Code {
				case "invalid_request":
					return eventlog.Page{}, errors.Join(eventlog.ErrInvalidCursor, err)
				case "event_cursor_expired":
					return eventlog.Page{}, errors.Join(eventlog.ErrExpiredCursor, err)
				}
			}
			return eventlog.Page{}, err
		}
		result := eventlog.Page{Events: []eventlog.Event{}, NextCursor: page.NextCursor, HasMore: page.HasMore, RetentionSeconds: page.RetentionSeconds}
		for _, e := range page.Events {
			data, _ := json.Marshal(struct {
				Kind       string `json:"kind"`
				ContentID  string `json:"content_id,omitempty"`
				VersionID  string `json:"version_id,omitempty"`
				EntityID   string `json:"entity_id,omitempty"`
				MergedInto string `json:"merged_into,omitempty"`
			}{e.Kind, e.ContentID, e.VersionID, e.EntityID, e.MergedInto})
			if len(e.Data) != 0 {
				data = e.Data
			}
			subject := eventlog.Subject{Type: e.Kind, ID: e.ContentID}
			if e.EntityID != "" {
				subject.ID = e.EntityID
			}
			result.Events = append(result.Events, eventlog.Event{ID: e.ID, Source: e.Source, Scope: e.Scope, Type: e.Type, Version: e.Version, OccurredAt: e.OccurredAt, RecordedAt: e.RecordedAt, Subject: subject, Data: data})
		}
		return result, nil
	})
}
