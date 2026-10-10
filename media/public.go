package media

import (
	"slices"
	"strconv"
	"strings"

	"github.com/open-rails/contentkit/media/layout"
)

// URL picks the narrowest published rendition at least width pixels wide,
// or the widest when the source is smaller. An unpublished image has no URL.
func (p PublicImage) URL(width int) string {
	var pick, widest PublicRendition
	for _, r := range p.Renditions {
		if r.W > widest.W {
			widest = r
		}
		if r.W >= width && (pick.URL == "" || r.W < pick.W) {
			pick = r
		}
	}
	if pick.URL == "" {
		pick = widest
	}
	return pick.URL
}

// SrcSet uses published URLs and actual encoded widths, without deriving a
// filename from the item ID. Duplicate widths (a small source) occur once.
func (p PublicImage) SrcSet() string {
	rends := slices.Clone(p.Renditions)
	slices.SortStableFunc(rends, func(a, b PublicRendition) int { return a.W - b.W })
	var set []string
	last := 0
	for _, r := range rends {
		if r.W != last {
			set = append(set, r.URL+" "+strconv.Itoa(r.W)+"w")
			last = r.W
		}
	}
	return strings.Join(set, ", ")
}

// GatewayRules are the media gateway's rules for an app's media host: the
// namespaces it serves there (MEDIA_GATEWAY_HOSTS, with
// layout.FormatHosts) and the default public names of its kinds
// (MEDIA_GATEWAY_DEFAULTS, with layout.FormatDefaults).
type GatewayRules struct {
	Namespaces []string
	Defaults   []layout.Default
}

// GatewayConfig combines the registry namespaces with the fallback mappings
// returned by image.PublishDefaults. Publish successfully before deploying them.
func GatewayConfig(r *Registry, defaults []layout.Default) GatewayRules {
	return GatewayRules{Namespaces: r.Namespaces(), Defaults: defaults}
}
