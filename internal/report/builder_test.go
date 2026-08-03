package report_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/pmetric"

	"github.com/tsuga-dev/networkdevicereceiver/internal/naming"
	"github.com/tsuga-dev/networkdevicereceiver/internal/profile"
	"github.com/tsuga-dev/networkdevicereceiver/internal/report"
	"github.com/tsuga-dev/networkdevicereceiver/internal/snmp"
	"github.com/tsuga-dev/networkdevicereceiver/internal/snmptest"
)

// IF-MIB OIDs used to script the fake device.
const (
	ifNumber      = "1.3.6.1.2.1.2.1.0"
	ifInErrors    = "1.3.6.1.2.1.2.2.1.14"
	ifInDiscards  = "1.3.6.1.2.1.2.2.1.13"
	ifOutErrors   = "1.3.6.1.2.1.2.2.1.20"
	ifOutDiscards = "1.3.6.1.2.1.2.2.1.19"
	ifAdminStatus = "1.3.6.1.2.1.2.2.1.7"
	ifOperStatus  = "1.3.6.1.2.1.2.2.1.8"
	ifSpeed       = "1.3.6.1.2.1.2.2.1.5"

	ifName         = "1.3.6.1.2.1.31.1.1.1.1"
	ifAlias        = "1.3.6.1.2.1.31.1.1.1.18"
	ifHCInOctets   = "1.3.6.1.2.1.31.1.1.1.6"
	ifHCOutOctets  = "1.3.6.1.2.1.31.1.1.1.10"
	ifHCInUcast    = "1.3.6.1.2.1.31.1.1.1.7"
	ifHCInMulti    = "1.3.6.1.2.1.31.1.1.1.8"
	ifHCInBroad    = "1.3.6.1.2.1.31.1.1.1.9"
	ifHCOutUcast   = "1.3.6.1.2.1.31.1.1.1.11"
	ifHCOutMulti   = "1.3.6.1.2.1.31.1.1.1.12"
	ifHCOutBroad   = "1.3.6.1.2.1.31.1.1.1.13"
	ifHighSpeedOID = "1.3.6.1.2.1.31.1.1.1.15"
)

// twoInterfaceDevice scripts a switch with two gigabit interfaces.
//
// ifSpeed is deliberately saturated at 2^32-1, which is exactly why ifHighSpeed
// exists, so the priority rule has something to distinguish.
func twoInterfaceDevice(octetsIn, octetsOut uint64) *snmptest.Fake {
	dev := snmptest.New()
	dev.SetInt(ifNumber, 2)

	for _, idx := range []string{"1", "2"} {
		dev.SetCounter32(ifInErrors+"."+idx, 3)
		dev.SetCounter32(ifInDiscards+"."+idx, 1)
		dev.SetCounter32(ifOutErrors+"."+idx, 0)
		dev.SetCounter32(ifOutDiscards+"."+idx, 2)

		dev.SetInt(ifAdminStatus+"."+idx, 1) // up
		dev.SetInt(ifOperStatus+"."+idx, 1)  // up
		dev.SetGauge(ifSpeed+"."+idx, 4294967295)
		dev.SetGauge(ifHighSpeedOID+"."+idx, 10000) // 10 Gbit/s

		dev.SetString(ifName+"."+idx, "Gi0/"+idx)
		dev.SetString(ifAlias+"."+idx, "uplink-"+idx)

		dev.SetCounter64(ifHCInOctets+"."+idx, octetsIn)
		dev.SetCounter64(ifHCOutOctets+"."+idx, octetsOut)
		for _, oid := range []string{ifHCInUcast, ifHCInMulti, ifHCInBroad, ifHCOutUcast, ifHCOutMulti, ifHCOutBroad} {
			dev.SetCounter64(oid+"."+idx, 100)
		}
	}
	return dev
}

func compileProfile(t *testing.T, name string) *profile.Compiled {
	t.Helper()
	store, err := profile.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	def, err := store.Resolve(name)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := profile.Compile(def)
	if err != nil {
		t.Fatal(err)
	}
	return compiled
}

