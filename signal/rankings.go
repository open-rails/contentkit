package signal

import "time"

// PopularityQuery sums content_daily for one site over windows that end
// together.
type PopularityQuery struct {
	// Domain and Language are the site; an empty Language spans the domain.
	Domain, Language string
	// Windows are the first UTC days of the windows summed; each runs to
	// Until (exclusive).
	Windows []time.Time
	Until   time.Time
	// Quality is q by content kind and outcome (outcomevalue keys); only
	// these kinds are summed.
	Quality map[string]map[string]float64
	// Strong is the |q| from which a person-day is a strong positive or
	// negative.
	Strong float64
	// Gated are the days contents were open only to tiers.
	Gated []GatedDays
}

// GatedDays are UTC days [From, Until) a content was open only to Tiers.
// Each day's sum scales by the site's active people over the active people
// holding one of them.
type GatedDays struct {
	ContentID   string
	From, Until time.Time
	Tiers       []string
}

// ContentPopularity is one content's sums, one per window in the query's
// order.
type ContentPopularity struct {
	ContentKind, ContentID string
	// Score is Σ q over its person-days, gated days scaled; Audience the
	// same over readers in its audience.
	Score, Audience []float64
	// Opens are its person-days with a read class.
	Opens []uint64
	// Positive and Negative are its person-days with q at least Strong or
	// at most -Strong.
	Positive, Negative []uint64
}
