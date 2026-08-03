package naming

import (
	"testing"

	"github.com/tsuga-dev/networkdevicereceiver/internal/profiledefinition"
)

func sym(name string) profiledefinition.SymbolConfig {
	return profiledefinition.SymbolConfig{Name: name}
}

func newRegistry(t *testing.T, opts Options) *Registry {
	t.Helper()
	r, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

func TestRegistryLoads(t *testing.T) {
	r := newRegistry(t, DefaultOptions())
	if r.Curated() == 0 {
		t.Fatal("registry loaded no entries")
	}
	t.Logf("%d curated symbols", r.Curated())
}

func TestResolveInterfaceTraffic(t *testing.T) {
	r := newRegistry(t, DefaultOptions())

	in := r.Resolve("IF-MIB", sym("ifHCInOctets"))
	if in.Generated {
		t.Error("ifHCInOctets should be curated, not generated")
	}
	if got := in.Names[0]; got != "hw.network.io" {
		t.Errorf("metric = %q, want hw.network.io", got)
	}
	if in.Entry.Instrument != Sum {
		t.Errorf("instrument = %q, want sum", in.Entry.Instrument)
	}
	if in.Entry.Unit != "By" {
		t.Errorf("unit = %q, want By", in.Entry.Unit)
	}
	if got := in.Entry.Attributes["network.io.direction"]; got != "receive" {
		t.Errorf("direction = %q, want receive", got)
	}
	if got := in.Entry.Attributes["hw.type"]; got != "network" {
		t.Errorf("hw.type = %q, want network", got)
	}
	if in.Entry.Tier != TierHardware {
		t.Errorf("tier = %d, want 1", in.Entry.Tier)
	}

	out := r.Resolve("IF-MIB", sym("ifHCOutOctets"))
	if got := out.Entry.Attributes["network.io.direction"]; got != "transmit" {
		t.Errorf("out direction = %q, want transmit", got)
	}
	// Both directions are the same metric, distinguished only by attribute.
	if out.Names[0] != in.Names[0] {
		t.Errorf("in/out should share a metric name: %q vs %q", in.Names[0], out.Names[0])
	}
}

// TestRegistryOverridesProfileInstrument pins the rule that semconv fixes the
// instrument: profiles declare ifHighSpeed a gauge, but the convention requires
// an UpDownCounter.
func TestRegistryOverridesProfileInstrument(t *testing.T) {
	r := newRegistry(t, DefaultOptions())
	res := r.Resolve("IF-MIB", sym("ifHighSpeed"))
	if res.Entry.Instrument != UpDownCounter {
		t.Errorf("instrument = %q, want updowncounter", res.Entry.Instrument)
	}
	if res.Entry.Unit != "By/s" {
		t.Errorf("unit = %q, want By/s", res.Entry.Unit)
	}
	// Mbit/s -> By/s.
	if res.Entry.Scale != 125000 {
		t.Errorf("scale = %v, want 125000", res.Entry.Scale)
	}

	// ifSpeed reports bit/s, so it needs a different factor for the same metric.
	slow := r.Resolve("IF-MIB", sym("ifSpeed"))
	if slow.Names[0] != res.Names[0] {
		t.Errorf("ifSpeed should map to %q, got %q", res.Names[0], slow.Names[0])
	}
	if slow.Entry.Scale != 0.125 {
		t.Errorf("ifSpeed scale = %v, want 0.125", slow.Entry.Scale)
	}
}

func TestResolveStateSet(t *testing.T) {
	r := newRegistry(t, DefaultOptions())
	res := r.Resolve("IF-MIB", sym("ifAdminStatus"))

	if res.Names[0] != "hw.status" {
		t.Fatalf("metric = %q, want hw.status", res.Names[0])
	}
	ss := res.Entry.StateSet
	if ss == nil {
		t.Fatal("expected a state set")
	}
	if ss.Attribute != "hw.state" {
		t.Errorf("attribute = %q, want hw.state", ss.Attribute)
	}
	// Every state must be listed, because a state set emits a 0 for the
	// inactive ones as well as a 1 for the active one.
	for _, want := range []string{"ok", "degraded", "failed"} {
		var found bool
		for _, s := range ss.States {
			if s == want {
				found = true
			}
		}
		if !found {
			t.Errorf("states %v missing %q", ss.States, want)
		}
	}
	if got := ss.Map["1"]; got != "ok" {
		t.Errorf("admin status 1 maps to %q, want ok", got)
	}
	if got := ss.Map["2"]; got != "failed" {
		t.Errorf("admin status 2 maps to %q, want failed", got)
	}
}

func TestResolveValueMap(t *testing.T) {
	r := newRegistry(t, DefaultOptions())
	res := r.Resolve("IF-MIB", sym("ifOperStatus"))
	if res.Names[0] != "hw.network.up" {
		t.Fatalf("metric = %q, want hw.network.up", res.Names[0])
	}
	if got, ok := res.Entry.ValueMap["1"]; !ok || got != 1 {
		t.Errorf("oper status 1 -> %v (present=%v), want 1", got, ok)
	}
	if got, ok := res.Entry.ValueMap["default"]; !ok || got != 0 {
		t.Errorf("default -> %v (present=%v), want 0", got, ok)
	}
}

func TestResolveTypeDispatch(t *testing.T) {
	r := newRegistry(t, DefaultOptions())
	res := r.Resolve("ENTITY-SENSOR-MIB", sym("entPhySensorValue"))

	td := res.Entry.TypeDispatch
	if td == nil {
		t.Fatal("entPhySensorValue must dispatch on its sibling type column")
	}
	if td.TypeSymbol != "entPhySensorType" {
		t.Errorf("type symbol = %q", td.TypeSymbol)
	}

	// Each case must resolve to a real entry, or a sensor row would silently
	// produce nothing.
	want := map[string]struct{ metric, unit string }{
		"8":  {"hw.temperature", "Cel"},
		"4":  {"hw.voltage", "V"},
		"10": {"hw.fan.speed", "rpm"},
		"6":  {"hw.power", "W"},
	}
	for code, expected := range want {
		target, ok := td.Cases[code]
		if !ok {
			t.Errorf("sensor type %s has no case", code)
			continue
		}
		entry, ok := r.EntryByName(target)
		if !ok {
			t.Errorf("case %s points at unknown entry %q", code, target)
			continue
		}
		if entry.Metric != expected.metric {
			t.Errorf("sensor type %s -> %q, want %q", code, entry.Metric, expected.metric)
		}
		if entry.Unit != expected.unit {
			t.Errorf("sensor type %s unit = %q, want %q", code, entry.Unit, expected.unit)
		}
	}
}

// TestSystemTierIsOptIn covers the namespace decision: cpu/memory fall back to
// snmp.* unless the operator opts into system.*.
func TestSystemTierIsOptIn(t *testing.T) {
	off := newRegistry(t, DefaultOptions())
	res := off.Resolve("UCD-SNMP-MIB", sym("cpu.usage"))
	if !res.Generated {
		t.Error("cpu.usage should fall back when the system tier is off")
	}
	if got := res.Names[0]; got != "snmp.ucd_snmp.cpu.usage" {
		t.Errorf("fallback name = %q", got)
	}

	opts := DefaultOptions()
	opts.SystemNamespaceForDeviceOS = true
	on := newRegistry(t, opts)
	res = on.Resolve("UCD-SNMP-MIB", sym("cpu.usage"))
	if res.Generated {
		t.Error("cpu.usage should be curated when the system tier is on")
	}
	if got := res.Names[0]; got != "system.cpu.utilization" {
		t.Errorf("metric = %q, want system.cpu.utilization", got)
	}
	if res.Entry.Tier != TierSystem {
		t.Errorf("tier = %d, want 2", res.Entry.Tier)
	}
	// SNMP reports a percentage; semconv utilization is a fraction.
	if res.Entry.Scale != 0.01 {
		t.Errorf("scale = %v, want 0.01", res.Entry.Scale)
	}
}

func TestFallbackNames(t *testing.T) {
	r := newRegistry(t, DefaultOptions())
	tests := []struct {
		mib, symbol, want string
	}{
		{"CISCO-MEMORY-POOL-MIB", "memPoolUsed", "snmp.cisco_memory_pool.mem_pool_used"},
		{"IF-MIB", "ifHCInOctets", "snmp.if.if_hc_in_octets"},
		{"PowerNet-MIB", "upsBasicStateOutputState", "snmp.powernet.ups_basic_state_output_state"},
		{"", "someSymbol", "snmp.some_symbol"},
		{"F5-BIGIP-SYSTEM-MIB", "sysMultiHostCpuUser", "snmp.f5_bigip_system.sys_multi_host_cpu_user"},
		// Acronym runs must not be split letter by letter.
		{"CISCO-ENTITY-FRU-CONTROL-MIB", "cefcFRUPowerStatus", "snmp.cisco_entity_fru_control.cefc_fru_power_status"},
	}
	for _, tc := range tests {
		if got := r.FallbackName(tc.mib, tc.symbol); got != tc.want {
			t.Errorf("FallbackName(%q, %q) = %q, want %q", tc.mib, tc.symbol, got, tc.want)
		}
	}
}

func TestFallbackNamespaceConfigurable(t *testing.T) {
	opts := DefaultOptions()
	opts.FallbackNamespace = "network.device"
	r := newRegistry(t, opts)
	if got := r.FallbackName("IF-MIB", "ifFoo"); got != "network.device.if.if_foo" {
		t.Errorf("got %q", got)
	}
}

func TestCamelToSnake(t *testing.T) {
	tests := []struct{ in, want string }{
		{"ifHCInOctets", "if_hc_in_octets"},
		{"memPoolUsed", "mem_pool_used"},
		{"cefcFRUPowerStatus", "cefc_fru_power_status"},
		{"sysUpTime", "sys_up_time"},
		{"cpu", "cpu"},
		{"CPU", "cpu"},
		{"tcpActiveOpens", "tcp_active_opens"},
		{"bsnAPName", "bsn_ap_name"},
		{"upsAdvBattery2Capacity", "ups_adv_battery2_capacity"},
	}
	for _, tc := range tests {
		if got := camelToSnake(tc.in); got != tc.want {
			t.Errorf("camelToSnake(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestDatadogCompatScheme(t *testing.T) {
	opts := DefaultOptions()
	opts.Scheme = SchemeDatadogCompat
	r := newRegistry(t, opts)

	res := r.Resolve("IF-MIB", sym("ifHCInOctets"))
	if len(res.Names) != 1 || res.Names[0] != "snmp.ifHCInOctets" {
		t.Errorf("names = %v, want [snmp.ifHCInOctets]", res.Names)
	}
	// Attributes and units from the curated entry are retained, so a compat-mode
	// series is still usable; only the name changes.
	if res.Entry.Unit != "By" {
		t.Errorf("unit = %q, want By retained in compat mode", res.Entry.Unit)
	}
}

func TestBothSchemeEmitsTwoNames(t *testing.T) {
	opts := DefaultOptions()
	opts.Scheme = SchemeBoth
	r := newRegistry(t, opts)

	res := r.Resolve("IF-MIB", sym("ifHCInOctets"))
	if len(res.Names) != 2 {
		t.Fatalf("names = %v, want both semconv and compat", res.Names)
	}
	if res.Names[0] != "hw.network.io" || res.Names[1] != "snmp.ifHCInOctets" {
		t.Errorf("names = %v", res.Names)
	}
}

func TestUnknownSchemeRejected(t *testing.T) {
	opts := DefaultOptions()
	opts.Scheme = "invented"
	if _, err := New(opts); err == nil {
		t.Error("expected an error for an unknown scheme")
	}
}

// TestUnmodelledSymbolStillEmits is the safety property: an unmapped vendor
// symbol must produce a metric rather than disappear.
func TestUnmodelledSymbolStillEmits(t *testing.T) {
	r := newRegistry(t, DefaultOptions())
	res := r.Resolve("ACME-PRIVATE-MIB", sym("acmeWidgetTemperature"))
	if len(res.Names) != 1 || res.Names[0] == "" {
		t.Fatalf("names = %v, want a generated name", res.Names)
	}
	if !res.Generated {
		t.Error("should be reported as generated")
	}
	if res.Entry.Tier != TierFallback {
		t.Errorf("tier = %d, want 3", res.Entry.Tier)
	}
	if res.Entry.Scale != 1 {
		t.Errorf("scale = %v, want 1", res.Entry.Scale)
	}
	if res.Entry.Instrument != Gauge {
		t.Errorf("instrument = %q, want gauge as the safe default", res.Entry.Instrument)
	}
}