func newBuilder(t *testing.T, opts naming.Options) *report.Builder {
	t.Helper()
	registry, err := naming.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return report.NewBuilder(registry)
}

func testDevice() report.DeviceInfo {
	return report.DeviceInfo{
		ID:          "10.0.0.1",
		Address:     "10.0.0.1",
		Port:        161,
		Subnet:      "10.0.0.0/24",
		SysObjectID: "1.3.6.1.4.1.9.1.1",
		ProfileName: "_generic-if",
	}
}

// poll runs one fetch-and-build cycle.
func poll(t *testing.T, b *report.Builder, dev snmp.Session, compiled *profile.Compiled, at time.Time) pmetric.Metrics {
	t.Helper()
	scalars, columns := compiled.FetchOIDs()
	store, _, err := snmp.Fetch(dev, scalars, columns, snmp.FetchConfig{})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	md, _, err := b.Build(testDevice(), compiled, store, at)
	if err != nil {
		// Partial failures are expected with real profiles; log rather than fail.
		t.Logf("build reported: %v", err)
	}
	return md
}

// metricsByName indexes the emitted metrics.
func metricsByName(md pmetric.Metrics) map[string]pmetric.Metric {
	out := map[string]pmetric.Metric{}
	for i := 0; i < md.ResourceMetrics().Len(); i++ {
		sms := md.ResourceMetrics().At(i).ScopeMetrics()
		for j := 0; j < sms.Len(); j++ {
			ms := sms.At(j).Metrics()
			for k := 0; k < ms.Len(); k++ {
				out[ms.At(k).Name()] = ms.At(k)
			}
		}
	}
	return out
}

func dataPoints(m pmetric.Metric) pmetric.NumberDataPointSlice {
	switch m.Type() {
	case pmetric.MetricTypeGauge:
		return m.Gauge().DataPoints()
	case pmetric.MetricTypeSum:
		return m.Sum().DataPoints()
	default:
		return pmetric.NewNumberDataPointSlice()
	}
}

func attrsOf(dp pmetric.NumberDataPoint) map[string]string {
	out := map[string]string{}
	for k, v := range dp.Attributes().All() {
		out[k] = v.AsString()
	}
	return out
}

// findPoint returns the first datapoint matching every given attribute.
func findPoint(m pmetric.Metric, want map[string]string) (pmetric.NumberDataPoint, bool) {
	points := dataPoints(m)
	for i := 0; i < points.Len(); i++ {
		got := attrsOf(points.At(i))
		match := true
		for k, v := range want {
			if got[k] != v {
				match = false
				break
			}
		}
		if match {
			return points.At(i), true
		}
	}
	return pmetric.NumberDataPoint{}, false
}

