package snmp

import (
	"strconv"
	"strings"
)

// CompareOIDs orders two OIDs the way SNMP does: arc by arc, numerically.
//
// String comparison is wrong here and the failure is subtle -- "1.3.6.1.2.1.2.2.1.14.10"
// sorts before "...14.9" lexicographically, which would make a walk look like it
// had stopped advancing on any table with ten or more rows.
//
// Returns a negative number if a sorts before b, zero if equal, positive otherwise.
func CompareOIDs(a, b string) int {
	as := strings.Split(CanonicalOID(a), ".")
	bs := strings.Split(CanonicalOID(b), ".")

	for i := 0; i < len(as) && i < len(bs); i++ {
		if as[i] == bs[i] {
			continue
		}
		an, aerr := strconv.ParseUint(as[i], 10, 64)
		bn, berr := strconv.ParseUint(bs[i], 10, 64)
		if aerr != nil || berr != nil {
			// Not a well-formed arc; fall back to a stable textual order rather
			// than claiming equality.
			return strings.Compare(as[i], bs[i])
		}
		switch {
		case an < bn:
			return -1
		case an > bn:
			return 1
		}
	}
	// A prefix sorts before anything that extends it.
	return len(as) - len(bs)
}

// OIDLess adapts CompareOIDs for sort.Slice.
func OIDLess(a, b string) bool { return CompareOIDs(a, b) < 0 }

// WithinSubtree reports whether oid lies strictly below root.
func WithinSubtree(root, oid string) bool {
	_, ok := rowIndex(root, oid)
	return ok
}
