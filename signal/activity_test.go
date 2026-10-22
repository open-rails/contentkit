package signal

import (
	"reflect"
	"strings"
	"testing"
)

func TestActivityCohortValidation(t *testing.T) {
	for _, c := range [][]ActivityCohort{
		{{ID: "", Identity: ActivityAny}},
		{{ID: "same", Identity: ActivityAny}, {ID: "same", Identity: ActivityAccount}},
		{{ID: "valid", Identity: "unknown"}},
		{{ID: "guest", Identity: ActivityAnonymous, AnyTier: []string{"paid"}}},
		{{ID: "paid", Identity: ActivityAccount, AnyTier: []string{""}}},
		{{ID: "paid", Identity: ActivityAccount, AnyTier: []string{"one", "one"}}},
	} {
		if err := ValidateActivityCohorts(c); err == nil {
			t.Errorf("accepted invalid cohorts: %+v", c)
		}
	}
	if err := ValidateActivityCohorts(nil); err != nil {
		t.Fatal(err)
	}
}

func TestActivityCohortQueryPreservesPriorityAndBindsValues(t *testing.T) {
	cohorts := []ActivityCohort{
		{ID: "supporter", Identity: ActivityAccount, AnyTier: []string{"supporter' OR 1=1"}},
		{ID: "member", Identity: ActivityAccount},
		{ID: "guest", Identity: ActivityAnonymous},
	}
	if err := ValidateActivityCohorts(cohorts); err != nil {
		t.Fatal(err)
	}
	query, args := activityCohortSQL(cohorts)
	if strings.Contains(query, "supporter") || !strings.HasPrefix(query, "multiIf(d.subject_kind='user' AND hasAny") {
		t.Fatalf("predicate is not bound or precedence changed: %s", query)
	}
	want := []any{[]string{"supporter' OR 1=1"}, "supporter", "member", "guest"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %#v, want %#v", args, want)
	}
}
