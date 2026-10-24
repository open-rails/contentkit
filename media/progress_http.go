package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/gate"
	"github.com/open-rails/contentkit/internal/resolve"
	"github.com/open-rails/helpers/eventlog"
)

func acceptsProgress(req *http.Request) bool {
	for _, part := range strings.Split(req.Header.Get("Accept"), ",") {
		if strings.TrimSpace(strings.SplitN(part, ";", 2)[0]) == "text/event-stream" {
			return true
		}
	}
	return false
}

func (h readHandler) progress(w http.ResponseWriter, req *http.Request) {
	if !req.URL.Query().Has("status") {
		fail(w, req, h.log, ErrInvalidRequest)
		return
	}
	ref, actor, err := h.r.requestRef(req, h.o)
	if err != nil {
		fail(w, req, h.log, err)
		return
	}
	authorize := func(ctx context.Context) error {
		var err error
		actor, err = gate.LiveActor(ctx, h.o.Auth, actor)
		if err != nil {
			return err
		}
		res, err := resolve.One(resolve.WithEditorRead(ctx), h.r.reg.cfg.Hooks.Resolver, ref, actor)
		if err != nil {
			return errors.Join(ErrResolve, err)
		}
		if !res.Visible || !res.Editor {
			return ErrNotVisible
		}
		return nil
	}
	if err := authorize(req.Context()); err != nil {
		failProgress(w, req, h, err)
		return
	}
	if h.o.Events == nil {
		fail(w, req, h.log, ErrUnavailable)
		return
	}
	reader, err := h.o.Events(ref)
	if err != nil {
		fail(w, req, h.log, err)
		return
	}
	snapshot := func(ctx context.Context) (*ReadResult, error) {
		return h.r.Read(ctx, ref, actor, ReadOptions{Status: true})
	}
	cursor := req.Header.Get("Last-Event-ID")
	// Query form keeps reconnects compatible with existing cross-origin hosts.
	if cursor == "" {
		cursor = req.URL.Query().Get("after")
	}
	if err := serveProgress(w, req, ref, reader, cursor, authorize, snapshot, time.Second, 10*time.Second); err != nil && req.Context().Err() == nil {
		h.log.Debug("media progress stream ended", "err", err)
	}
}

func failProgress(w http.ResponseWriter, req *http.Request, h readHandler, err error) {
	if errors.Is(err, auth.ErrUnauthenticated) || errors.Is(err, auth.ErrForbidden) || errors.Is(err, auth.ErrUnavailable) {
		refusal := auth.Refuse(req, err)
		for key, values := range refusal.Header {
			w.Header()[key] = values
		}
		code := "unavailable"
		if refusal.Status == 401 {
			code = "unauthorized"
		} else if refusal.Status == 403 {
			code = "forbidden"
		}
		w.Header().Set("Cache-Control", "private, no-store")
		writeJSON(w, refusal.Status, ErrorReply{Code: code, Error: http.StatusText(refusal.Status)})
		return
	}
	fail(w, req, h.log, err)
}

