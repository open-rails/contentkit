package contentkit

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/open-rails/contentkit/eval"
)

const (
	// evalBaselinePath is the committed golden report the regression gate
	// compares against. Regenerate with CONTENTKIT_EVAL_UPDATE=1.
	evalBaselinePath = "eval/testdata/golden_gallery_baseline.json"
	evalUpdateEnv    = "CONTENTKIT_EVAL_UPDATE"
)

// newEvalTestClient provisions an isolated schema, seeds a small deterministic
// corpus, and returns a keyword client. Shared by the baseline-gate and
// config-diff tests so the corpus stays identical.
func newEvalTestClient(t *testing.T) (context.Context, *Client) {
	t.Helper()
	pool := testPG(t)
	ctx := context.Background()
	schema := keywordSchema(t, ctx, pool)
	upsertDocs(t, ctx, pool, schema,
		doc("gallery", "1", "en", "two factor authentication", nil, nil),
		doc("gallery", "2", "en", "two factor backup codes", nil, nil),
		doc("gallery", "3", "en", "single sign on saml", nil, nil),
		doc("gallery", "4", "en", "password reset email flow", nil, nil),
		doc("gallery", "5", "en", "unrelated cooking recipe", nil, nil),
	)
	client, err := NewClient(ClientConfig{Pool: pool, Schema: schema, Tenant: testTenant})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return ctx, client
}

func loadGoldenSuite(t *testing.T) eval.Suite {
	t.Helper()
	f, err := os.Open("eval/testdata/golden_gallery.json")
	if err != nil {
		t.Fatalf("open golden suite: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	suite, err := eval.ParseSuite(f)
	if err != nil {
		t.Fatalf("parse golden suite: %v", err)
	}
	return suite
}

// runGolden executes the golden suite under one search configuration and
// builds a report.
func runGolden(ctx context.Context, t *testing.T, client *Client, suite eval.Suite, candidateID string, base SearchOptions) eval.Report {
	t.Helper()
	base.Language = "en"
	runner := NewEvalRunner(client, base)
	identity := eval.ReportIdentity{DatasetID: "gallery-smoke", SuiteID: suite.ID, CandidateID: candidateID}
	report, err := eval.RunSuite(ctx, suite, runner, identity, "query_type")
	if err != nil {
		t.Fatalf("RunSuite(%s): %v", candidateID, err)
	}
	return report
}

// TestEvalRunSuite_Integration runs the committed lexical golden suite through
// client.Search against a real Postgres, asserts a clean baseline, and gates on
// the committed baseline report so a future quality regression fails CI.
// Regenerate the baseline with CONTENTKIT_EVAL_UPDATE=1.
func TestEvalRunSuite_Integration(t *testing.T) {
	ctx, client := newEvalTestClient(t)
	suite := loadGoldenSuite(t)
	report := runGolden(ctx, t, client, suite, "keyword", SearchOptions{})

	if report.Metrics.Cases != 4 || report.Metrics.FailedCases != 0 || report.Metrics.JudgedCases != 3 {
		t.Fatalf("metrics = %+v, want 4 cases, 0 failed, 3 judged", report.Metrics)
	}
	if report.Metrics.RecallAtK != 1 || report.Metrics.SuccessAtK != 1 {
		t.Fatalf("recall/success = %v/%v, want 1/1", report.Metrics.RecallAtK, report.Metrics.SuccessAtK)
	}
	if report.Metrics.NDCGAtK < 0.7 {
		t.Fatalf("NDCGAtK = %v, want >= 0.7 (top-graded docs rank near top)", report.Metrics.NDCGAtK)
	}
	if report.Metrics.EmptyCases != 1 || report.Metrics.ExactEmptyRate != 1 {
		t.Fatalf("empty metrics = {cases:%d rate:%v}, want {1, 1}", report.Metrics.EmptyCases, report.Metrics.ExactEmptyRate)
	}
	if _, ok := report.Breakdowns["query_type"]; !ok {
		t.Fatalf("missing query_type breakdown: %+v", report.Breakdowns)
	}
	if report.ContentID == "" {
		t.Fatal("report ContentID is empty")
	}

	// Golden-file regression gate: update on demand, otherwise compare.
	if os.Getenv(evalUpdateEnv) != "" {
		writeBaselineReport(t, evalBaselinePath, report)
		t.Logf("wrote baseline %s (%s set)", evalBaselinePath, evalUpdateEnv)
		return
	}
	baseline := loadReport(t, evalBaselinePath)
	comparison, err := eval.Compare(baseline, report, eval.Tolerances{MRRAtKDrop: 0.05, NDCGAtKDrop: 0.05})
	if err != nil {
		t.Fatalf("Compare(baseline, current): %v", err)
	}
	if !comparison.Compatible {
		t.Fatalf("baseline incompatible with current report (regenerate with %s=1): %v", evalUpdateEnv, comparison.Mismatches)
	}
	if comparison.Regressed() {
		t.Fatalf("search quality regressed vs committed baseline: %+v", comparison.Regressions)
	}
}

// TestEvalConfigDiff_Integration proves the eval can compare two configurations
// side by side: a host filter that hides gallery 1 regresses recall on the
// cases that judge it, and the comparator flags it.
func TestEvalConfigDiff_Integration(t *testing.T) {
	ctx, client := newEvalTestClient(t)
	suite := loadGoldenSuite(t)

	full := runGolden(ctx, t, client, suite, "keyword", SearchOptions{})
	filtered := runGolden(ctx, t, client, suite, "keyword-without-1", SearchOptions{FilterSQL: "sd.content_id <> @hidden", FilterArgs: map[string]any{"hidden": "1"}})
	if full.Metrics.RecallAtK != 1 || filtered.Metrics.RecallAtK >= 1 {
		t.Fatalf("recall full=%v filtered=%v", full.Metrics.RecallAtK, filtered.Metrics.RecallAtK)
	}
	comparison, err := eval.Compare(full, filtered, eval.Tolerances{})
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if !comparison.Compatible || !comparison.Regressed() {
		t.Fatalf("expected a comparable regression: %+v", comparison)
	}
	var sawRecall bool
	for _, r := range comparison.Regressions {
		if r.Metric == "recall_at_k" {
			sawRecall = true
		}
	}
	if !sawRecall {
		t.Fatalf("expected a recall_at_k regression, got %+v", comparison.Regressions)
	}
}

func loadReport(t *testing.T, path string) eval.Report {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open baseline %s (generate with %s=1): %v", path, evalUpdateEnv, err)
	}
	defer func() { _ = f.Close() }()
	var report eval.Report
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&report); err != nil {
		t.Fatalf("decode baseline %s: %v", path, err)
	}
	return report
}

func writeBaselineReport(t *testing.T, path string, report eval.Report) {
	t.Helper()
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatalf("marshal baseline: %v", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write baseline %s: %v", path, err)
	}
}
