package popularity

import (
	"fmt"
	"time"

	signalstore "github.com/open-rails/contentkit/internal/signalstore"

	"github.com/open-rails/contentkit/signal"
)

// Periods are the only public popularity windows: literal whole-UTC-day
// windows with equal weight inside, "all" unbounded.
var Periods = []string{"7d", "30d", "90d", "365d", "all"}

// WindowForPeriod maps a public period onto the window ending on now's UTC
// day. Anything else is an error, never a silent default.
func WindowForPeriod(period string, now time.Time) (signal.Window, error) {
	switch period {
	case "7d":
		return signalstore.LastDays(7, now), nil
	case "30d":
		return signalstore.LastDays(30, now), nil
	case "90d":
		return signalstore.LastDays(90, now), nil
	case "365d":
		return signalstore.LastDays(365, now), nil
	case "all":
		return signalstore.AllTime(), nil
	}
	return signal.Window{}, fmt.Errorf("popularity: unsupported period %q (known: %v)", period, Periods)
}