// serveProgress holds one page and one snapshot, with no producer goroutine or
// unbounded subscriber buffer. Backpressure is the socket's bounded write; a
// disconnected client cancels the retained-log query and every timer.
func serveProgress(w http.ResponseWriter, req *http.Request, ref contentref.ContentRef, reader eventlog.Reader, cursor string, authorize func(context.Context) error, snapshot func(context.Context) (*ReadResult, error), interval, heartbeatInterval time.Duration) error {
	ctx := req.Context()
	query := eventlog.Query{After: cursor, Limit: 100, Types: []string{EventProcessingProgressed, EventProcessingCompleted}}
	page, err := reader.ListEvents(ctx, query)
	name := "snapshot"
	if errors.Is(err, eventlog.ErrExpiredCursor) {
		query.After = ""
		page, err = reader.ListEvents(ctx, query)
		name = "resync"
	}
	if err != nil {
		status, code := http.StatusServiceUnavailable, "unavailable"
		if errors.Is(err, eventlog.ErrInvalidCursor) || errors.Is(err, eventlog.ErrInvalidQuery) {
			status, code = 400, "invalid_request"
		}
		writeJSON(w, status, ErrorReply{Code: code, Error: http.StatusText(status)})
		return err
	}
	// Boundary before snapshot: an event committed during the status read will
	// still be visited, so reconnect cannot miss a concurrent completion.
	if err := authorize(ctx); err != nil {
		status, failure := progressFailure(err)
		writeJSON(w, status, failure)
		return err
	}
	initial, err := snapshot(ctx)
	if err != nil {
		status, failure := progressFailure(err)
		writeJSON(w, status, failure)
		return err
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	rc := http.NewResponseController(w)
	write := func(name, id string, data any) error {
		body, err := json.Marshal(data)
		if err != nil {
			return err
		}
		if len(body) > 1<<20 {
			return errors.New("media: progress snapshot exceeds limit")
		}
		if err = rc.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return err
		}
		if strings.ContainsAny(id, "\r\n") {
			return eventlog.ErrInvalidCursor
		}
		if _, err = fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", id, name, body); err != nil {
			return err
		}
		if err := rc.Flush(); err != nil {
			return err
		}
		// Bound writes, not idle observation. A lingering deadline can terminate
		// HTTP/2 at the heartbeat boundary, even with no blocked writer.
		if err := rc.SetWriteDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return err
		}
		return nil
	}
	query.After = page.NextCursor
	if err := write(name, query.After, initial); err != nil {
		return err
	}
	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()
	timer := time.NewTimer(interval)
	defer timer.Stop()
	current := initial
	lastStatus := progressStatusKey(initial)
	beat := func() error {
		if err := authorize(ctx); err != nil {
			_, failure := progressFailure(err)
			_ = write("error", query.After, failure)
			return err
		}
		// Encoder checkpoints can advance during a long chunk without changing
		// the manifest. Observe that existing canonical state on the heartbeat.
		if current.Status != nil && current.Status.State == StateProcessing {
			value, err := snapshot(ctx)
			if err != nil {
				_, failure := progressFailure(err)
				_ = write("error", query.After, failure)
				return err
			}
			key := progressStatusKey(value)
			current = value
			if !bytes.Equal(key, lastStatus) {
				if err := write("progress", query.After, value); err != nil {
					return err
				}
				lastStatus = key
				return nil
			}
		}
		return write("heartbeat", query.After, struct{}{})
	}
	more := page.HasMore
	for {
		// Catch-up pages need not wait for the poll timer, but still service
		// cancellation and heartbeat while unrelated retained facts are read.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-heartbeat.C:
			if err := beat(); err != nil {
				return err
			}
		default:
		}
		if !more {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-heartbeat.C:
				if err := beat(); err != nil {
					return err
				}
				continue
			case <-timer.C:
			}
		}
		if err := authorize(ctx); err != nil {
			_, failure := progressFailure(err)
			_ = write("error", query.After, failure)
			return err
		}
		page, err = reader.ListEvents(ctx, query)
		name = "progress"
		if errors.Is(err, eventlog.ErrExpiredCursor) {
			query.After = ""
			page, err = reader.ListEvents(ctx, query)
			name = "resync"
		}
		if err != nil {
			_ = write("error", query.After, ErrorReply{Code: "unavailable", Error: "Media progress is unavailable; reconnect."})
			return err
		}
		changed := name == "resync"
		for _, event := range page.Events {
			var fact ProcessingEvent
			if json.Unmarshal(event.Data, &fact) == nil && fact.Kind == ref.ContentKind && fact.ContentID == ref.ContentID && fact.VersionID == ref.Version() {
				changed = true
			}
		}
		query.After = page.NextCursor
		if changed {
			if err := authorize(ctx); err != nil {
				_, failure := progressFailure(err)
				_ = write("error", query.After, failure)
				return err
			}
			value, err := snapshot(ctx)
			if err != nil {
				_, failure := progressFailure(err)
				_ = write("error", query.After, failure)
				return err
			}
			current, lastStatus = value, progressStatusKey(value)
			// A meaningful producer fact may replace an output without changing counts.
			if err := write(name, query.After, value); err != nil {
				return err
			}
		}
		more = page.HasMore
		if !more {
			timer.Reset(interval)
		}
	}
}

func progressFailure(err error) (int, ErrorReply) {
	switch {
	case errors.Is(err, auth.ErrUnauthenticated), errors.Is(err, auth.ErrExpired), errors.Is(err, auth.ErrRevoked):
		return 401, ErrorReply{Code: "unauthorized", Error: "Authentication required."}
	case errors.Is(err, auth.ErrForbidden):
		return 403, ErrorReply{Code: "forbidden", Error: "Media observation is no longer authorized."}
	case errors.Is(err, ErrNotVisible), errors.Is(err, ErrNotAllowed):
		return 404, ErrorReply{Code: "not_found", Error: "Media status is no longer available."}
	default:
		return 503, ErrorReply{Code: "unavailable", Error: "Media progress is unavailable; reconnect."}
	}
}

// At alone is a liveness timestamp, not a visible progress change. Phase,
// segments, speed, ETA and stalled state remain observable.
func progressStatusKey(read *ReadResult) []byte {
	if read.Status == nil {
		return nil
	}
	status := *read.Status
	if status.Encoding != nil {
		encoding := *status.Encoding
		encoding.At = 0
		status.Encoding = &encoding
	}
	key, _ := json.Marshal(status)
	return key
}
