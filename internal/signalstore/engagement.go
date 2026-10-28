package signalstore

import (
	"fmt"
	"slices"

	signal "github.com/open-rails/contentkit/signal"
)

// MeasurePages validates and normalizes a reader's measured page coverage.
func MeasurePages(pages []signal.PageEngagement, total, activeMS uint32) (signal.Measurement, error) {
	m := signal.Measurement{ActiveMS: activeMS, Total: total * 100, Scale: 100, Pages: pages, Units: []uint32{}, Attention: [][2]uint64{}}
	if total == 0 || total > 100_000 || activeMS == 0 || activeMS > signal.MaxEngagementMS || len(pages) == 0 || len(pages) > signal.MaxEngagementPages {
		return m, fmt.Errorf("signal: invalid page measurement size or attention")
	}
	seen := map[uint32]bool{}
	var attention uint64
	ranges := 0
	for _, p := range pages {
		if p.Index >= total || seen[p.Index] || p.ActiveMS == 0 || len(p.Ranges) == 0 || len(p.Ranges) > signal.MaxPageRanges {
			return m, fmt.Errorf("signal: invalid or duplicate measured page %d", p.Index)
		}
		seen[p.Index] = true
		attention += uint64(p.ActiveMS)
		ranges += len(p.Ranges)
		m.Attention = append(m.Attention, [2]uint64{uint64(p.Index), uint64(p.ActiveMS)})
		var until uint32
		for i, r := range p.Ranges {
			if r.From >= r.To || r.To > signal.CoverageScale || i > 0 && r.From <= until {
				return m, fmt.Errorf("signal: page coverage ranges must be merged and ordered")
			}
			until = r.To
			for n := r.From / 100; n < (r.To+99)/100; n++ {
				m.Units = append(m.Units, p.Index*100+n)
			}
		}
	}
	if ranges > signal.MaxCoverageRanges || attention > uint64(activeMS)+uint64(len(pages)) {
		return m, fmt.Errorf("signal: page attention exceeds the active interval or range budget")
	}
	slices.Sort(m.Units)
	m.Units = slices.Compact(m.Units)
	slices.SortFunc(m.Attention, func(a, b [2]uint64) int {
		if a[0] < b[0] {
			return -1
		}
		if a[0] > b[0] {
			return 1
		}
		return 0
	})
	return m, nil
}

// MeasurePlayback normalizes only actually played ranges, not the furthest
// position reached. Timeline units have one-second precision.
func MeasurePlayback(watched []signal.WatchedRange, durationMS, activeMS uint32) (signal.Measurement, error) {
	m := signal.Measurement{ActiveMS: activeMS, Total: (durationMS + 999) / 1000, Scale: 1, Watched: watched, Units: []uint32{}, Attention: [][2]uint64{}}
	if durationMS == 0 || durationMS > 86_400_000 || activeMS == 0 || activeMS > signal.MaxEngagementMS || len(watched) == 0 || len(watched) > signal.MaxWatchedRanges {
		return m, fmt.Errorf("signal: invalid playback measurement size or attention")
	}
	var until uint32
	var played uint64
	perSecond := map[uint32]uint64{}
	for i, r := range watched {
		if r.FromMS >= r.ToMS || r.ToMS > durationMS || i > 0 && r.FromMS <= until {
			return m, fmt.Errorf("signal: watched ranges must be bounded, merged and ordered")
		}
		until = r.ToMS
		played += uint64(r.ToMS - r.FromMS)
		for n := r.FromMS / 1000; n < (r.ToMS+999)/1000; n++ {
			m.Units = append(m.Units, n)
			perSecond[n] += uint64(min(r.ToMS, (n+1)*1000) - max(r.FromMS, n*1000))
		}
	}
	// Match the player's supported measurement bound. A seek across a movie
	// cannot become complete coverage under one millisecond of attention.
	if played > uint64(activeMS)*4+250 {
		return m, fmt.Errorf("signal: played coverage exceeds active playback time")
	}
	slices.Sort(m.Units)
	m.Units = slices.Compact(m.Units)
	var allocated uint64
	for _, second := range m.Units {
		ms := uint64(activeMS) * perSecond[second] / played
		m.Attention = append(m.Attention, [2]uint64{uint64(second), ms})
		allocated += ms
	}
	if len(m.Attention) > 0 {
		m.Attention[len(m.Attention)-1][1] += uint64(activeMS) - allocated
	}
	return m, nil
}
