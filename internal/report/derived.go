package report

import (
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"

	"github.com/tsuga-dev/networkdevicereceiver/internal/naming"
	"github.com/tsuga-dev/networkdevicereceiver/internal/profiledefinition"
	"github.com/tsuga-dev/networkdevicereceiver/internal/snmp"
)

// IF-MIB OIDs needed to derive bandwidth utilisation.
const (
	oidIfHCInOctets  = "1.3.6.1.2.1.31.1.1.1.6"
	oidIfHCOutOctets = "1.3.6.1.2.1.31.1.1.1.10"
	oidIfInOctets    = "1.3.6.1.2.1.2.2.1.10"
	oidIfOutOctets   = "1.3.6.1.2.1.2.2.1.16"
	oidIfHighSpeed   = "1.3.6.1.2.1.31.1.1.1.15"
	oidIfSpeed       = "1.3.6.1.2.1.2.2.1.5"

	// utilizationMetric is a fraction, not a percentage: semconv gives
	// hw.network.bandwidth.utilization unit "1".
	utilizationMetric = "hw.network.bandwidth.utilization"
)

// emitBandwidthUtilization derives interface utilisation from octet counter
// deltas against the interface's speed.
//
// This stays in the receiver, unlike other rates, because it needs a join
// between two tables (traffic counters and ifHighSpeed) that a metrics backend
// cannot easily express. Nothing is emitted on the first poll, since a delta
// needs two observations.
func (b *Builder) emitBandwidthUtilization(pool *metricPool, resolver *tagResolver,
	store *snmp.ValueStore, def *profiledefinition.ProfileDefinition,
	ts pcommon.Timestamp, now time.Time, report *BuildReport) {

	speeds := interfaceSpeeds(store)
	if len(speeds) == 0 {
		return
	}

	directions := []struct {
		oids      []string
		direction string
	}{
		{[]string{oidIfHCInOctets, oidIfInOctets}, "receive"},
		{[]string{oidIfHCOutOctets, oidIfOutOctets}, "transmit"},
	}

	for _, dir := range directions {
		rows := firstAvailableColumn(store, dir.oids)
		for index, value := range rows {
			capacity, ok := speeds[index]
			if !ok || capacity <= 0 {
				continue
			}
			octets, err := value.Float()
			if err != nil {
				continue
			}

			key := dir.direction + "|" + index
			fraction, ok := b.utilization(key, octets, capacity, now)
			if !ok {
				continue
			}

			tags, _ := resolver.rowTags(interfaceNameTags(def), index)
			attrs := map[string]string{
				"hw.type":              "network",
				"network.io.direction": dir.direction,
				"hw.id":                "if_" + index,
			}
			if name := preferredName(tags); name != "" {
				attrs["hw.name"] = name
			}
			_ = pool.add(utilizationMetric, "1", naming.Gauge, attrs, fraction, ts, b, 0)
		}
	}
}

// utilization computes the fraction of capacity used since the previous poll,
// recording this poll's reading for next time.
func (b *Builder) utilization(key string, octets, capacityBytesPerSecond float64, now time.Time) (float64, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	previous, seen := b.prevCounters[key]
	b.prevCounters[key] = counterSample{value: octets, at: now}
	if !seen {
		return 0, false
	}

	elapsed := now.Sub(previous.at).Seconds()
	if elapsed <= 0 {
		return 0, false
	}
	// A counter that went backwards means the device reset or the counter
	// wrapped; deriving a rate from that would produce a nonsense spike.
	if octets < previous.value {
		return 0, false
	}

	bytesPerSecond := (octets - previous.value) / elapsed
	fraction := bytesPerSecond / capacityBytesPerSecond
	if fraction < 0 {
		return 0, false
	}
	return fraction, true
}

// interfaceSpeeds returns each interface's capacity in bytes per second,
// preferring ifHighSpeed because ifSpeed saturates at 4.29 Gbit/s.
func interfaceSpeeds(store *snmp.ValueStore) map[string]float64 {
	out := map[string]float64{}

	if rows, err := store.Column(oidIfSpeed); err == nil {
		for index, value := range rows {
			if bits, err := value.Float(); err == nil && bits > 0 {
				out[index] = bits / 8
			}
		}
	}
	// ifHighSpeed is in Mbit/s and overwrites the ifSpeed estimate.
	if rows, err := store.Column(oidIfHighSpeed); err == nil {
		for index, value := range rows {
			if mbits, err := value.Float(); err == nil && mbits > 0 {
				out[index] = mbits * 125000
			}
		}
	}
	return out
}

// firstAvailableColumn returns the first of several equivalent columns that the
// device actually answered, so a 64-bit counter is preferred over its 32-bit
// counterpart.
func firstAvailableColumn(store *snmp.ValueStore, oids []string) map[string]snmp.ResultValue {
	for _, oid := range oids {
		if rows, err := store.Column(oid); err == nil && len(rows) > 0 {
			return rows
		}
	}
	return nil
}

// interfaceNameTags finds the profile's interface-naming tags, so a derived
// metric carries the same hw.name as the collected ones.
func interfaceNameTags(def *profiledefinition.ProfileDefinition) profiledefinition.MetricTagConfigList {
	for _, m := range def.Metrics {
		if m.Table.OID != "1.3.6.1.2.1.2.2" {
			continue
		}
		for _, tag := range m.MetricTags {
			if tag.Tag == "interface" {
				return profiledefinition.MetricTagConfigList{tag}
			}
		}
	}
	return nil
}
