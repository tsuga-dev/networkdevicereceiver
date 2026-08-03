package profile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tsuga-dev/networkdevicereceiver/internal/profiledefinition"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore("")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

// TestResolveCorpus is WS2's exit criterion: every shipped profile resolves.
func TestResolveCorpus(t *testing.T) {
	s := newTestStore(t)
	if len(s.Names()) < 240 {
		t.Fatalf("loaded %d profiles, want >= 240", len(s.Names()))
	}

	var concrete, withMetrics, metadataOnly int
	for _, name := range s.Names() {
		def, err := s.Resolve(name)
		if err != nil {
			t.Errorf("resolve %s: %v", name, err)
			continue
		}
		if err := def.Validate(); err != nil {
			t.Errorf("resolved %s fails validation: %v", name, err)
		}
		if IsAbstract(name) {
			continue
		}
		concrete++

		switch {
		case len(def.Metrics) > 0:
			withMetrics++
		case len(def.Metadata) > 0:
			// Legitimate: vendor umbrella profiles such as ibm.yaml carry only
			// inventory fields and exist to be extended. A few (servertech,
			// avtech, tripplite) additionally declare a sysobjectid, so a device
			// can match them and yield inventory but no metrics -- that is
			// upstream's own behaviour, not a resolution failure.
			metadataOnly++
		default:
			t.Errorf("profile %s resolved to neither metrics nor metadata", name)
		}
	}
	t.Logf("%d concrete profiles: %d with metrics, %d metadata-only", concrete, withMetrics, metadataOnly)
}

// TestExtendsReach confirms inheritance actually delivers the shared IF-MIB
// metrics rather than silently resolving to nothing. The plan observed that
// _generic-if reaches ~93 of the 240 profiles.
func TestExtendsReach(t *testing.T) {
	s := newTestStore(t)
	const ifTableOID = "1.3.6.1.2.1.2.2"

	var reach int
	for _, name := range s.Names() {
		if IsAbstract(name) {
			continue
		}
		def, err := s.Resolve(name)
		if err != nil {
			continue
		}
		for _, m := range def.Metrics {
			if m.Table.OID == ifTableOID {
				reach++
				break
			}
		}
	}
	t.Logf("%d profiles collect ifTable via inheritance", reach)
	if reach < 50 {
		t.Errorf("ifTable reach = %d; extends resolution is not merging parents", reach)
	}
}

// TestDiamondInheritanceDoesNotDuplicate guards the dedup in expand(). Most
// profiles reach _base through several paths, and merging it twice would double
// every base metric and every datapoint derived from it.
func TestDiamondInheritanceDoesNotDuplicate(t *testing.T) {
	s := newTestStore(t)
	for _, name := range s.Names() {
		if IsAbstract(name) {
			continue
		}
		def, err := s.Resolve(name)
		if err != nil {
			continue
		}
		seen := map[string]int{}
		for _, m := range def.Metrics {
			seen[metricKey(m)]++
		}
		for key, n := range seen {
			if n > 1 {
				t.Errorf("%s: metric %s merged %d times", name, key, n)
			}
		}
	}
}

// metricKey identifies a metric entry for duplicate detection. Options are part
// of the key because flag_stream legitimately repeats one symbol per bit.
func metricKey(m profiledefinition.MetricsConfig) string {
	oids := make([]string, 0, len(m.Symbols)+1)
	oids = append(oids, m.Symbol.OID+"|"+m.Symbol.Name)
	for _, s := range m.Symbols {
		oids = append(oids, s.OID+"|"+s.Name)
	}
	return fmt.Sprintf("%s/%s/%s/%d/%s", m.Table.OID, strings.Join(oids, ","),
		m.MetricType, m.Options.Placement, m.Options.MetricSuffix)
}

