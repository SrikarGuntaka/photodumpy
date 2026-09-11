package store

import (
	"strings"
	"testing"
	"time"
)

func ptrBool(b bool) *bool { return &b }

// The sort map is the boundary that keeps user-supplied text out of the query
// string. Anything not in it must be rejected.
func TestSortAllowList(t *testing.T) {
	for _, s := range []PhotoSort{SortPath, SortCaptured, SortSize, SortQuality, SortCreated} {
		if !ValidSort(s) {
			t.Errorf("ValidSort(%q) = false, want true", s)
		}
	}

	// The shapes an injection attempt takes, plus ordinary typos.
	for _, s := range []PhotoSort{
		"", "pathh", "relative_path", "1",
		"path; DROP TABLE photos",
		"path--",
		"(SELECT 1)",
	} {
		if ValidSort(s) {
			t.Errorf("ValidSort(%q) = true, want false", s)
		}
	}
}

// Every sort expression must be a bare column reference plus optional
// modifiers -- no user input can reach it, and nothing here should ever grow
// a subquery or a function call.
func TestSortExpressionsAreSimple(t *testing.T) {
	for key, expr := range sortColumns {
		if strings.ContainsAny(expr, ";()'\"") {
			t.Errorf("sortColumns[%q] = %q contains suspicious punctuation", key, expr)
		}
	}
}

func TestValidSortsIsDeterministic(t *testing.T) {
	first := strings.Join(ValidSorts(), ",")
	for i := 0; i < 20; i++ {
		if got := strings.Join(ValidSorts(), ","); got != first {
			t.Fatalf("ValidSorts() varies between calls: %q then %q", first, got)
		}
	}
}

func TestEscapeLike(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"holiday", "holiday"},
		// A literal underscore must not become a single-character wildcard.
		{"photo_1", `photo\_1`},
		// A literal percent must not match everything.
		{"50%", `50\%`},
		{"%", `\%`},
		// The backslash is escaped FIRST, otherwise it would escape the
		// escapes added afterwards and the pattern would be malformed.
		{`a\b`, `a\\b`},
		{`\%`, `\\\%`},
		{"", ""},
	}
	for _, tc := range cases {
		if got := escapeLike(tc.in); got != tc.want {
			t.Errorf("escapeLike(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The count query and the page query share one predicate builder. If they
// could drift, a paginator would offer pages that do not exist.
func TestPredicateIsShapedForBothQueries(t *testing.T) {
	from := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	f := PhotoFilter{
		State:         "processing",
		Flag:          "possibly_blurry",
		HasGPS:        ptrBool(true),
		HasDuplicates: ptrBool(false),
		CapturedFrom:  &from,
		PathContains:  "trip",
	}

	where, args := f.predicate("lib-1")

	// One arg for the library, plus one for each value-bearing clause. The
	// presence filters bind nothing -- they are operators, not values.
	if len(args) != 5 {
		t.Errorf("got %d args %v, want 5 (library, state, flag, from, path)", len(args), args)
	}
	if args[0] != "lib-1" {
		t.Errorf("args[0] = %v, want the library id first", args[0])
	}

	// Every bound value must appear as a placeholder, never inline.
	for _, literal := range []string{"processing", "possibly_blurry", "trip"} {
		if strings.Contains(where, literal) {
			t.Errorf("predicate inlines %q instead of binding it:\n%s", literal, where)
		}
	}
	for _, want := range []string{"$1", "$2", "$3", "$4", "$5"} {
		if !strings.Contains(where, want) {
			t.Errorf("predicate is missing placeholder %s:\n%s", want, where)
		}
	}
}

func TestPredicatePlaceholdersAreSequential(t *testing.T) {
	to := time.Now()
	f := PhotoFilter{State: "processing", Flag: "possibly_blurry", CapturedTo: &to, PathContains: "x"}
	where, args := f.predicate("lib")

	// A gap or a repeat here means a clause is bound to the wrong value --
	// the failure mode of hand-numbering placeholders.
	for i := 1; i <= len(args); i++ {
		marker := "$" + string(rune('0'+i))
		if i < 10 && !strings.Contains(where, marker) {
			t.Errorf("placeholder %s missing; predicate binds %d args:\n%s", marker, len(args), where)
		}
	}
}

// Absent, true and false are three different requests.
func TestPresenceFiltersAreTriState(t *testing.T) {
	absent, _ := PhotoFilter{}.predicate("lib")
	if strings.Contains(absent, "latitude") {
		t.Errorf("no has_gps filter should emit no latitude predicate:\n%s", absent)
	}

	yes, _ := PhotoFilter{HasGPS: ptrBool(true)}.predicate("lib")
	if !strings.Contains(yes, "latitude IS NOT NULL") {
		t.Errorf("has_gps=true should require coordinates:\n%s", yes)
	}

	no, _ := PhotoFilter{HasGPS: ptrBool(false)}.predicate("lib")
	if !strings.Contains(no, "latitude IS NULL") {
		t.Errorf("has_gps=false should require their absence:\n%s", no)
	}
	if strings.Contains(no, "IS NOT NULL") {
		t.Errorf("has_gps=false emitted the positive predicate too:\n%s", no)
	}
}

func TestMembershipNegationUsesNotExists(t *testing.T) {
	if got := membership("similar_group_members", true); !strings.HasPrefix(got, "EXISTS") {
		t.Errorf("got %q, want EXISTS", got)
	}
	if got := membership("similar_group_members", false); !strings.HasPrefix(got, "NOT EXISTS") {
		t.Errorf("got %q, want NOT EXISTS", got)
	}
}