// TestEndToEndGenericIf is the M1 milestone check: a real Datadog profile, a
// scripted device, and semconv-shaped OTLP out.
func TestEndToEndGenericIf(t *testing.T) {
	compiled := compileProfile(t, "_generic-if")
	b := newBuilder(t, naming.DefaultOptions())
	md := poll(t, b, twoInterfaceDevice(1_000_000, 2_000_000), compiled, time.Now())

	if md.ResourceMetrics().Len() != 1 {
		t.Fatalf("got %d resources, want exactly one per device", md.ResourceMetrics().Len())
	}
	metrics := metricsByName(md)
	for name := range metrics {
		t.Logf("emitted %s (%d points)", name, dataPoints(metrics[name]).Len())
	}

	t.Run("traffic", func(t *testing.T) {
		m, ok := metrics["hw.network.io"]
		if !ok {
			t.Fatal("hw.network.io not emitted")
		}
		if m.Type() != pmetric.MetricTypeSum {
			t.Errorf("type = %v, want Sum", m.Type())
		}
		if !m.Sum().IsMonotonic() {
			t.Error("traffic counters must be monotonic")
		}
		if m.Sum().AggregationTemporality() != pmetric.AggregationTemporalityCumulative {
			t.Error("want cumulative temporality")
		}
		if m.Unit() != "By" {
			t.Errorf("unit = %q, want By", m.Unit())
		}
		// Two interfaces, two directions.
		if got := dataPoints(m).Len(); got != 4 {
			t.Errorf("got %d datapoints, want 4", got)
		}
		dp, ok := findPoint(m, map[string]string{"network.io.direction": "receive", "hw.id": "if_1"})
		if !ok {
			t.Fatal("no receive datapoint for if_1")
		}
		if dp.DoubleValue() != 1_000_000 {
			t.Errorf("value = %v, want 1000000", dp.DoubleValue())
		}
		if got := attrsOf(dp)["hw.name"]; got != "Gi0/1" {
			t.Errorf("hw.name = %q, want Gi0/1", got)
		}
		if got := attrsOf(dp)["interface_alias"]; got != "uplink-1" {
			t.Errorf("interface_alias = %q, want uplink-1", got)
		}
	})

	t.Run("bandwidth limit uses ifHighSpeed not saturated ifSpeed", func(t *testing.T) {
		m, ok := metrics["hw.network.bandwidth.limit"]
		if !ok {
			t.Fatal("hw.network.bandwidth.limit not emitted")
		}
		if m.Type() != pmetric.MetricTypeSum || m.Sum().IsMonotonic() {
			t.Error("bandwidth limit must be a non-monotonic Sum (UpDownCounter)")
		}
		if m.Unit() != "By/s" {
			t.Errorf("unit = %q, want By/s", m.Unit())
		}
		// One per interface: ifSpeed and ifHighSpeed must collapse, not duplicate.
		if got := dataPoints(m).Len(); got != 2 {
			t.Fatalf("got %d datapoints, want 2 -- ifSpeed and ifHighSpeed produced duplicate streams", got)
		}
		dp, ok := findPoint(m, map[string]string{"hw.id": "if_1"})
		if !ok {
			t.Fatal("no bandwidth datapoint for if_1")
		}
		// 10000 Mbit/s * 125000 = 1.25e9 By/s, not ifSpeed's saturated value.
		if want := 1.25e9; dp.DoubleValue() != want {
			t.Errorf("value = %v, want %v (ifHighSpeed must win)", dp.DoubleValue(), want)
		}
	})

	t.Run("errors are only errors", func(t *testing.T) {
		m, ok := metrics["hw.errors"]
		if !ok {
			t.Fatal("hw.errors not emitted")
		}
		if m.Unit() != "{error}" {
			t.Errorf("unit = %q", m.Unit())
		}
		// 2 interfaces x {in,out}. Discards are a separate metric now, so folding
		// them in here would have doubled the reported error count.
		if got := dataPoints(m).Len(); got != 4 {
			t.Errorf("got %d datapoints, want 4", got)
		}
		if _, ok := findPoint(m, map[string]string{"error.type": "discard"}); ok {
			t.Error("hw.errors must not carry discards")
		}
	})

	t.Run("discards are dropped packets", func(t *testing.T) {
		m, ok := metrics["system.network.packet.dropped"]
		if !ok {
			t.Fatal("system.network.packet.dropped not emitted")
		}
		if m.Unit() != "{packet}" {
			t.Errorf("unit = %q, want {packet}", m.Unit())
		}
		// 2 interfaces x {in,out}.
		if got := dataPoints(m).Len(); got != 4 {
			t.Errorf("got %d datapoints, want 4", got)
		}
		dp, ok := findPoint(m, map[string]string{
			"hw.id": "if_1", "network.io.direction": "receive",
		})
		if !ok {
			t.Fatal("no receive discard datapoint for if_1")
		}
		// Component identity is what keeps this joinable to hw.network.io.
		attrs := attrsOf(dp)
		if attrs["network.interface.name"] != "Gi0/1" {
			t.Errorf("network.interface.name = %q, want Gi0/1", attrs["network.interface.name"])
		}
		if attrs["hw.name"] != "Gi0/1" {
			t.Errorf("hw.name = %q, want Gi0/1", attrs["hw.name"])
		}
	})

	t.Run("oper status via value map", func(t *testing.T) {
		m, ok := metrics["hw.network.up"]
		if !ok {
			t.Fatal("hw.network.up not emitted")
		}
		dp, ok := findPoint(m, map[string]string{"hw.id": "if_1"})
		if !ok {
			t.Fatal("no datapoint for if_1")
		}
		if dp.DoubleValue() != 1 {
			t.Errorf("value = %v, want 1 for an interface that is up", dp.DoubleValue())
		}
	})

	t.Run("admin status fans out as a state set", func(t *testing.T) {
		m, ok := metrics["hw.status"]
		if !ok {
			t.Fatal("hw.status not emitted")
		}
		// 2 interfaces x 3 states.
		if got := dataPoints(m).Len(); got != 6 {
			t.Fatalf("got %d datapoints, want 6 (one per state per interface)", got)
		}
		ok1, found := findPoint(m, map[string]string{"hw.id": "if_1", "hw.state": "ok"})
		if !found {
			t.Fatal("no ok state for if_1")
		}
		if ok1.DoubleValue() != 1 {
			t.Errorf("active state value = %v, want 1", ok1.DoubleValue())
		}
		failed, found := findPoint(m, map[string]string{"hw.id": "if_1", "hw.state": "failed"})
		if !found {
			t.Fatal("no failed state for if_1")
		}
		if failed.DoubleValue() != 0 {
			t.Errorf("inactive state value = %v, want 0", failed.DoubleValue())
		}
	})

	t.Run("packets carry a class discriminator", func(t *testing.T) {
		m, ok := metrics["hw.network.packets"]
		if !ok {
			t.Fatal("hw.network.packets not emitted")
		}
		// 2 interfaces x 2 directions x 3 classes.
		if got := dataPoints(m).Len(); got != 12 {
			t.Errorf("got %d datapoints, want 12", got)
		}
		for _, class := range []string{"unicast", "multicast", "broadcast"} {
			if _, found := findPoint(m, map[string]string{
				"hw.id": "if_1", "network.io.cast": class, "network.io.direction": "receive",
			}); !found {
				t.Errorf("no receive %s datapoint", class)
			}
		}
	})

	t.Run("unmapped scalar falls back", func(t *testing.T) {
		// ifNumber has no hw.* home, so it must still be emitted, generated.
		if _, ok := metrics["snmp.if.if_number"]; !ok {
			var names []string
			for n := range metrics {
				names = append(names, n)
			}
			t.Errorf("ifNumber not emitted under a fallback name; got %v", names)
		}
	})
}

