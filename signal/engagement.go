package signal

import (
	"fmt"
	"slices"
)

// TypeEngagement records measured consumption, never opening a page or reader.
// Its snapshots are cumulative within a bounded segment. A segment's stable
// receipt and revision make retries independent of the number of deliveries.
const TypeEngagement = "engagement"

const engagementPayloadKey = "contentkit_engagement"

const (
	MaxEngagementPages = 128
	MaxPageRanges      = 16
	MaxCoverageRanges  = 512
	MaxWatchedRanges   = 256
	MaxEngagementMS    = 600_000
	CoverageScale      = 10_000
)

// CoverageRange is an actually visible part of a loaded page, in basis points.
type CoverageRange struct {
	From uint32 `json:"from"`
	To   uint32 `json:"to"`
}

// PageEngagement retains actual exposure and attention separately: reaching a
// page number does not imply that earlier pages were read.
type PageEngagement struct {
	Index    uint32          `json:"index"`
	Ranges   []CoverageRange `json:"ranges"`
	ActiveMS uint32          `json:"active_ms"`
}

// WatchedRange is a played timeline interval. Seeking is not watching.
type WatchedRange struct {
	FromMS uint32 `json:"from_ms"`
	ToMS   uint32 `json:"to_ms"`
}

// Measurement is the trusted, normalized storage form. Covered units are
// hundredths of a reader page, or seconds of a player's timeline. Attention is
// retained per page (per second for players), so short partials can accumulate
// across segments and visits without counting visits themselves.
type Measurement struct {
	VersionID     string   `json:"version_id"`
	VisitID       string   `json:"visit_id"`
	OriginClickID string   `json:"origin_click_id,omitempty"`
	SegmentID     string   `json:"segment_id"`
	ActiveMS      uint32   `json:"active_ms"`
	Total         uint32   `json:"total"`
	Scale         uint32   `json:"scale"`
	Units         []uint32 `json:"units"`
	// Pairs are [logical unit index, active milliseconds], sorted by index.
	Attention [][2]uint64 `json:"attention"`
	// Raw bounded measurements remain available for later quality policies.
	Pages   []PageEngagement `json:"pages,omitempty"`
	Watched []WatchedRange   `json:"watched,omitempty"`
}

// MeasurePages validates and normalizes a reader's measured page coverage.
func MeasurePages(pages []PageEngagement, total, activeMS uint32) (Measurement, error) {
	m := Measurement{ActiveMS: activeMS, Total: total * 100, Scale: 100, Pages: pages, Units: []uint32{}, Attention: [][2]uint64{}}
	if total == 0 || total > 100_000 || activeMS == 0 || activeMS > MaxEngagementMS || len(pages) == 0 || len(pages) > MaxEngagementPages {
		return m, fmt.Errorf("signal: invalid page measurement size or attention")
	}
	seen := map[uint32]bool{}
	var attention uint64
	ranges := 0
	for _, p := range pages {
		if p.Index >= total || seen[p.Index] || p.ActiveMS == 0 || len(p.Ranges) == 0 || len(p.Ranges) > MaxPageRanges {
			return m, fmt.Errorf("signal: invalid or duplicate measured page %d", p.Index)
		}
		seen[p.Index] = true
		attention += uint64(p.ActiveMS)
		ranges += len(p.Ranges)
		m.Attention = append(m.Attention, [2]uint64{uint64(p.Index), uint64(p.ActiveMS)})
		var until uint32
		for i, r := range p.Ranges {
			if r.From >= r.To || r.To > CoverageScale || i > 0 && r.From <= until {
				return m, fmt.Errorf("signal: page coverage ranges must be merged and ordered")
			}
			until = r.To
			for n := r.From / 100; n < (r.To+99)/100; n++ {
				m.Units = append(m.Units, p.Index*100+n)
			}
		}
	}
	if ranges > MaxCoverageRanges || attention > uint64(activeMS)+uint64(len(pages)) {
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
func MeasurePlayback(watched []WatchedRange, durationMS, activeMS uint32) (Measurement, error) {
	m := Measurement{ActiveMS: activeMS, Total: (durationMS + 999) / 1000, Scale: 1, Watched: watched, Units: []uint32{}, Attention: [][2]uint64{}}
	if durationMS == 0 || durationMS > 86_400_000 || activeMS == 0 || activeMS > MaxEngagementMS || len(watched) == 0 || len(watched) > MaxWatchedRanges {
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
