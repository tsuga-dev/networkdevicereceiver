package report

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"

	"github.com/tsuga-dev/networkdevicereceiver/internal/naming"
	"github.com/tsuga-dev/networkdevicereceiver/internal/profile"
	"github.com/tsuga-dev/networkdevicereceiver/internal/profiledefinition"
	"github.com/tsuga-dev/networkdevicereceiver/internal/snmp"
)

// ScopeName identifies this receiver as the instrumentation scope.
const ScopeName = "github.com/tsuga-dev/networkdevicereceiver"

// DeviceInfo is what the discovery layer knows about a device, and becomes the
// resource the metrics are reported for.
type DeviceInfo struct {
	// ID is a stable identifier for the device across polls and restarts.
	ID string
	// Address is the device's IP.
	Address string
	Port    uint16
	// Subnet is the configured CIDR the device was discovered in, empty for a
	// statically configured device.
	Subnet      string
	SysObjectID string
	ProfileName string
}

// BuildReport summarises one build, for self-telemetry and for reporting how
// much of a profile is still unmapped.
type BuildReport struct {
	DataPoints int
	// GeneratedMetrics counts distinct metric names that fell back to the
	// generated namespace, i.e. the size of the curation gap for this device.
	GeneratedMetrics map[string]struct{}
	// SkippedSymbols counts symbols that produced nothing.
	SkippedSymbols int
}

// Builder converts poll results to OTLP metrics for one device. It is stateful:
// cumulative streams need a start timestamp, and bandwidth utilisation needs the
// previous poll's counters.
type Builder struct {
	registry *naming.Registry

	mu sync.Mutex
	// startTimes records when each cumulative stream was first observed.
	startTimes map[string]pcommon.Timestamp
	// prevCounters holds the last reading per interface counter stream, for
	// deriving bandwidth utilisation.
	prevCounters map[string]counterSample
}

type counterSample struct {
	value float64
	at    time.Time
}

// NewBuilder returns a builder for one device.
func NewBuilder(registry *naming.Registry) *Builder {
	return &Builder{
		registry:     registry,
		startTimes:   map[string]pcommon.Timestamp{},
		prevCounters: map[string]counterSample{},
	}
}

// Build produces the metrics for one poll.
//
// Errors are collected rather than fatal: SNMP polls are routinely partial, and
// a device that answers ninety per cent of its OIDs should report ninety per
// cent of its metrics.
func (b *Builder) Build(dev DeviceInfo, compiled *profile.Compiled, store *snmp.ValueStore, now time.Time) (pmetric.Metrics, *BuildReport, error) {
	def := compiled.Definition
	resolver := &tagResolver{compiled: compiled, store: store}
	report := &BuildReport{GeneratedMetrics: map[string]struct{}{}}
	var errs []error

	md := pmetric.NewMetrics()
	rm := md.ResourceMetrics().AppendEmpty()

	deviceTags, err := resolver.deviceTags(def.MetricTags)
	if err != nil {
		errs = append(errs, fmt.Errorf("device tags: %w", err))
	}
	b.setResourceAttributes(rm.Resource(), dev, def, store, deviceTags)

	sm := rm.ScopeMetrics().AppendEmpty()
	sm.Scope().SetName(ScopeName)
	pool := newMetricPool(sm.Metrics())

	profileStatic := staticTags(def.StaticTags)
	ts := pcommon.NewTimestampFromTime(now)

	for i := range def.Metrics {
		m := &def.Metrics[i]
		if err := b.emitMetric(pool, resolver, store, m, profileStatic, ts, now, report); err != nil {
			errs = append(errs, fmt.Errorf("metrics[%d] (%s): %w", i, metricLabel(m), err))
		}
	}

	b.emitBandwidthUtilization(pool, resolver, store, def, ts, now, report)

	report.DataPoints = countDataPoints(md)
	return md, report, errors.Join(errs...)
}

func metricLabel(m *profiledefinition.MetricsConfig) string {
	if m.Symbol.Name != "" {
		return m.Symbol.Name
	}
	if m.Table.Name != "" {
		return m.Table.Name
	}
	return m.MIB
}

