package contentkit

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/open-rails/contentkit/media"
)

func TestWatchMediaHTTPAndInProcess(t *testing.T) {
	for _, embedded := range []bool{false, true} {
		t.Run(fmt.Sprint(embedded), func(t *testing.T) {
			closed := make(chan struct{})
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(closed)
				if r.Header.Get("Accept") != "text/event-stream" || r.URL.Query().Get("status") != "1" || r.URL.Query().Get("after") != "resume" {
					t.Errorf("request=%v", r)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "id: next\nevent: snapshot\ndata: {\"files\":[],\"status\":{\"state\":\"processing\"}}\n\n")
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			})
			var client *Client
			if embedded {
				client = &Client{base: inprocessBase, hc: &http.Client{Transport: inprocess{handler}}}
			} else {
				server := httptest.NewServer(handler)
				defer server.Close()
				client = NewClient(server.URL, nil)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			err := client.WatchMedia(ctx, media.RefBody{Kind: "gallery", ID: "owner", Version: "v1"}, "resume", func(update MediaProgress) error {
				if update.Cursor != "next" || update.Read == nil || update.Read.Status.State != "processing" {
					t.Fatalf("update=%+v", update)
				}
				return context.Canceled
			})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled=%v", err)
			}
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("stream handler did not stop")
			}
		})
	}
}
