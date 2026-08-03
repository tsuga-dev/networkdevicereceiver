package report_test

import (
	"testing"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/collector/pdata/pcommon"

	"github.com/tsuga-dev/networkdevicereceiver/internal/naming"
	"github.com/tsuga-dev/networkdevicereceiver/internal/profile"
	"github.com/tsuga-dev/networkdevicereceiver/internal/profiledefinition"
	"github.com/tsuga-dev/networkdevicereceiver/internal/report"
	"github.com/tsuga-dev/networkdevicereceiver/internal/snmp"
	"github.com/tsuga-dev/networkdevicereceiver/internal/snmptest"
)

// TestBinaryTagValueIsValidUTF8 covers a failure mode observed in production:
// a tag column holding a raw binary OCTET STRING (a six-byte MAC address, say)
// is not valid UTF-8, and OTLP string fields must be. An invalid attribute makes
// the receiving backend reject the entire request, so one such column silently
// destroys every metric for that device -- not just the offending datapoint.
func TestBinaryTagValueIsValidUTF8(t *testing.T) {
	const (
		table  = "1.3.6.1.4.1.9.9.500.1.2.1"
		state  = "1.3.6.1.4.1.9.9.500.1.2.1.1.6" // cswSwitchState
		macCol = "1.3.6.1.4.1.9.9.500.1.2.1.1.7" // a raw MAC address column
	)

	def, err := profiledefinition.Unmarshal([]byte(`
metrics:
  - MIB: CISCO-STACKWISE-MIB
    table: {OID: 1.3.6.1.4.1.9.9.500.1.2.1, name: cswSwitchInfoTable}
    symbols:
      - OID: 1.3.6.1.4.1.9.9.500.1.2.1.1.6
        name: cswSwitchState
    metric_tags:
      - symbol: {OID: 1.3.6.1.4.1.9.9.500.1.2.1.1.7, name: cswSwitchMacAddress}
        tag: mac_addr
`))
	if err != nil {
		t.Fatal(err)
	}
	def.Normalize()
	compiled, err := profile.Compile(def)
	if err != nil {
		t.Fatal(err)
	}

	// Six raw MAC octets, exactly as an agent returns them. 0xf4 alone is not a
	// valid UTF-8 sequence.
	mac := []byte{0xf4, 0x7f, 0x35, 0x93, 0xaf, 0x80}
	if utf8.Valid(mac) {
		t.Fatal("test fixture is not actually invalid UTF-8")
	}

	dev := snmptest.New()
	dev.SetInt(state+".1", 4)
	dev.SetBytes(macCol+".1", mac)

	registry, err := naming.New(naming.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	builder := report.NewBuilder(registry)

	scalars, columns := compiled.FetchOIDs()
	values, _, err := snmp.Fetch(dev, scalars, columns, snmp.FetchConfig{})
	if err != nil {
		t.Fatal(err)
	}
	md, _, err := builder.Build(report.DeviceInfo{
		ID: "10.0.0.1", Address: "10.0.0.1", ProfileName: "test",
	}, compiled, values, time.Now())
	if err != nil {
		t.Logf("build reported: %v", err)
	}

	assertUTF8 := func(where, k string, v pcommon.Value) {
		if !utf8.ValidString(v.AsString()) {
			t.Errorf("%s attribute %q holds invalid UTF-8: %q", where, k, v.AsString())
		}
	}
	var checked int
	for i := 0; i < md.ResourceMetrics().Len(); i++ {
		rm := md.ResourceMetrics().At(i)
		for k, v := range rm.Resource().Attributes().All() {
			assertUTF8("resource", k, v)
		}
		sms := rm.ScopeMetrics()
		for j := 0; j < sms.Len(); j++ {
			ms := sms.At(j).Metrics()
			for m := 0; m < ms.Len(); m++ {
				points := dataPoints(ms.At(m))
				for p := 0; p < points.Len(); p++ {
					for k, v := range points.At(p).Attributes().All() {
						checked++
						assertUTF8("metric "+ms.At(m).Name(), k, v)
					}
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no datapoint attributes were produced, so nothing was checked")
	}
}