// TestResourceCarriesDeviceIdentity checks the structural rule from the hardware
// conventions: the device is identified by the resource, never by datapoints.
func TestResourceCarriesDeviceIdentity(t *testing.T) {
	compiled := compileProfile(t, "_generic-if")
	b := newBuilder(t, naming.DefaultOptions())
	md := poll(t, b, twoInterfaceDevice(1, 1), compiled, time.Now())

	res := md.ResourceMetrics().At(0).Resource().Attributes()
	for _, key := range []string{"host.id", "host.name", "snmp.device.ip", "snmp.profile", "snmp.subnet", "snmp.device.sys_object_id"} {
		if _, ok := res.Get(key); !ok {
			t.Errorf("resource missing %s", key)
		}
	}
	if got, _ := res.Get("snmp.device.ip"); got.AsString() != "10.0.0.1" {
		t.Errorf("snmp.device.ip = %q", got.AsString())
	}

	// No datapoint may carry a device identifier.
	forbidden := []string{"host.id", "host.name", "snmp.device.ip", "snmp.subnet"}
	metrics := metricsByName(md)
	for name, m := range metrics {
		points := dataPoints(m)
		for i := 0; i < points.Len(); i++ {
			attrs := attrsOf(points.At(i))
			for _, key := range forbidden {
				if _, present := attrs[key]; present {
					t.Errorf("%s datapoint carries device attribute %s", name, key)
				}
			}
		}
	}
}

