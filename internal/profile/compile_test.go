package profile

import (
	"slices"
	"testing"

	"github.com/tsuga-dev/networkdevicereceiver/internal/profiledefinition"
)

// TestCompileCorpus is WS2's compile exit criterion: every resolved profile
// compiles, and every concrete one asks the device for something.
func TestCompileCorpus(t *testing.T) {
	s := newTestStore(t)

	var totalScalars, totalColumns, compiled int
	for _, name := range s.Names() {
		def, err := s.Resolve(name)
		if err != nil {
			continue
		}
		c, err := Compile(def)
		if err != nil {
			t.Errorf("compile %s: %v", name, err)
			continue
		}
		compiled++
		totalScalars += len(c.ScalarOIDs)
		totalColumns += len(c.ColumnOIDs)

		if IsAbstract(name) {
			continue
		}
		if len(c.ScalarOIDs) == 0 && len(c.ColumnOIDs) == 0 {
			t.Errorf("%s compiled to zero OIDs", name)
		}
		for _, oid := range slices.Concat(c.ScalarOIDs, c.ColumnOIDs) {
			if oid == "" {
				t.Errorf("%s produced an empty OID", name)
			}
			if oid[0] == '.' {
				t.Errorf("%s produced a non-canonical OID %q", name, oid)
			}
		}
	}
	t.Logf("compiled %d profiles: %d scalar OIDs, %d column OIDs", compiled, totalScalars, totalColumns)
}

func TestCompileSplitsScalarsAndColumns(t *testing.T) {
	s := newTestStore(t)
	def, err := s.Resolve("_generic-if")
	if err != nil {
		t.Fatal(err)
	}
	c, err := Compile(def)
	if err != nil {
		t.Fatal(err)
	}

	// ifNumber is a scalar; ifTable columns and the ifXTable tag columns walk.
	if !slices.Contains(c.ScalarOIDs, "1.3.6.1.2.1.2.1.0") {
		t.Errorf("ifNumber missing from scalars: %v", c.ScalarOIDs)
	}
	if !slices.Contains(c.ColumnOIDs, "1.3.6.1.2.1.2.2.1.14") {
		t.Error("ifInErrors column missing")
	}
	// ifName lives in ifXTable but tags ifTable rows, so it must still be walked.
	if !slices.Contains(c.ColumnOIDs, "1.3.6.1.2.1.31.1.1.1.1") {
		t.Error("ifName tag column from the joined table missing")
	}
	if slices.Contains(c.ScalarOIDs, "1.3.6.1.2.1.2.2.1.14") {
		t.Error("a table column must not be fetched as a scalar")
	}
}

func TestCompileSkipsConstantValueOneSymbols(t *testing.T) {
	s := newTestStore(t)
	def, err := s.Resolve("3com-huawei")
	if err != nil {
		t.Fatal(err)
	}
	c, err := Compile(def)
	if err != nil {
		t.Fatal(err)
	}
	// Constant symbols carry no OID; if one leaked in, the poller would ask for
	// an empty OID and the device would answer NoSuchObject every cycle.
	for _, oid := range slices.Concat(c.ScalarOIDs, c.ColumnOIDs) {
		if oid == "" {
			t.Fatal("constant_value_one symbol leaked into the fetch set")
		}
	}

	var constants int
	for _, m := range def.Metrics {
		for _, sym := range m.Symbols {
			if sym.ConstantValueOne {
				constants++
			}
		}
	}
	if constants == 0 {
		t.Skip("profile no longer uses constant_value_one")
	}
}

func TestCompilePrecompilesRegexes(t *testing.T) {
	s := newTestStore(t)
	// _arista-metadata uses match_pattern to pull a model out of sysDescr.
	def, err := s.Resolve("_arista-metadata")
	if err != nil {
		t.Fatal(err)
	}
	c, err := Compile(def)
	if err != nil {
		t.Fatal(err)
	}

	var pattern string
	for _, field := range def.Metadata["device"].Fields {
		for _, sym := range field.Symbols {
			if sym.MatchPattern != "" {
				pattern = sym.MatchPattern
			}
		}
	}
	if pattern == "" {
		t.Skip("profile no longer uses match_pattern")
	}
	if c.Regexp(pattern) == nil {
		t.Errorf("match_pattern %q was not precompiled", pattern)
	}
	if c.Regexp("") != nil {
		t.Error("empty pattern should yield nil")
	}
}

func TestCompileRejectsBadRegex(t *testing.T) {
	def, err := profiledefinition.Unmarshal([]byte(`
metrics:
  - MIB: TEST-MIB
    symbol:
      OID: 1.2.3.4.0
      name: broken
      match_pattern: "([unclosed"
      match_value: "$1"
`))
	if err != nil {
		t.Fatal(err)
	}
	def.Normalize()
	if _, err := Compile(def); err == nil {
		t.Error("expected compile to reject an invalid regex")
	}
}

func TestMissingOIDPruning(t *testing.T) {
	s := newTestStore(t)
	def, err := s.Resolve("_generic-if")
	if err != nil {
		t.Fatal(err)
	}
	c, err := Compile(def)
	if err != nil {
		t.Fatal(err)
	}

	scalars, columns := c.FetchOIDs()
	scalarsBefore, columnsBefore := len(scalars), len(columns)
	if scalarsBefore == 0 || columnsBefore < 2 {
		t.Fatalf("need at least 1 scalar and 2 columns, got %d/%d", scalarsBefore, columnsBefore)
	}
	prunedScalar, prunedColumn, keptColumn := scalars[0], columns[0], columns[1]

	c.MarkMissing(prunedScalar, prunedColumn)
	if got := c.MissingCount(); got != 2 {
		t.Errorf("MissingCount = %d, want 2", got)
	}

	scalars, columns = c.FetchOIDs()
	if len(scalars) != scalarsBefore-1 {
		t.Errorf("scalars = %d, want %d after pruning", len(scalars), scalarsBefore-1)
	}
	if len(columns) != columnsBefore-1 {
		t.Errorf("columns = %d, want %d after pruning", len(columns), columnsBefore-1)
	}
	if slices.Contains(columns, prunedColumn) {
		t.Error("pruned column still fetched")
	}
	if !slices.Contains(columns, keptColumn) {
		t.Error("pruning dropped an OID it should have kept")
	}

	// Re-marking is idempotent, including via the non-canonical spelling.
	c.MarkMissing("." + prunedColumn)
	c.MarkMissing(prunedColumn)
	if got := c.MissingCount(); got != 2 {
		t.Errorf("MissingCount = %d after re-marking, want 2", got)
	}
}
