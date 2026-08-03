package snmp

import (
	"sort"
	"testing"
)

func TestCompareOIDs(t *testing.T) {
	tests := []struct {
		a, b string
		want string // "lt", "gt" or "eq"
	}{
		// The case that motivates numeric comparison: as strings, "10" < "9".
		{"1.3.6.1.2.1.2.2.1.14.9", "1.3.6.1.2.1.2.2.1.14.10", "lt"},
		{"1.3.6.1.2.1.2.2.1.14.10", "1.3.6.1.2.1.2.2.1.14.9", "gt"},
		{"1.3.6.1.2.1.2.2.1.14.100", "1.3.6.1.2.1.2.2.1.14.99", "gt"},
		// A prefix precedes anything extending it.
		{"1.3.6.1.2.1.2.2", "1.3.6.1.2.1.2.2.1", "lt"},
		{"1.3.6.1.2.1.2.2.1", "1.3.6.1.2.1.2.2", "gt"},
		{"1.3.6.1.2.1.2.2", "1.3.6.1.2.1.2.2", "eq"},
		// A leading dot is not significant.
		{".1.3.6.1.2.1.1.3.0", "1.3.6.1.2.1.1.3.0", "eq"},
		// Multi-arc indexes, as used by cross-table joins.
		{"1.1.1.2.10", "1.1.1.10.2", "lt"},
	}
	for _, tc := range tests {
		got := CompareOIDs(tc.a, tc.b)
		var label string
		switch {
		case got < 0:
			label = "lt"
		case got > 0:
			label = "gt"
		default:
			label = "eq"
		}
		if label != tc.want {
			t.Errorf("CompareOIDs(%q, %q) = %d (%s), want %s", tc.a, tc.b, got, label, tc.want)
		}
	}
}

func TestOIDSortOrder(t *testing.T) {
	oids := []string{
		"1.3.6.1.2.1.2.2.1.10.11",
		"1.3.6.1.2.1.2.2.1.10.2",
		"1.3.6.1.2.1.2.2.1.10.1",
		"1.3.6.1.2.1.2.2.1.2.1",
	}
	sort.Slice(oids, func(i, j int) bool { return OIDLess(oids[i], oids[j]) })

	want := []string{
		"1.3.6.1.2.1.2.2.1.2.1",
		"1.3.6.1.2.1.2.2.1.10.1",
		"1.3.6.1.2.1.2.2.1.10.2",
		"1.3.6.1.2.1.2.2.1.10.11",
	}
	for i := range want {
		if oids[i] != want[i] {
			t.Errorf("position %d = %s, want %s (full: %v)", i, oids[i], want[i], oids)
		}
	}
}

func TestWithinSubtree(t *testing.T) {
	const column = "1.3.6.1.2.1.2.2.1.10"
	tests := []struct {
		oid  string
		want bool
	}{
		{column + ".1", true},
		{column + ".1.2.3", true},
		{column, false},                    // the column root itself holds no row
		{"1.3.6.1.2.1.2.2.1.16.1", false},  // sibling column
		{"1.3.6.1.2.1.2.2.1.100.1", false}, // shares a textual prefix but not an arc
	}
	for _, tc := range tests {
		if got := WithinSubtree(column, tc.oid); got != tc.want {
			t.Errorf("WithinSubtree(%s, %s) = %v, want %v", column, tc.oid, got, tc.want)
		}
	}
}

func TestResultValueStringFormatting(t *testing.T) {
	tests := []struct {
		value any
		want  string
	}{
		{float64(42), "42"},
		{float64(42.5), "42.5"},
		{"text", "text"},
		{[]byte("bytes"), "bytes"},
		// Integral floats must not become "1e+06", which would end up as an
		// attribute value on a metric.
		{float64(1000000), "1000000"},
	}
	for _, tc := range tests {
		got := ResultValue{Value: tc.value}.String()
		if got != tc.want {
			t.Errorf("ResultValue{%v}.String() = %q, want %q", tc.value, got, tc.want)
		}
	}
}