// setResourceAttributes puts device identity on the resource. The hardware
// semantic conventions require this: hw.* metrics must not carry attributes
// identifying the device they came from.
func (b *Builder) setResourceAttributes(res pcommon.Resource, dev DeviceInfo,
	def *profiledefinition.ProfileDefinition, store *snmp.ValueStore, deviceTags map[string]string) {

	attrs := res.Attributes()
	attrs.PutStr("host.id", dev.ID)
	attrs.PutStr("snmp.device.ip", dev.Address)
	if dev.SysObjectID != "" {
		attrs.PutStr("snmp.device.sys_object_id", dev.SysObjectID)
	}
	if dev.ProfileName != "" {
		attrs.PutStr("snmp.profile", dev.ProfileName)
	}
	if dev.Subnet != "" {
		attrs.PutStr("snmp.subnet", dev.Subnet)
	}

	// Device inventory fields become resource attributes, mapped onto semconv
	// names where one exists.
	for field, value := range b.deviceMetadata(def, store) {
		switch field {
		case "name", "sys_name":
			attrs.PutStr("host.name", value)
		case "vendor":
			attrs.PutStr("device.manufacturer", value)
		case "model":
			attrs.PutStr("device.model.identifier", value)
		case "os_name":
			attrs.PutStr("os.name", value)
		case "os_version", "version":
			attrs.PutStr("os.version", value)
		case "serial_number":
			attrs.PutStr("snmp.device.serial_number", value)
		default:
			attrs.PutStr("snmp.device."+field, value)
		}
	}

	// Profile-level tags identify the device (snmp_host from sysName, for
	// example), so they belong here rather than on datapoints.
	for k, v := range deviceTags {
		if _, exists := attrs.Get(k); !exists {
			attrs.PutStr(k, v)
		}
	}
	if _, ok := attrs.Get("host.name"); !ok {
		// Fall back to the address so every resource is identifiable.
		attrs.PutStr("host.name", dev.Address)
	}
}

// deviceMetadata resolves the profile's device inventory fields.
func (b *Builder) deviceMetadata(def *profiledefinition.ProfileDefinition, store *snmp.ValueStore) map[string]string {
	res, ok := def.Metadata["device"]
	if !ok {
		return nil
	}
	out := make(map[string]string, len(res.Fields))
	for name, field := range res.Fields {
		if field.Value != "" {
			out[name] = field.Value
			continue
		}
		candidates := field.Symbols
		if field.Symbol.OID != "" {
			candidates = append([]profiledefinition.SymbolConfig{field.Symbol}, candidates...)
		}
		// The first candidate that yields a value wins, which is how profiles
		// try a vendor-specific OID before falling back to sysDescr.
		for _, sym := range candidates {
			value, err := store.Scalar(sym.OID)
			if err != nil {
				continue
			}
			text, err := stringValueNoCompile(sym, value)
			if err != nil || text == "" {
				continue
			}
			out[name] = text
			break
		}
	}
	return out
}

// emitMetric handles one profile metric entry, scalar or table.
func (b *Builder) emitMetric(pool *metricPool, resolver *tagResolver, store *snmp.ValueStore,
	m *profiledefinition.MetricsConfig, profileStatic map[string]string,
	ts pcommon.Timestamp, now time.Time, report *BuildReport) error {

	metricStatic := mergeTags(mergeTags(nil, profileStatic), staticTags(m.StaticTags))

	if m.IsColumn() {
		return b.emitTableMetric(pool, resolver, store, m, metricStatic, ts, now, report)
	}
	return b.emitScalarMetric(pool, resolver, store, m, metricStatic, ts, report)
}

func (b *Builder) emitScalarMetric(pool *metricPool, resolver *tagResolver, store *snmp.ValueStore,
	m *profiledefinition.MetricsConfig, static map[string]string,
	ts pcommon.Timestamp, report *BuildReport) error {

	value, err := store.Scalar(m.Symbol.OID)
	if err != nil {
		report.SkippedSymbols++
		// An absent scalar is normal; the OID gets pruned by the caller.
		return nil //nolint:nilerr // absence is not a failure
	}

	tags, tagErr := resolver.deviceTags(m.MetricTags)
	attrs := mergeTags(mergeTags(nil, static), tags)

	err = b.emitSymbol(pool, store, m, m.Symbol, value, attrs, "", "", ts, report)
	return errors.Join(tagErr, err)
}

