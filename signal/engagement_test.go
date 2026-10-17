package signal

import (
	"slices"
	"testing"
)

func TestMeasuredPagesDoNotInferSkippedPages(t *testing.T) {
	m, err := MeasurePages([]PageEngagement{{Index: 99, Ranges: []CoverageRange{{From: 0, To: 10_000}}, ActiveMS: 1200}}, 100, 1200)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Units) != 100 || m.Total != 10_000 || slices.Contains(m.Units, 0) || m.Attention[0] != [2]uint64{99, 1200} {
		t.Fatalf("a jump to page 100 must cover only that page: %+v", m)
	}
}

func TestMeasuredTallPageKeepsItsPartialCoverage(t *testing.T) {
	m, err := MeasurePages([]PageEngagement{{Index: 0, Ranges: []CoverageRange{{From: 0, To: 1000}}, ActiveMS: 600}}, 1, 600)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Units) != 10 || m.Total != 100 || m.Attention[0][1] != 600 {
		t.Fatalf("the first viewport of a tall page is not the whole page: %+v", m)
	}
}

func TestMeasuredPlaybackDoesNotInferSeekedRanges(t *testing.T) {
	m, err := MeasurePlayback([]WatchedRange{{FromMS: 60_000, ToMS: 61_200}}, 120_000, 1200)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(m.Units, []uint32{60, 61}) || m.Total != 120 {
		t.Fatalf("seek is not played coverage: %+v", m)
	}
	if m.Attention[0][1]+m.Attention[1][1] != 1200 {
		t.Fatalf("lost measured timeline attention: %+v", m.Attention)
	}
	if _, err := MeasurePlayback([]WatchedRange{{FromMS: 0, ToMS: 86_400_000}}, 86_400_000, 1); err == nil {
		t.Fatal("accepted an entire video under one millisecond of attention")
	}
}

func TestMeasurementRejectsUnloadedZeroAttentionAndOverclaim(t *testing.T) {
	valid := PageEngagement{Index: 0, Ranges: []CoverageRange{{From: 0, To: 10_000}}, ActiveMS: 1200}
	for _, tc := range []struct {
		pages  []PageEngagement
		active uint32
	}{
		{[]PageEngagement{valid}, 0},
		{[]PageEngagement{{Index: 0, ActiveMS: 1200}}, 1200},
		{[]PageEngagement{valid}, 600},
		{[]PageEngagement{valid, valid}, 2400},
		{[]PageEngagement{{Index: 1, Ranges: valid.Ranges, ActiveMS: 1200}}, 1200},
	} {
		if _, err := MeasurePages(tc.pages, 1, tc.active); err == nil {
			t.Fatalf("accepted invalid measurement: %+v", tc)
		}
	}
}