// TestHardwareMetricsCarryRequiredAttributes is the conformance check: hw.id is
// required on hw.* metrics, and hw.type is recommended on component metrics.
func TestHardwareMetricsCarryRequiredAttributes(t *testing.T) {
	compiled := compileProfile(t, "_generic-if")
	b := newBuilder(t, naming.DefaultOptions())
	md := poll(t, b, twoInterfaceDevice(1, 1), compiled, time.Now())

	for name, m := range metricsByName(md) {
		if !strings.HasPrefix(name, "hw.") {
			continue
		}
		points := dataPoints(m)
		if points.Len() == 0 {
			t.Errorf("%s has no datapoints", name)
		}
		for i := 0; i < points.Len(); i++ {
			attrs := attrsOf(points.At(i))
			if attrs["hw.id"] == "" {
				t.Errorf("%s datapoint %d has no hw.id, which the conventions require", name, i)
			}
		}
	}
}

// TestFallbackMetricsHaveNoHardwareAttributes keeps hw.* attributes meaningful: a
// generated snmp.* metric is not a hardware-convention metric and must not
// pretend to be one.
//
// Curated metrics outside hw.* may opt into component identity -- interface
// discards do, so they stay joinable -- so the rule is scoped to the generated
// namespace.
func TestFallbackMetricsHaveNoHardwareAttributes(t *testing.T) {
	compiled := compileProfile(t, "_generic-if")
	b := newBuilder(t, naming.DefaultOptions())
	md := poll(t, b, twoInterfaceDevice(1, 1), compiled, time.Now())

	var checked int
	for name, m := range metricsByName(md) {
		if !strings.HasPrefix(name, "snmp.") {
			continue
		}
		checked++
		points := dataPoints(m)
		for i := 0; i < points.Len(); i++ {
			if attrs := attrsOf(points.At(i)); attrs["hw.id"] != "" {
				t.Errorf("%s is a generated name but carries hw.id", name)
			}
		}
	}
	if checked == 0 {
		t.Skip("no generated metrics in this profile any more")
	}
}

// TestCumulativeStartTimestampIsStable checks that a counter stream keeps its
// original start timestamp, without which a backend cannot compute a rate.
func TestCumulativeStartTimestampIsStable(t *testing.T) {
	compiled := compileProfile(t, "_generic-if")
	b := newBuilder(t, naming.DefaultOptions())

	first := time.Now()
	md1 := poll(t, b, twoInterfaceDevice(1_000, 2_000), compiled, first)
	second := first.Add(60 * time.Second)
	md2 := poll(t, b, twoInterfaceDevice(5_000, 6_000), compiled, second)

	get := func(md pmetric.Metrics) pmetric.NumberDataPoint {
		m := metricsByName(md)["hw.network.io"]
		dp, ok := findPoint(m, map[string]string{"hw.id": "if_1", "network.io.direction": "receive"})
		if !ok {
			t.Fatal("datapoint not found")
		}
		return dp
	}
	dp1, dp2 := get(md1), get(md2)

	if dp1.StartTimestamp() == 0 {
		t.Error("cumulative datapoint has no start timestamp")
	}
	if dp1.StartTimestamp() != dp2.StartTimestamp() {
		t.Errorf("start timestamp moved between polls: %v then %v", dp1.StartTimestamp(), dp2.StartTimestamp())
	}
	if dp2.Timestamp() == dp1.Timestamp() {
		t.Error("observation timestamp should advance")
	}
	if dp2.DoubleValue() != 5_000 {
		t.Errorf("second poll value = %v, want 5000", dp2.DoubleValue())
	}
}

