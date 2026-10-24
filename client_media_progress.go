package contentkit

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/open-rails/contentkit/media"
)

// MediaProgress is a current authorized status snapshot from the retained
// processing stream. Resync means the previous cursor fell outside retention.
// Heartbeats carry a cursor but no Read. Persist a cursor only after handling
// its observation; cursors are scoped to the server's selected event log.
type MediaProgress struct {
	Event  string
	Cursor string
	Read   *media.ReadResult
}

// WatchMedia observes status over the same authenticated HTTP transport as
// GetMedia/GetMediaVersion. It blocks until cancellation, a stream failure, or
// receive returns an error. Reconnect explicitly with the last handled cursor;
// every connection starts with a fresh authorized snapshot. It issues no media
// capabilities and never enqueues processing. receive provides backpressure.
func (c *Client) WatchMedia(ctx context.Context, ref media.RefBody, after string, receive func(MediaProgress) error) error {
	path := "/v1/media/" + url.PathEscape(ref.Kind) + "/" + url.PathEscape(ref.ID)
	if ref.Version != "" {
		path += "/versions/" + url.PathEscape(ref.Version)
	}
	response, err := c.doHeaders(ctx, http.MethodGet, path, url.Values{"status": {"1"}, "after": {after}}, nil, http.Header{"Accept": {"text/event-stream"}})
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		return fmt.Errorf("contentkit: expected media event stream")
	}
	scan := bufio.NewScanner(response.Body)
	scan.Buffer(make([]byte, 4096), 1<<20)
	var event, cursor, data string
	dispatch := func() error {
		if data == "" {
			event = ""
			return nil
		}
		update := MediaProgress{Event: event, Cursor: cursor}
		switch event {
		case "snapshot", "progress", "resync":
			if err := json.Unmarshal([]byte(data), &update.Read); err != nil {
				return err
			}
		case "error":
			var failure media.ErrorReply
			if err := json.Unmarshal([]byte(data), &failure); err != nil {
				return err
			}
			return &Error{Code: failure.Code, Message: failure.Error}
		case "heartbeat":
		default:
			data, event = "", ""
			return nil
		}
		data, event = "", ""
		return receive(update)
	}
	for scan.Scan() {
		line := scan.Text()
		if line == "" {
			if err := dispatch(); err != nil {
				return err
			}
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimPrefix(value, " ")
		switch key {
		case "event":
			event = value
		case "id":
			if !strings.ContainsRune(value, 0) {
				cursor = value
			}
		case "data":
			if len(data)+len(value) > 1<<20 {
				return fmt.Errorf("contentkit: media event exceeds limit")
			}
			if data != "" {
				data += "\n"
			}
			data += value
		}
	}
	if err := scan.Err(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return io.ErrUnexpectedEOF
}

// The in-process client normally records finite replies. Streams use a pipe so
// first headers arrive immediately and the same cancellation/backpressure
// contract applies without collecting an indefinite response in memory.
func streamInProcess(handler http.Handler, req *http.Request) (*http.Response, error) {
	body, writer := io.Pipe()
	ready, done := make(chan struct{}), make(chan struct{})
	record := &streamRecorder{header: make(http.Header), writer: writer, ready: ready}
	go func() {
		defer close(done)
		defer writer.Close()
		handler.ServeHTTP(record, req)
		record.WriteHeader(http.StatusOK)
	}()
	go func() {
		select {
		case <-req.Context().Done():
			_ = writer.CloseWithError(req.Context().Err())
		case <-done:
		}
	}()
	select {
	case <-req.Context().Done():
		_ = body.Close()
		return nil, req.Context().Err()
	case <-ready:
		return &http.Response{StatusCode: record.status, Header: record.header.Clone(), Body: body, Request: req}, nil
	}
}

type streamRecorder struct {
	header http.Header
	writer *io.PipeWriter
	ready  chan struct{}
	once   sync.Once
	status int
}

func (w *streamRecorder) Header() http.Header { return w.header }
func (w *streamRecorder) WriteHeader(status int) {
	w.once.Do(func() { w.status = status; close(w.ready) })
}
func (w *streamRecorder) Write(data []byte) (int, error) {
	w.WriteHeader(200)
	return w.writer.Write(data)
}
func (w *streamRecorder) Flush() { w.WriteHeader(200) }
