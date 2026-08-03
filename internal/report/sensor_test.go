package report_test

import (
	"testing"
	"time"

	"github.com/tsuga-dev/networkdevicereceiver/internal/naming"
	"github.com/tsuga-dev/networkdevicereceiver/internal/profile"
	"github.com/tsuga-dev/networkdevicereceiver/internal/report"
	"github.com/tsuga-dev/networkdevicereceiver/internal/snmp"
	"github.com/tsuga-dev/networkdevicereceiver/internal/snmptest"
)

// ENTITY-SENSOR-MIB columns.
const (
	entPhySensorType      = "1.3.6.1.2.1.99.1.1.1.1"
	entPhySensorScale     = "1.3.6.1.2.1.99.1.1.1.2"
	entPhySensorPrecision = "1.3.6.1.2.1.99.1.1.1.3"
	entPhySensorValue     = "1.3.6.1.2.1.99.1.1.1.4"
)

// EntitySensorDataType and EntitySensorDataScale enum values.
const (
	typeVoltsDC = 4
	typeWatts   = 6
	typeCelsius = 8
	typeRPM     = 10

	scaleUnits = 9
	scaleMilli = 8
	scaleKilo  = 10
)

// sensorRow is one row of entPhySensorTable.
type sensorRow struct {
	index                   string
	sensorType, scale, prec int
	raw                     int
}

func sensorDevice(rows ...sensorRow) *snmptest.Fake {
	dev := snmptest.New()
	for _, r := range rows {
		dev.SetInt(entPhySensorType+"."+r.index, r.sensorType)
		dev.SetInt(entPhySensorScale+"."+r.index, r.scale)
		dev.SetInt(entPhySensorPrecision+"."+r.index, r.prec)
		dev.SetInt(entPhySensorValue+"."+r.index, r.raw)
	}
	return dev
}

func buildSensors(t *testing.T, dev snmp.Session) map[string]pointsByID {
	t.Helper()
	def, err := profile.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := def.Resolve("_generic-entity-sensor")
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := profile.Compile(resolved)
	if err != nil {
		t.Fatal(err)
	}

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
	md, _, buildErr := builder.Build(report.DeviceInfo{
		ID: "10.0.0.1", Address: "10.0.0.1", ProfileName: "_generic-entity-sensor",
	}, compiled, values, time.Now())
	if buildErr != nil {
		t.Logf("build reported: %v", buildErr)
	}

	out := map[string]pointsByID{}
	for name, m := range metricsByName(md) {
		byID := pointsByID{}
		points := dataPoints(m)
		for i := 0; i < points.Len(); i++ {
			dp := points.At(i)
			byID[attrsOf(dp)["hw.id"]] = dp.DoubleValue()
		}
		out[name] = byID
	}
	return out
}

type pointsByID map[string]float64

// TestSensorDispatchAndExponent covers the case a hand-written pipeline has to
// special-case per device: one sensor table holding several kinds of sensor, each
// with its own exponent.
//
// The demo NDM collector needs an OTTL divisor scoped by host.name for the
// deci-celsius switch, with a comment noting it must be revisited when another
// such device appears. Reading the device's own scale and precision removes that.
func TestSensorDispatchAndExponent(t *testing.T) {
	metrics := buildSensors(t, sensorDevice(
		// A switch reporting deci-celsius: 425 with precision 1 is 42.5 C.
		sensorRow{index: "1", sensorType: typeCelsius, scale: scaleUnits, prec: 1, raw: 425},
		// A firewall reporting whole degrees in the same table shape: 39 C.
		sensorRow{index: "2", sensorType: typeCelsius, scale: scaleUnits, prec: 0, raw: 39},
		// A PSU rail: 334 with precision 2 is 3.34 V.
		sensorRow{index: "3", sensorType: typeVoltsDC, scale: scaleUnits, prec: 2, raw: 334},
		// A fan at 8700 rpm, no scaling.
		sensorRow{index: "4", sensorType: typeRPM, scale: scaleUnits, prec: 0, raw: 8700},
		// Power reported in milliwatts: 1500 mW is 1.5 W.
		sensorRow{index: "5", sensorType: typeWatts, scale: scaleMilli, prec: 0, raw: 1500},
		// Power reported in kilowatts: 2 kW is 2000 W.
		sensorRow{index: "6", sensorType: typeWatts, scale: scaleKilo, prec: 0, raw: 2},
	))

	tests := []struct {
		metric, id string
		want       float64
	}{
		{"hw.temperature", "ent_phy_sensor_1", 42.5},
		{"hw.temperature", "ent_phy_sensor_2", 39},
		{"hw.voltage", "ent_phy_sensor_3", 3.34},
		{"hw.fan.speed", "ent_phy_sensor_4", 8700},
		{"hw.power", "ent_phy_sensor_5", 1.5},
		{"hw.power", "ent_phy_sensor_6", 2000},
	}
	for _, tc := range tests {
		byID, ok := metrics[tc.metric]
		if !ok {
			t.Errorf("%s was not emitted; got %v", tc.metric, keysOfFloat(metrics))
			continue
		}
		got, ok := byID[tc.id]
		if !ok {
			t.Errorf("%s has no datapoint for %s; got %v", tc.metric, tc.id, byID)
			continue
		}
		if !closeEnough(got, tc.want) {
			t.Errorf("%s[%s] = %v, want %v", tc.metric, tc.id, got, tc.want)
		}
	}

	// A single sensor table must not collapse into one metric: the whole point of
	// dispatch is that each row goes to the metric its type implies.
	for _, unwanted := range []string{"snmp.entity_sensor.ent_phy_sensor_value"} {
		if _, present := metrics[unwanted]; present {
			t.Errorf("%s emitted; sensor rows should have dispatched by type", unwanted)
		}
	}
}