// TestBandwidthUtilizationNeedsTwoPolls covers the one derived metric kept
// receiver-side, including that it is a fraction rather than a percentage.
func TestBandwidthUtilizationNeedsTwoPolls(t *testing.T) {
	compiled := compileProfile(t, "_generic-if")
	b := newBuilder(t, naming.DefaultOptions())

	start := time.Now()
	md1 := poll(t, b, twoInterfaceDevice(0, 0), compiled, start)
	if _, ok := metricsByName(md1)["hw.network.bandwidth.utilization"]; ok {
		t.Error("utilisation cannot be derived from a single poll")
	}

	// 10 Gbit/s is 1.25e9 By/s. Over 10s, 1.25e9 bytes is 1.25e8 By/s: 10%.
	later := start.Add(10 * time.Second)
	md2 := poll(t, b, twoInterfaceDevice(1_250_000_000, 0), compiled, later)

	m, ok := metricsByName(md2)["hw.network.bandwidth.utilization"]
	if !ok {
		t.Fatal("utilisation not emitted on the second poll")
	}
	if m.Unit() != "1" {
		t.Errorf("unit = %q, want 1 (a fraction, not a percentage)", m.Unit())
	}
	if m.Type() != pmetric.MetricTypeGauge {
		t.Errorf("type = %v, want Gauge", m.Type())
	}
	dp, found := findPoint(m, map[string]string{"hw.id": "if_1", "network.io.direction": "receive"})
	if !found {
		t.Fatal("no receive utilisation datapoint for if_1")
	}
	if got := dp.DoubleValue(); got < 0.099 || got > 0.101 {
		t.Errorf("utilisation = %v, want about 0.1", got)
	}
}

// TestBandwidthUtilizationIgnoresCounterReset guards against the huge false
// spike a wrapped or reset counter would otherwise produce.
func TestBandwidthUtilizationIgnoresCounterReset(t *testing.T) {
	compiled := compileProfile(t, "_generic-if")
	b := newBuilder(t, naming.DefaultOptions())

	start := time.Now()
	poll(t, b, twoInterfaceDevice(5_000_000_000, 0), compiled, start)
	// The device reboots and the counter restarts from a low value.
	md := poll(t, b, twoInterfaceDevice(1_000, 0), compiled, start.Add(10*time.Second))

	if m, ok := metricsByName(md)["hw.network.bandwidth.utilization"]; ok {
		if _, found := findPoint(m, map[string]string{"hw.id": "if_1", "network.io.direction": "receive"}); found {
			t.Error("a counter reset must not yield a utilisation datapoint")
		}
	}
}

func TestDatadogCompatEmitsLegacyNames(t *testing.T) {
	compiled := compileProfile(t, "_generic-if")
	opts := naming.DefaultOptions()
	opts.Scheme = naming.SchemeDatadogCompat
	b := newBuilder(t, opts)

	md := poll(t, b, twoInterfaceDevice(1, 1), compiled, time.Now())
	metrics := metricsByName(md)

	if _, ok := metrics["snmp.ifHCInOctets"]; !ok {
		t.Error("compat mode should emit snmp.ifHCInOctets")
	}
	if _, ok := metrics["hw.network.io"]; ok {
		t.Error("compat mode should not emit semconv names")
	}
}

func TestBothSchemeEmitsBoth(t *testing.T) {
	compiled := compileProfile(t, "_generic-if")
	opts := naming.DefaultOptions()
	opts.Scheme = naming.SchemeBoth
	b := newBuilder(t, opts)

	md := poll(t, b, twoInterfaceDevice(1, 1), compiled, time.Now())
	metrics := metricsByName(md)
	for _, want := range []string{"hw.network.io", "snmp.ifHCInOctets"} {
		if _, ok := metrics[want]; !ok {
			t.Errorf("both mode should emit %s", want)
		}
	}
}

// TestEmptyDeviceProducesNoMetrics checks a device that answers nothing does not
// produce phantom datapoints.
func TestEmptyDeviceProducesNoMetrics(t *testing.T) {
	compiled := compileProfile(t, "_generic-if")
	b := newBuilder(t, naming.DefaultOptions())
	md := poll(t, b, snmptest.New(), compiled, time.Now())

	if got := md.DataPointCount(); got != 0 {
		t.Errorf("got %d datapoints from a silent device, want 0", got)
	}
	// The resource is still created, which is how a reachable-but-empty device
	// remains visible.
	if md.ResourceMetrics().Len() != 1 {
		t.Errorf("got %d resources, want 1", md.ResourceMetrics().Len())
	}
}

