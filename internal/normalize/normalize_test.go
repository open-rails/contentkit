package normalize

import "testing"

func TestQuery(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{"", ""}, {"  two   factor  ", "two factor"}, {"two-factor", "two factor"}, {"-factor", "factor"},
		{"--factor", "factor"}, {"two not factor", "two not factor"}, {"not two-factor", "not two factor"}, {"R-18", "R 18"},
	} {
		if got := Query(tc.in); got != tc.want {
			t.Fatalf("Query(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
	if HasAnyLetterOrNumber("!!! ???") || !HasAnyLetterOrNumber("鬼") {
		t.Fatal("HasAnyLetterOrNumber")
	}
}
