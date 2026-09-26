package media_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/token"
)

type windowResolver struct{}

func (windowResolver) Resolve(context.Context, []contentref.ContentRef, access.Actor) (map[contentref.ContentKey]access.Resolution, error) {
	return nil, nil
}

func TestNewReaderTokenWindow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		window time.Duration
		reject bool
	}{
		{name: "default"},
		{name: "one second", window: time.Second},
		{name: "subsecond", window: 500 * time.Millisecond, reject: true},
		{name: "fractional second", window: 1500 * time.Millisecond, reject: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := media.NewReader(media.ReaderOptions{
				Manifests: &media.Manifests{}, Kinds: &media.Registry{}, Resolver: windowResolver{},
				Delivery: media.Delivery{Mode: media.DeliverURL, BaseURL: "https://media.example",
					SigningKey: token.Key{ID: "k", Secret: bytes.Repeat([]byte("s"), 32)}, Window: tc.window},
			})
			if tc.reject {
				if err == nil || !strings.Contains(err.Error(), "Delivery.Window") {
					t.Fatalf("NewReader accepted %s token window: %v", tc.window, err)
				}
			} else if err != nil {
				t.Fatalf("NewReader rejected %s token window: %v", tc.window, err)
			}
		})
	}
}