// TestChildOverridesParentMetadata pins the merge precedence: a vendor profile
// that defines its own model field must not have a parent's definition win.
func TestChildOverridesParentMetadata(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("_parent-meta", `
metadata:
  device:
    fields:
      model:
        symbol: {OID: 1.1.1.1.0, name: parentModel}
      serial_number:
        symbol: {OID: 2.2.2.2.0, name: parentSerial}
`)
	write("child-meta", `
sysobjectid: 1.3.6.1.4.1.99999.1
extends:
  - _parent-meta
metadata:
  device:
    fields:
      model:
        symbol: {OID: 3.3.3.3.0, name: childModel}
metrics:
  - MIB: TEST-MIB
    symbol: {OID: 1.3.6.1.2.1.1.3.0, name: sysUpTime}
`)

	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	def, err := s.Resolve("child-meta")
	if err != nil {
		t.Fatal(err)
	}
	fields := def.Metadata["device"].Fields
	if got := fields["model"].Symbol.Name; got != "childModel" {
		t.Errorf("model = %q, want childModel (child must win)", got)
	}
	if got := fields["serial_number"].Symbol.Name; got != "parentSerial" {
		t.Errorf("serial_number = %q, want parentSerial (inherited)", got)
	}
}

func TestUserProfileShadowsEmbedded(t *testing.T) {
	dir := t.TempDir()
	body := `
sysobjectid: 1.3.6.1.4.1.9.1.99999
metrics:
  - MIB: OVERRIDE-MIB
    symbol: {OID: 9.9.9.9.0, name: overrideMarker}
`
	if err := os.WriteFile(filepath.Join(dir, "cisco-nexus.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	def, err := s.Resolve("cisco-nexus")
	if err != nil {
		t.Fatal(err)
	}
	if len(def.Metrics) != 1 || def.Metrics[0].Symbol.Name != "overrideMarker" {
		t.Errorf("user profile did not fully shadow embedded one: %d metrics", len(def.Metrics))
	}
}

// TestMatchCorpusPatterns walks every sysObjectID pattern in the corpus and
// asserts it resolves back to the profile that declared it. This is the
// (sysObjectID -> expected profile) parity table from WS2's exit criteria.
func TestMatchCorpusPatterns(t *testing.T) {
	s := newTestStore(t)

	var checked, mismatched int
	for _, name := range s.Names() {
		if IsAbstract(name) {
			continue
		}
		for _, pattern := range s.raw[name].SysObjectIDs {
			pattern = strings.TrimSpace(pattern)
			if pattern == "" {
				continue
			}
			// Substitute an implausible arc for wildcards so the probe cannot
			// accidentally land on another profile's literal pattern.
			probe := strings.ReplaceAll(pattern, "*", "99999")
			got, err := s.Match(probe)
			checked++
			if err != nil {
				t.Errorf("pattern %s (profile %s): %v", pattern, name, err)
				continue
			}
			if got != name {
				mismatched++
				t.Errorf("sysObjectID %s (from %s pattern %s) matched profile %s",
					probe, name, pattern, got)
			}
		}
	}
	t.Logf("checked %d sysObjectID patterns, %d mismatched", checked, mismatched)
	if checked < 1700 {
		t.Errorf("only checked %d patterns, expected ~1718", checked)
	}
}

func TestMatchMostSpecific(t *testing.T) {
	dir := t.TempDir()
	write := func(name, sysoid string) {
		body := fmt.Sprintf("sysobjectid: %s\nmetrics:\n  - MIB: T\n    symbol: {OID: 1.1.1.0, name: x}\n", sysoid)
		if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("vendor-broad", "1.3.6.1.4.1.99998.*")
	write("vendor-narrow", "1.3.6.1.4.1.99998.7.*")
	write("vendor-exact", "1.3.6.1.4.1.99998.7.1")

	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct{ oid, want string }{
		{"1.3.6.1.4.1.99998.7.1", "vendor-exact"},
		{"1.3.6.1.4.1.99998.7.2", "vendor-narrow"},
		{"1.3.6.1.4.1.99998.4", "vendor-broad"},
	}
	for _, tc := range tests {
		got, err := s.Match(tc.oid)
		if err != nil {
			t.Errorf("Match(%s): %v", tc.oid, err)
			continue
		}
		if got != tc.want {
			t.Errorf("Match(%s) = %s, want %s", tc.oid, got, tc.want)
		}
	}
}

func TestMatchConflictIsReported(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"dup-a", "dup-b"} {
		body := "sysobjectid: 1.3.6.1.4.1.99997.1\nmetrics:\n  - MIB: T\n    symbol: {OID: 1.1.1.0, name: x}\n"
		if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Match("1.3.6.1.4.1.99997.1"); err == nil {
		t.Error("expected an error for two equal-precedence profiles claiming one pattern")
	}
}

func TestMatchUnknownAndLeadingDot(t *testing.T) {
	s := newTestStore(t)

	// generic-device.yaml claims 1.3.6.1.4.*, so any unrecognised device under
	// the enterprises arc still resolves -- to the catch-all, by design.
	got, err := s.Match("1.3.6.1.4.1.123456789.1")
	if err != nil {
		t.Errorf("unknown enterprise OID should fall back to the catch-all: %v", err)
	} else if got != "generic-device" {
		t.Errorf("unknown enterprise OID matched %s, want generic-device", got)
	}

	// An OID outside every declared pattern has genuinely no profile.
	if _, err := s.Match("1.2.3.4.5"); err == nil {
		t.Error("expected no match for an OID outside all patterns")
	}
	if _, err := s.Match(""); err == nil {
		t.Error("expected an error for an empty sysObjectID")
	}
	// Devices commonly return a leading dot; it must not defeat matching.
	withDot, err := s.Match(".1.3.6.1.4.1.9.1.1")
	if err != nil {
		t.Fatalf("leading-dot sysObjectID: %v", err)
	}
	without, err := s.Match("1.3.6.1.4.1.9.1.1")
	if err != nil {
		t.Fatal(err)
	}
	if withDot != without {
		t.Errorf("leading dot changed the match: %s vs %s", withDot, without)
	}
}

func TestComparePatterns(t *testing.T) {
	tests := []struct {
		a, b string
		want string // "a", "b" or "eq"
	}{
		{"1.3.6.1.4.1.9.1.*", "1.3.6.1.4.1.9.*", "a"},
		{"1.3.6.1.4.1.9.*", "1.3.6.1.4.1.9.1.*", "b"},
		{"1.3.6.1.4.1.9.1", "1.3.6.1.4.1.9.*", "a"},
		{"1.3.6.1.4.1.9.*", "1.3.6.1.4.1.9.1", "b"},
		{"1.3.6.1.4.1.9.1", "1.3.6.1.4.1.9.1", "eq"},
	}
	for _, tc := range tests {
		got := comparePatterns(tc.a, tc.b)
		var label string
		switch {
		case got > 0:
			label = "a"
		case got < 0:
			label = "b"
		default:
			label = "eq"
		}
		if label != tc.want {
			t.Errorf("comparePatterns(%q, %q) = %d (%s), want %s", tc.a, tc.b, got, label, tc.want)
		}
	}
}

func TestUnknownExtendsIsAnError(t *testing.T) {
	dir := t.TempDir()
	body := "extends:\n  - _does-not-exist\nsysobjectid: 1.3.6.1.4.1.99996.1\n"
	if err := os.WriteFile(filepath.Join(dir, "broken.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resolve("broken"); err == nil {
		t.Error("expected an error when extending an unknown profile")
	}
}

// TestSelfReferentialExtendsTerminates guards against a hang, and against the
// profile's own content being merged into itself.
func TestSelfReferentialExtendsTerminates(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("cycle-a", "extends: [cycle-b]\nsysobjectid: 1.3.6.1.4.1.99995.1\nmetrics:\n  - MIB: T\n    symbol: {OID: 1.1.1.0, name: a}\n")
	write("cycle-b", "extends: [cycle-a]\nmetrics:\n  - MIB: T\n    symbol: {OID: 2.2.2.0, name: b}\n")

	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	def, err := s.Resolve("cycle-a")
	if err != nil {
		t.Fatalf("cycle should terminate, not error: %v", err)
	}
	if len(def.Metrics) != 2 {
		t.Errorf("got %d metrics, want 2 (each contributed once)", len(def.Metrics))
	}
}
