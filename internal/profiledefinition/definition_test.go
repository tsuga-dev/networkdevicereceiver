package profiledefinition

import (
	"os"
	"path/filepath"
	"testing"
)

const corpusDir = "../profile/default_profiles"

// TestCorpusParses is the parity check that matters: every shipped Datadog
// profile must parse with KnownFields enabled, normalize, and validate. A new
// field appearing upstream fails here instead of silently dropping metrics.
func TestCorpusParses(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(corpusDir, "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 240 {
		t.Fatalf("expected the full corpus, found %d files", len(files))
	}

	var symbols, metrics int
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			data, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			def, err := Unmarshal(data)
			if err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			def.Normalize()
			if err := def.Validate(); err != nil {
				t.Fatalf("validate: %v", err)
			}
			metrics += len(def.Metrics)
			for _, m := range def.Metrics {
				if m.IsColumn() {
					symbols += len(m.Symbols)
				} else {
					symbols++
				}
			}
		})
	}
	// The plan counted 2,532 symbol references across the corpus; hold that
	// line so a parsing regression that quietly skips entries is visible.
	t.Logf("parsed %d metric entries, %d symbol references", metrics, symbols)
	if symbols < 2500 {
		t.Errorf("symbol references = %d, expected ~2532; parser is dropping entries", symbols)
	}
}

// TestCorpusEdgeCases pins the three shapes an inattentive port gets wrong.
func TestCorpusEdgeCases(t *testing.T) {
	load := func(t *testing.T, name string) *ProfileDefinition {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(corpusDir, name))
		if err != nil {
			t.Fatal(err)
		}
		def, err := Unmarshal(data)
		if err != nil {
			t.Fatal(err)
		}
		def.Normalize()
		return def
	}

	t.Run("sysobjectid scalar and list", func(t *testing.T) {
		// arista.yaml declares a bare string, cisco-nexus a list.
		if got := load(t, "arista.yaml").SysObjectIDs; len(got) != 1 {
			t.Errorf("arista sysobjectid = %v, want 1 entry", got)
		}
		if got := load(t, "_generic-if.yaml").SysObjectIDs; len(got) != 0 {
			t.Errorf("abstract profile sysobjectid = %v, want none", got)
		}
	})

	t.Run("constant_value_one has no OID", func(t *testing.T) {
		def := load(t, "3com-huawei.yaml")
		var found bool
		for _, m := range def.Metrics {
			for _, s := range m.Symbols {
				if s.ConstantValueOne {
					found = true
					if s.OID != "" {
						t.Errorf("%s: expected no OID on constant symbol, got %q", s.Name, s.OID)
					}
					if s.Name == "" {
						t.Error("constant symbol must still carry a name")
					}
				}
			}
		}
		if !found {
			t.Skip("no constant_value_one symbol in this profile any more")
		}
	})

	t.Run("flag_stream options", func(t *testing.T) {
		def := load(t, "apc_ups.yaml")
		var n int
		for _, m := range def.Metrics {
			if m.MetricType != MetricTypeFlagStream {
				continue
			}
			n++
			if m.Options.Placement == 0 || m.Options.MetricSuffix == "" {
				t.Errorf("flag_stream %s missing options: %+v", m.Symbol.Name, m.Options)
			}
		}
		if n == 0 {
			t.Skip("no flag_stream metrics in this profile any more")
		}
	})

	t.Run("legacy device vendor folded into metadata", func(t *testing.T) {
		def := load(t, "apc_ups.yaml")
		if def.Device.Vendor != "" {
			t.Error("Device.Vendor should be cleared after Normalize")
		}
		if got := def.Metadata["device"].Fields["vendor"].Value; got != "apc" {
			t.Errorf("metadata vendor = %q, want apc", got)
		}
	})

	t.Run("index_transform parsed", func(t *testing.T) {
		def := load(t, "_cisco-wlc.yaml")
		var found bool
		for _, m := range def.Metrics {
			for _, tag := range m.MetricTags {
				if len(tag.IndexTransform) > 0 {
					found = true
					if tag.Table == "" {
						t.Errorf("tag %q has index_transform but no table to join against", tag.Tag)
					}
				}
			}
		}
		if !found {
			t.Skip("no index_transform in this profile any more")
		}
	})
}

func TestNormalizeLegacyTypes(t *testing.T) {
	tests := []struct {
		in   ProfileMetricType
		want ProfileMetricType
	}{
		{MetricTypeCounter, MetricTypeRate},
		{MetricTypePercent, MetricTypeGauge},
		{MetricTypeGauge, MetricTypeGauge},
		{MetricTypeMonotonicCount, MetricTypeMonotonicCount},
		{"", ""},
	}
	for _, tc := range tests {
		if got := tc.in.normalize(); got != tc.want {
			t.Errorf("normalize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNormalizeForcedTypeAndLegacyScalar(t *testing.T) {
	def, err := Unmarshal([]byte(`
metrics:
  - MIB: TEST-MIB
    forced_type: counter
    OID: 1.2.3.4.0
    name: legacyScalar
`))
	if err != nil {
		t.Fatal(err)
	}
	def.Normalize()

	m := def.Metrics[0]
	if m.MetricType != MetricTypeRate {
		t.Errorf("MetricType = %q, want rate (from forced_type: counter)", m.MetricType)
	}
	if m.ForcedType != "" {
		t.Error("ForcedType should be cleared")
	}
	if m.Symbol.OID != "1.2.3.4.0" || m.Symbol.Name != "legacyScalar" {
		t.Errorf("legacy scalar not folded into Symbol: %+v", m.Symbol)
	}
	if m.IsColumn() {
		t.Error("a legacy scalar must not be treated as a table walk")
	}
	if err := def.Validate(); err != nil {
		t.Errorf("validate: %v", err)
	}
}

func TestSymbolBareString(t *testing.T) {
	def, err := Unmarshal([]byte(`
metrics:
  - MIB: IF-MIB
    table: {OID: 1.3.6.1.2.1.2.2, name: ifTable}
    symbols: [ifInOctets]
    metric_tags:
      - {index: 1, tag: interface}
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := def.Metrics[0].Symbols[0].Name; got != "ifInOctets" {
		t.Errorf("bare string symbol name = %q", got)
	}
	// A bare-string symbol has no OID and is not a constant, so it must fail
	// validation rather than silently collecting nothing.
	if err := def.Validate(); err == nil {
		t.Error("expected validation error for symbol without OID")
	}
}

func TestValidateReportsAllProblems(t *testing.T) {
	def, err := Unmarshal([]byte(`
metrics:
  - MIB: TEST-MIB
    metric_type: flag_stream
    symbol: {OID: 1.2.3.0, name: state}
  - MIB: TEST-MIB
    table: {name: noOID}
    symbols: [{OID: 1.2.4.1, name: col}]
    metric_tags:
      - {tag: bad}
`))
	if err != nil {
		t.Fatal(err)
	}
	def.Normalize()
	err = def.Validate()
	if err == nil {
		t.Fatal("expected errors")
	}
	// flag_stream placement + suffix, table.OID, tag without symbol or index.
	for _, want := range []string{"options.placement", "options.metric_suffix", "table.OID", "needs symbol or index"} {
		if !contains(err.Error(), want) {
			t.Errorf("error missing %q:\n%v", want, err)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