// TestSensorWithoutScaleColumnsIsUnscaled checks a device that omits the scale
// and precision columns still reports, at face value rather than not at all.
func TestSensorWithoutScaleColumnsIsUnscaled(t *testing.T) {
	dev := snmptest.New()
	dev.SetInt(entPhySensorType+".1", typeCelsius)
	dev.SetInt(entPhySensorValue+".1", 41)

	metrics := buildSensors(t, dev)
	byID, ok := metrics["hw.temperature"]
	if !ok {
		t.Fatalf("hw.temperature not emitted; got %v", keysOfFloat(metrics))
	}
	if got := byID["ent_phy_sensor_1"]; !closeEnough(got, 41) {
		t.Errorf("value = %v, want 41 with no exponent applied", got)
	}
}

// TestSensorUnmappedTypeIsSkipped covers a sensor kind with no hw.* home, such as
// percentRH: it must not be mis-filed under another metric.
func TestSensorUnmappedTypeIsSkipped(t *testing.T) {
	const typePercentRH = 9
	metrics := buildSensors(t, sensorDevice(
		sensorRow{index: "1", sensorType: typeCelsius, scale: scaleUnits, prec: 0, raw: 30},
		sensorRow{index: "2", sensorType: typePercentRH, scale: scaleUnits, prec: 0, raw: 55},
	))

	temps, ok := metrics["hw.temperature"]
	if !ok {
		t.Fatal("hw.temperature not emitted")
	}
	if _, present := temps["ent_phy_sensor_2"]; present {
		t.Error("a humidity sensor was reported as a temperature")
	}
	if got := temps["ent_phy_sensor_1"]; !closeEnough(got, 30) {
		t.Errorf("temperature = %v, want 30", got)
	}
}

func TestSensorExponentUnitsAreCorrect(t *testing.T) {
	// Verify the SI-prefix arithmetic directly across the enum's range.
	cases := []struct {
		scale, prec int
		raw         float64
		want        float64
	}{
		{scaleUnits, 0, 100, 100},
		{scaleUnits, 1, 100, 10},
		{scaleUnits, 2, 100, 1},
		{scaleMilli, 0, 100, 0.1},
		{scaleKilo, 0, 100, 100000},
		// micro(7) is 10^-6.
		{7, 0, 1_000_000, 1},
	}
	for _, tc := range cases {
		metrics := buildSensors(t, sensorDevice(sensorRow{
			index: "1", sensorType: typeWatts, scale: tc.scale, prec: tc.prec, raw: int(tc.raw),
		}))
		got := metrics["hw.power"]["ent_phy_sensor_1"]
		if !closeEnough(got, tc.want) {
			t.Errorf("scale=%d precision=%d raw=%v -> %v, want %v", tc.scale, tc.prec, tc.raw, got, tc.want)
		}
	}
}

func closeEnough(got, want float64) bool {
	diff := got - want
	if diff < 0 {
		diff = -diff
	}
	tolerance := 1e-9
	if want != 0 {
		tolerance = 1e-9 + want*1e-9
		if tolerance < 0 {
			tolerance = -tolerance
		}
	}
	return diff <= tolerance
}

func keysOfFloat(m map[string]pointsByID) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
