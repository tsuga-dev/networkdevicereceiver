package report

import (
	"fmt"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"

	"github.com/tsuga-dev/networkdevicereceiver/internal/naming"
)

// metricPool creates each metric once and appends datapoints to it.
//
// Several symbols share one metric name -- ifHCInOctets and ifHCOutOctets are
// both hw.network.io, distinguished only by network.io.direction -- so emitting
// a fresh Metric per symbol would produce duplicate metric names within one
// scope, which OTLP consumers are entitled to reject.
type metricPool struct {
	metrics pmetric.MetricSlice
	byName  map[string]pmetric.Metric
	kinds   map[string]naming.Instrument
	// streams deduplicates datapoints that share a metric name and attribute
	// set, keeping the highest-priority contributor.
	streams map[string]stream
}

// stream is one emitted datapoint, retained so a higher-priority symbol can
// replace its value rather than adding a conflicting duplicate.
type stream struct {
	point    pmetric.NumberDataPoint
	priority int
}

func newMetricPool(metrics pmetric.MetricSlice) *metricPool {
	return &metricPool{
		metrics: metrics,
		byName:  map[string]pmetric.Metric{},
		kinds:   map[string]naming.Instrument{},
		streams: map[string]stream{},
	}
}

// metric returns the metric for a name, creating it on first use.
func (p *metricPool) metric(name, unit string, instrument naming.Instrument) (pmetric.Metric, error) {
	if existing, ok := p.byName[name]; ok {
		// A name must keep one instrument kind. Two symbols disagreeing means a
		// registry mistake, and silently mixing them would corrupt the series.
		if p.kinds[name] != instrument {
			return pmetric.Metric{}, fmt.Errorf(
				"metric %s requested as %s but already emitted as %s", name, instrument, p.kinds[name])
		}
		return existing, nil
	}

	m := p.metrics.AppendEmpty()
	m.SetName(name)
	m.SetUnit(unit)

	switch instrument {
	case naming.Gauge:
		m.SetEmptyGauge()
	case naming.Sum:
		sum := m.SetEmptySum()
		sum.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
		sum.SetIsMonotonic(true)
	case naming.UpDownCounter:
		sum := m.SetEmptySum()
		sum.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
		sum.SetIsMonotonic(false)
	default:
		return pmetric.Metric{}, fmt.Errorf("unknown instrument %q for metric %s", instrument, name)
	}

	p.byName[name] = m
	p.kinds[name] = instrument
	return m, nil
}

// add appends one datapoint. Attributes are supplied up front because a
// cumulative stream's start timestamp is keyed on them.
//
// If a datapoint already exists for this metric and attribute set, priority
// decides: a higher-priority symbol overwrites the value, a lower or equal one
// is dropped. Appending regardless would emit conflicting duplicate series.
func (p *metricPool) add(name, unit string, instrument naming.Instrument,
	attrs map[string]string, value float64, ts pcommon.Timestamp, b *Builder,
	priority int) error {

	m, err := p.metric(name, unit, instrument)
	if err != nil {
		return err
	}

	key := streamKeyFromMap(name, attrs)
	if existing, ok := p.streams[key]; ok {
		if priority > existing.priority {
			existing.point.SetDoubleValue(value)
			p.streams[key] = stream{point: existing.point, priority: priority}
		}
		return nil
	}

	var points pmetric.NumberDataPointSlice
	switch instrument {
	case naming.Gauge:
		points = m.Gauge().DataPoints()
	default:
		points = m.Sum().DataPoints()
	}

	dp := points.AppendEmpty()
	dp.SetTimestamp(ts)
	for k, v := range attrs {
		dp.Attributes().PutStr(k, v)
	}
	dp.SetDoubleValue(value)

	if instrument != naming.Gauge {
		dp.SetStartTimestamp(b.startTime(key, ts))
	}

	p.streams[key] = stream{point: dp, priority: priority}
	return nil
}