func (b *Builder) emitTableMetric(pool *metricPool, resolver *tagResolver, store *snmp.ValueStore,
	m *profiledefinition.MetricsConfig, static map[string]string,
	ts pcommon.Timestamp, now time.Time, report *BuildReport) error {

	var errs []error
	component := componentPrefix(m.Table.Name, m.Table.OID)

	for _, sym := range m.Symbols {
		rows, err := b.rowsFor(store, m, sym)
		if err != nil {
			report.SkippedSymbols++
			continue
		}
		for _, index := range sortedIndexes(rows) {
			tags, tagErr := resolver.rowTags(m.MetricTags, index)
			if tagErr != nil {
				errs = append(errs, tagErr)
			}
			attrs := mergeTags(mergeTags(nil, static), tags)

			value := rows[index]
			if err := b.emitSymbol(pool, store, m, sym, value, attrs, component, index, ts, report); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// rowsFor returns the rows to emit for a symbol. A constant_value_one symbol has
// no column of its own, so it borrows the row set of a sibling that does --
// otherwise the entity count it represents would never be reported.
func (b *Builder) rowsFor(store *snmp.ValueStore, m *profiledefinition.MetricsConfig,
	sym profiledefinition.SymbolConfig) (map[string]snmp.ResultValue, error) {

	if sym.OID != "" {
		return store.Column(sym.OID)
	}
	if !sym.ConstantValueOne {
		return nil, fmt.Errorf("symbol %q has no OID", sym.Name)
	}
	for _, sibling := range m.Symbols {
		if sibling.OID == "" {
			continue
		}
		if rows, err := store.Column(sibling.OID); err == nil && len(rows) > 0 {
			return rows, nil
		}
	}
	// Fall back to any tag column of the same table.
	for _, tag := range m.MetricTags {
		if tag.Symbol.OID == "" || tag.Table != "" {
			continue
		}
		if rows, err := store.Column(tag.Symbol.OID); err == nil && len(rows) > 0 {
			return rows, nil
		}
	}
	return nil, fmt.Errorf("no sibling column to enumerate rows for %q", sym.Name)
}

// emitSymbol resolves one symbol through the naming registry and appends its
// datapoints.
func (b *Builder) emitSymbol(pool *metricPool, store *snmp.ValueStore, m *profiledefinition.MetricsConfig,
	sym profiledefinition.SymbolConfig, value snmp.ResultValue, attrs map[string]string,
	component, index string, ts pcommon.Timestamp, report *BuildReport) error {

	// flag_stream produces one metric per bit, distinguished by suffix, so the
	// suffix is part of the name the registry resolves.
	lookupName := sym.Name
	if m.MetricType == profiledefinition.MetricTypeFlagStream {
		lookupName += m.Options.MetricSuffix
	}

	resolution := b.registry.Resolve(m.MIB, profiledefinition.SymbolConfig{Name: lookupName})
	entry := resolution.Entry

	// A sensor column's meaning comes from a sibling column in the same row.
	if entry.TypeDispatch != nil {
		dispatched, err := b.dispatchSensor(entry.TypeDispatch, store, m, index, report)
		if err != nil {
			return err
		}
		entry = dispatched
		resolution.Entry = dispatched
	}

	number, err := b.numericValue(m, sym, value, entry)
	if err != nil {
		report.SkippedSymbols++
		return fmt.Errorf("symbol %s: %w", sym.Name, err)
	}

	if resolution.Generated {
		for _, name := range resolution.Names {
			report.GeneratedMetrics[name] = struct{}{}
		}
	}

	instrument := entry.Instrument
	if resolution.Generated {
		// Only a curated entry is authoritative about the instrument; otherwise
		// infer it from the profile and the SNMP type.
		instrument = instrumentFor(effectiveMetricType(m, sym), value)
	}

	for _, name := range resolution.Names {
		dpAttrs := buildAttributes(entry, attrs, name, component, index, sym.Name)

		if entry.StateSet != nil {
			if err := b.emitStateSet(pool, name, entry, value, dpAttrs, ts); err != nil {
				return err
			}
			continue
		}
		if err := pool.add(name, entry.Unit, instrument, dpAttrs, number, ts, b, entry.Priority); err != nil {
			return err
		}
	}
	return nil
}

// emitStateSet writes one datapoint per possible state: 1 for the active state
// and 0 for the rest, which is the StateSet representation hw.status uses.
//
// An SNMP value outside the mapping yields all-zero rather than no datapoints,
// so a device in an unexpected state reads as "no known state active" instead of
// silently dropping out of the series.
func (b *Builder) emitStateSet(pool *metricPool, name string, entry naming.Entry,
	value snmp.ResultValue, attrs map[string]string, ts pcommon.Timestamp) error {

	active := entry.StateSet.Map[value.String()]
	for _, state := range entry.StateSet.States {
		stateAttrs := make(map[string]string, len(attrs)+1)
		for k, v := range attrs {
			stateAttrs[k] = v
		}
		stateAttrs[entry.StateSet.Attribute] = state

		reading := 0.0
		if state == active {
			reading = 1
		}
		if err := pool.add(name, entry.Unit, entry.Instrument, stateAttrs, reading, ts, b, entry.Priority); err != nil {
			return err
		}
	}
	return nil
}

// dispatchSensor follows a type_dispatch to the entry for this row's sensor kind.
func (b *Builder) dispatchSensor(td *naming.TypeDispatch, store *snmp.ValueStore,
	m *profiledefinition.MetricsConfig, index string, report *BuildReport) (naming.Entry, error) {

	// The sibling type column is found by name among the metric's tags or
	// symbols, since the profile does not otherwise link them.
	typeValue, ok := siblingValue(store, td.TypeSymbol, m, index)
	if !ok {
		report.SkippedSymbols++
		return naming.Entry{}, fmt.Errorf("sensor type column %q not collected for row %s", td.TypeSymbol, index)
	}
	target, ok := td.Cases[typeValue]
	if !ok {
		report.SkippedSymbols++
		return naming.Entry{}, fmt.Errorf("sensor type %q has no mapping", typeValue)
	}
	entry, ok := b.registry.EntryByName(target)
	if !ok {
		return naming.Entry{}, fmt.Errorf("type_dispatch target %q is not a registry entry", target)
	}
	return entry, nil
}

// numericValue produces the number to report for a symbol.
func (b *Builder) numericValue(m *profiledefinition.MetricsConfig, sym profiledefinition.SymbolConfig,
	value snmp.ResultValue, entry naming.Entry) (float64, error) {

	if m.MetricType == profiledefinition.MetricTypeFlagStream {
		return flagValue(value, m.Options.Placement)
	}
	// A value map turns an enum into a number, as ifOperStatus up(1) becomes 1
	// and everything else 0.
	if len(entry.ValueMap) > 0 {
		if mapped, ok := entry.ValueMap[value.String()]; ok {
			return mapped, nil
		}
		if fallback, ok := entry.ValueMap["default"]; ok {
			return fallback, nil
		}
	}
	return floatValue(sym, value, entry.Scale)
}

// effectiveMetricType resolves the metric type, a symbol overriding its metric.
func effectiveMetricType(m *profiledefinition.MetricsConfig, sym profiledefinition.SymbolConfig) profiledefinition.ProfileMetricType {
	if sym.MetricType != "" {
		return sym.MetricType
	}
	return m.MetricType
}

// instrumentFor picks an instrument when the registry has no opinion.
func instrumentFor(metricType profiledefinition.ProfileMetricType, value snmp.ResultValue) naming.Instrument {
	switch metricType {
	case profiledefinition.MetricTypeMonotonicCount,
		profiledefinition.MetricTypeMonotonicCountAndRate,
		profiledefinition.MetricTypeRate:
		// Rates are not computed here: the cumulative total is emitted and the
		// pipeline derives a rate, so monotonic_count_and_rate collapses to one
		// Sum rather than two streams.
		return naming.Sum
	case profiledefinition.MetricTypeGauge, profiledefinition.MetricTypeFlagStream:
		return naming.Gauge
	default:
		// No declared type: trust the device. A counter is cumulative.
		if value.IsCounter() {
			return naming.Sum
		}
		return naming.Gauge
	}
}

// buildAttributes assembles the datapoint attributes: registry-declared ones,
// the resolved profile tags, and the hardware component identity.
func buildAttributes(entry naming.Entry, tags map[string]string,
	metricName, component, index, symbolName string) map[string]string {

	out := make(map[string]string, len(entry.Attributes)+len(tags)+2)
	for k, v := range entry.Attributes {
		out[k] = v
	}
	for k, v := range tags {
		out[k] = v
	}

	// hw.id is required on hw.* metrics and must be unique within the device.
	// Other namespaces must not carry it, so the attribute keeps its meaning.
	if !strings.HasPrefix(metricName, "hw.") {
		return out
	}
	if id := componentID(component, index, symbolName); id != "" {
		out["hw.id"] = id
	}
	if name := preferredName(tags); name != "" {
		out["hw.name"] = name
	}
	return out
}

// siblingValue reads another column of the same row, by symbol name. Used to
// resolve a sensor's kind from the column that declares it.
func siblingValue(store *snmp.ValueStore, symbolName string,
	m *profiledefinition.MetricsConfig, index string) (string, bool) {

	lookup := func(sym profiledefinition.SymbolConfig, transforms []profiledefinition.MetricIndexTransform) (string, bool) {
		if sym.Name != symbolName || sym.OID == "" {
			return "", false
		}
		target, err := transformIndex(index, transforms)
		if err != nil {
			return "", false
		}
		value, err := store.ColumnValue(sym.OID, target)
		if err != nil {
			return "", false
		}
		return value.String(), true
	}

	for _, sym := range m.Symbols {
		if v, ok := lookup(sym, nil); ok {
			return v, true
		}
	}
	for _, tag := range m.MetricTags {
		if v, ok := lookup(tag.Symbol, tag.IndexTransform); ok {
			return v, true
		}
	}
	return "", false
}

// componentID builds a per-device unique component identifier: the table's short
// name plus the row index, so interface 1 of ifTable becomes "if_1".
func componentID(component, index, symbolName string) string {
	switch {
	case component != "" && index != "":
		return component + "_" + index
	case symbolName != "":
		return toSnake(symbolName)
	default:
		return ""
	}
}

// preferredName picks a human-readable component name from the row's tags.
func preferredName(tags map[string]string) string {
	for _, key := range []string{"interface", "name", "sensor", "port"} {
		if v, ok := tags[key]; ok && v != "" {
			return v
		}
	}
	return ""
}

// canonicalComponents maps tables that describe the same physical components to
// one component namespace, keyed by table OID.
//
// ifTable and ifXTable are both indexed by ifIndex and describe the same
// interfaces, so metrics from either must agree on hw.id -- otherwise interface
// 1 appears as both "if_1" and "if_x_1" and no consumer can join them.
var canonicalComponents = map[string]string{
	"1.3.6.1.2.1.2.2":    "if", // ifTable
	"1.3.6.1.2.1.31.1.1": "if", // ifXTable
}

// componentPrefix derives a short component namespace from a table name:
// ifTable becomes "if", entPhySensorTable becomes "ent_phy_sensor".
func componentPrefix(tableName, tableOID string) string {
	if canonical, ok := canonicalComponents[snmp.CanonicalOID(tableOID)]; ok {
		return canonical
	}
	if tableName == "" {
		return sanitizeIdent(tableOID)
	}
	trimmed := strings.TrimSuffix(tableName, "Table")
	if trimmed == "" {
		trimmed = tableName
	}
	return toSnake(trimmed)
}

func sortedIndexes(rows map[string]snmp.ResultValue) []string {
	out := make([]string, 0, len(rows))
	for index := range rows {
		out = append(out, index)
	}
	sort.Slice(out, func(i, j int) bool { return snmp.OIDLess(out[i], out[j]) })
	return out
}

func countDataPoints(md pmetric.Metrics) int {
	return md.DataPointCount()
}

// streamKeyFromMap identifies one metric stream, so a cumulative series keeps a
// stable start timestamp across polls and duplicate streams can be detected.
func streamKeyFromMap(name string, attrs map[string]string) string {
	pairs := make([]string, 0, len(attrs))
	for k, v := range attrs {
		pairs = append(pairs, k+"="+v)
	}
	sort.Strings(pairs)
	return name + "|" + strings.Join(pairs, ",")
}

// startTime returns the start timestamp for a stream, recording the first
// observation. A cumulative point without a start timestamp cannot be turned
// into a rate by a backend that did not see the earlier points.
func (b *Builder) startTime(key string, now pcommon.Timestamp) pcommon.Timestamp {
	b.mu.Lock()
	defer b.mu.Unlock()
	if start, ok := b.startTimes[key]; ok {
		return start
	}
	b.startTimes[key] = now
	return now
}

func toSnake(s string) string {
	var out strings.Builder
	runes := []rune(s)
	for i, r := range runes {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				prev := runes[i-1]
				endOfAcronym := prev >= 'A' && prev <= 'Z' && i+1 < len(runes) && runes[i+1] >= 'a' && runes[i+1] <= 'z'
				if (prev >= 'a' && prev <= 'z') || (prev >= '0' && prev <= '9') || endOfAcronym {
					out.WriteByte('_')
				}
			}
			out.WriteRune(r + 32)
			continue
		}
		out.WriteRune(r)
	}
	return sanitizeIdent(out.String())
}

func sanitizeIdent(s string) string {
	var out strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			out.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			out.WriteRune(r + 32)
		default:
			out.WriteRune('_')
		}
	}
	return strings.Trim(out.String(), "_")
}

// stringValueNoCompile shapes a metadata value without a compiled profile. Used
// for inventory fields, whose regexes are compiled with the profile but which
// may also be plain.
func stringValueNoCompile(sym profiledefinition.SymbolConfig, value snmp.ResultValue) (string, error) {
	if sym.Format != "" {
		return applyFormat(sym.Format, value)
	}
	return value.String(), nil
}