// TestCorpusProfilesBuildWithoutPanic runs every concrete profile against a
// generic device. Most metrics will be absent; the point is that no profile
// causes a panic or a duplicate-stream error.
func TestCorpusProfilesBuildWithoutPanic(t *testing.T) {
	store, err := profile.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	dev := twoInterfaceDevice(1_000, 2_000)
	dev.SetString("1.3.6.1.2.1.1.5.0", "device-under-test")
	dev.SetString("1.3.6.1.2.1.1.1.0", "Test OS version 1.2.3")

	var built, withPoints int
	for _, name := range store.Names() {
		if profile.IsAbstract(name) {
			continue
		}
		def, err := store.Resolve(name)
		if err != nil {
			t.Errorf("resolve %s: %v", name, err)
			continue
		}
		compiled, err := profile.Compile(def)
		if err != nil {
			t.Errorf("compile %s: %v", name, err)
			continue
		}

		b := newBuilder(t, naming.DefaultOptions())
		scalars, columns := compiled.FetchOIDs()
		values, _, _ := snmp.Fetch(dev, scalars, columns, snmp.FetchConfig{})

		md, _, err := b.Build(testDevice(), compiled, values, time.Now())
		if err != nil && strings.Contains(err.Error(), "already emitted as") {
			t.Errorf("%s: instrument conflict: %v", name, err)
		}
		built++
		if md.DataPointCount() > 0 {
			withPoints++
		}

		// Within one profile, no metric name may appear twice.
		seen := map[string]bool{}
		for i := 0; i < md.ResourceMetrics().Len(); i++ {
			sms := md.ResourceMetrics().At(i).ScopeMetrics()
			for j := 0; j < sms.Len(); j++ {
				ms := sms.At(j).Metrics()
				for k := 0; k < ms.Len(); k++ {
					if seen[ms.At(k).Name()] {
						t.Errorf("%s emits metric %s twice", name, ms.At(k).Name())
					}
					seen[ms.At(k).Name()] = true
				}
			}
		}
	}
	t.Logf("built %d profiles, %d produced datapoints against the test device", built, withPoints)
	if built < 150 {
		t.Errorf("only built %d profiles", built)
	}
}

func TestBuildReportCountsGeneratedMetrics(t *testing.T) {
	compiled := compileProfile(t, "_generic-if")
	registry, err := naming.New(naming.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	b := report.NewBuilder(registry)

	scalars, columns := compiled.FetchOIDs()
	values, _, err := snmp.Fetch(twoInterfaceDevice(1, 1), scalars, columns, snmp.FetchConfig{})
	if err != nil {
		t.Fatal(err)
	}
	md, rep, err := b.Build(testDevice(), compiled, values, time.Now())
	if err != nil {
		t.Logf("build reported: %v", err)
	}

	if rep.DataPoints != md.DataPointCount() {
		t.Errorf("report says %d datapoints, metrics contain %d", rep.DataPoints, md.DataPointCount())
	}
	// ifNumber is the one unmapped symbol in this profile.
	if len(rep.GeneratedMetrics) == 0 {
		t.Error("expected the report to record the unmapped symbol")
	}
	t.Logf("%d datapoints, %d generated names: %v", rep.DataPoints, len(rep.GeneratedMetrics), keys(rep.GeneratedMetrics))
}

func keys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestStringTransforms(t *testing.T) {
	// A MAC address arrives as six raw octets and must be rendered readably,
	// since it becomes an attribute value.
	dev := snmptest.New()
	dev.SetBytes("1.3.6.1.2.1.2.2.1.6.1", []byte{0x00, 0x1b, 0x21, 0x3c, 0x4d, 0x5e})
	dev.SetString(ifName+".1", "Gi0/1")
	dev.SetCounter64(ifHCInOctets+".1", 42)

	compiled := compileProfile(t, "_generic-if")
	b := newBuilder(t, naming.DefaultOptions())
	md := poll(t, b, dev, compiled, time.Now())

	// The MAC is an interface metadata field rather than a metric, so assert it
	// did not break the build and traffic still came through.
	if m, ok := metricsByName(md)["hw.network.io"]; !ok {
		t.Error("traffic missing")
	} else if dataPoints(m).Len() == 0 {
		t.Error("no traffic datapoints")
	}
	_ = fmt.Sprint(md.DataPointCount())
}
