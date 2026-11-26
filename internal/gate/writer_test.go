package gate

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/open-rails/authkit/auth"

	"github.com/open-rails/contentkit/internal/httpapi"
)

type conn struct {
	http.ResponseWriter
	read    time.Time
	flushed bool
}

func (c *conn) SetReadDeadline(at time.Time) error { c.read = at; return nil }
func (c *conn) FlushError() error                  { c.flushed = true; return nil }

// A handler behind the gate's cache writer reaches the connection.
func TestGuardReachesTheConnection(t *testing.T) {
	at := time.Now().Add(time.Hour)
	h := (&Gate{}).Guard(httpapi.Spec{Method: http.MethodGet, Path: "/x", Auth: httpapi.Open}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		rc := http.NewResponseController(w)
		if err := rc.SetReadDeadline(at); err != nil {
			t.Errorf("SetReadDeadline: %v", err)
		}
		if err := rc.Flush(); err != nil {
			t.Errorf("Flush: %v", err)
		}
	}))
	c := &conn{ResponseWriter: httptest.NewRecorder()}
	h.ServeHTTP(c, httptest.NewRequest(http.MethodGet, "/x", nil))
	if !c.read.Equal(at) || !c.flushed {
		t.Fatalf("the connection was not reached: %+v", c)
	}
}

type pathAuth struct{ seen string }

func (a *pathAuth) Authenticate(r *http.Request) (auth.Verified, error) {
	a.seen = r.URL.EscapedPath()
	return nil, auth.ErrUnauthenticated
}

// A sender proof names the URL as the client sent it: the gate authenticates
// that, not the path a module's router sees beneath its prefix.
func TestGuardAuthenticatesTheURLAsSent(t *testing.T) {
	a := &pathAuth{}
	h := (&Gate{Auth: a}).Guard(httpapi.Spec{Method: http.MethodGet, Path: "/x", Auth: httpapi.Public}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	r := httptest.NewRequest(http.MethodGet, "/content/v1/x", nil)
	r.Header.Set("Authorization", "DPoP token")
	http.StripPrefix("/content/v1", h).ServeHTTP(httptest.NewRecorder(), httpapi.KeepSentURL(r))
	if a.seen != "/content/v1/x" {
		t.Fatalf("authenticated %q, want the sent /content/v1/x", a.seen)
	}
}
