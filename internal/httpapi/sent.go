package httpapi

import (
	"context"
	"net/http"
	"net/url"
)

type sentURLKey struct{}

// KeepSentURL records r's URL before a module's router strips its prefix: a
// sender proof (a DPoP proof's htu) names the URL as the client sent it.
func KeepSentURL(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), sentURLKey{}, r.URL))
}

// AsSent is r with the URL KeepSentURL recorded, for authenticating it; r
// itself when none was recorded.
func AsSent(r *http.Request) *http.Request {
	u, ok := r.Context().Value(sentURLKey{}).(*url.URL)
	if !ok || u == r.URL {
		return r
	}
	sent := new(http.Request)
	*sent = *r
	sent.URL = u
	return sent
}
