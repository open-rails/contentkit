package locationpolicy

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"sync"
	"time"

	"github.com/open-rails/contentkit/content"
	"github.com/oschwald/geoip2-golang"
)

// Geo owns the City reader and checks for a replaced database once a minute.
// The lock also prevents closing an mmap while another request reads it.
type Geo struct {
	mu       sync.RWMutex
	path     string
	reader   *geoip2.Reader
	modified time.Time
	checked  time.Time
	log      *slog.Logger
}

func Open(path string, log *slog.Logger) (*Geo, error) {
	g := &Geo{path: path, log: log}
	if path == "" {
		return g, nil
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	g.reader, err = geoip2.Open(path)
	if err != nil {
		return nil, err
	}
	g.modified, g.checked = st.ModTime(), time.Now()
	return g, nil
}

func (g *Geo) Lookup(ctx context.Context, ip netip.Addr) content.VisitorLocation {
	g.mu.RLock()
	if g.path != "" && time.Since(g.checked) >= time.Minute {
		g.mu.RUnlock()
		g.mu.Lock()
		if g.path != "" && time.Since(g.checked) >= time.Minute {
			g.reload(ctx)
		}
		g.mu.Unlock()
		g.mu.RLock()
	}
	defer g.mu.RUnlock()

	var l content.VisitorLocation
	ip = ip.Unmap()
	if g.reader != nil && ip.IsValid() && ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() {
		city, err := g.reader.City(net.IP(ip.AsSlice()))
		if err == nil {
			l.Country, l.CountryName = city.Country.IsoCode, city.Country.Names["en"]
			if l.Country != "" && len(city.Subdivisions) > 0 && city.Subdivisions[0].IsoCode != "" {
				l.Subdivision = l.Country + "-" + city.Subdivisions[0].IsoCode
				l.SubdivisionName = city.Subdivisions[0].Names["en"]
			}
		}
	}
	if l.Country == "" {
		g.log.DebugContext(ctx, "location: GeoIP lookup miss")
	}
	return l
}

func (g *Geo) reload(ctx context.Context) {
	g.checked = time.Now()
	st, err := os.Stat(g.path)
	if err == nil && !st.ModTime().Equal(g.modified) {
		var reader *geoip2.Reader
		reader, err = geoip2.Open(g.path)
		if err == nil {
			if g.reader != nil {
				if err := g.reader.Close(); err != nil {
					g.log.DebugContext(ctx, "location: close previous GeoIP database", "err", err)
				}
			}
			g.reader, g.modified = reader, st.ModTime()
		}
	}
	if err != nil {
		g.log.DebugContext(ctx, "location: GeoIP reload failed; retaining previous database", "err", err)
	}
}

func (g *Geo) Close() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.reader == nil {
		return nil
	}
	err := g.reader.Close()
	g.reader = nil
	g.path = ""
	return err
}
