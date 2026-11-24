package catalogstore

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type conn struct {
	http.ResponseWriter
	read    time.Time
	flushed bool
}

func (c *conn) SetReadDeadline(at time.Time) error { c.read = at; return nil }
func (c *conn) FlushError() error                  { c.flushed = true; return nil }

// The access log's status writer leaves the connection reachable.
func TestStatusWriterReachesTheConnection(t *testing.T) {
	at := time.Now().Add(time.Hour)
	c := &conn{ResponseWriter: httptest.NewRecorder()}
	rc := http.NewResponseController(&statusWriter{ResponseWriter: c})
	if err := rc.SetReadDeadline(at); err != nil || !c.read.Equal(at) {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	if err := rc.Flush(); err != nil || !c.flushed {
		t.Fatalf("Flush: %v", err)
	}
}
