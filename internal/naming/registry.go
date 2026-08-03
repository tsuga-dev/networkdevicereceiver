// Package naming maps SNMP profile symbols onto OpenTelemetry metric names.
//
// This is deliberately separate from profile parsing: profiles stay in Datadog's
// format so the upstream library and users' own profiles keep working, while
// output naming evolves independently here.
//
// Three tiers, per the design:
//
//	Tier 1  hw.*      the hardware semantic conventions, preferred
//	Tier 2  system.*  device-OS metrics (cpu/memory), opt-in
//	Tier 3  snmp.*    deterministic fallback for unmodelled vendor symbols
//
// Tier 1 entries are curated data in registry.yaml. Tier 3 names are generated,
// and are stable because they derive only from the MIB and symbol names.
package naming

import (
	"embed"
	"fmt"

	"go.yaml.in/yaml/v3"

	"github.com/tsuga-dev/networkdevicereceiver/internal/profiledefinition"
)

//go:embed registry.yaml
var registryData embed.FS

// Instrument is the OTel instrument kind to emit.
type Instrument string

const (
	// Gauge is a point-in-time reading.
	Gauge Instrument = "gauge"
	// Sum is a cumulative monotonic counter.
	Sum Instrument = "sum"
	// UpDownCounter is a cumulative non-monotonic sum.
	UpDownCounter Instrument = "updowncounter"
)

// StateSet describes an OpenMetrics-style state set. hw.status must emit one
// datapoint per possible state with value 1 for the active state and 0 for the
// others, so an SNMP enum becomes N datapoints rather than one enum-valued gauge.
type StateSet struct {
	// Attribute is the attribute carrying the state, normally hw.state.
	Attribute string `yaml:"attribute"`
	// States is every state to emit, including the inactive ones.
	States []string `yaml:"states"`
	// Map translates the raw SNMP value to one of States.
	Map map[string]string `yaml:"map"`
}

// TypeDispatch routes one column to different metrics according to a sibling
// column in the same row. ENTITY-SENSOR-MIB needs this: entPhySensorValue is
// temperature, voltage, fan speed or power depending on entPhySensorType.
type TypeDispatch struct {
	// TypeSymbol is the sibling symbol naming this row's kind.
	TypeSymbol string `yaml:"type_symbol"`
	// Cases maps that sibling's value to an entry name in this registry.
	Cases map[string]string `yaml:"cases"`

	// ScaleSymbol and PrecisionSymbol name the sibling columns holding this
	// row's exponent, as ENTITY-SENSOR-MIB provides. Reading them makes the
	// value correct for whatever a device reports -- deci-celsius on one switch,
	// whole degrees on a firewall, centivolts on a PSU -- instead of hardcoding
	// a divisor per device.
	ScaleSymbol     string `yaml:"scale_symbol"`
	PrecisionSymbol string `yaml:"precision_symbol"`
}

// Entry is one symbol's mapping.
type Entry struct {
	// Metric is the emitted metric name.
	Metric string `yaml:"metric"`
	// Instrument overrides the profile's metric_type. The semantic conventions
	// fix the instrument for hw.* metrics, so where an entry states one it wins:
	// hw.network.bandwidth.limit is an UpDownCounter even though profiles
	// declare ifHighSpeed a gauge.
	Instrument Instrument `yaml:"instrument"`
	Unit       string     `yaml:"unit"`

	// Attributes are datapoint attributes added verbatim, such as
	// hw.type=network and network.io.direction=receive.
	Attributes map[string]string `yaml:"attributes"`

	// Scale multiplies the value, for unit conversions the profile does not do.
	Scale float64 `yaml:"scale"`

	// ValueMap turns an enum into a number. The "default" key covers values not
	// listed, which is how ifOperStatus becomes 1 for up and 0 for everything
	// else.
	ValueMap map[string]float64 `yaml:"value_map"`

	StateSet     *StateSet     `yaml:"state_set"`
	TypeDispatch *TypeDispatch `yaml:"type_dispatch"`

	// ComponentIdentity attaches the per-component attributes (hw.id, hw.name and
	// network.interface.name) to a metric outside the hw.* namespace.
	//
	// Needed where a component metric has no hw.* home: interface discards are
	// system.network.packet.dropped, and without these attributes they could not
	// be joined to the hw.network.* metrics for the same interface.
	ComponentIdentity bool `yaml:"component_identity"`

	// Priority breaks ties when two symbols map to the same metric and the same
	// attributes, which happens whenever a profile collects both a 32-bit and a
	// 64-bit form of one counter, or both ifSpeed and ifHighSpeed. The higher
	// priority wins; without this the receiver would emit two conflicting
	// datapoints for one stream.
	Priority int `yaml:"priority"`
}

// Scheme selects how metrics are named.
type Scheme string

const (
	// SchemeSemconv emits semantic-convention names where known and generated
	// snmp.* names otherwise.
	SchemeSemconv Scheme = "semconv"
	// SchemeDatadogCompat emits snmp.<symbolName> verbatim, easing migration
	// from a Datadog deployment whose dashboards use those names.
	SchemeDatadogCompat Scheme = "datadog_compat"
	// SchemeBoth emits both, for a migration window.
	SchemeBoth Scheme = "both"
)

// Options configures the registry.
type Options struct {
	Scheme Scheme
	// FallbackNamespace prefixes generated names. Named after the protocol
	// rather than the domain, which is against semconv guidance but honestly
	// signals "raw MIB-derived, not yet modelled".
	FallbackNamespace string
	// SystemNamespaceForDeviceOS puts device cpu and memory metrics under
	// system.*, on by default. Set false to send them to the generated fallback
	// namespace. See the README's naming tiers for why system.* is the right home.
	SystemNamespaceForDeviceOS bool
}

// DefaultOptions returns the shipped defaults.
func DefaultOptions() Options {
	return Options{
		Scheme:                     SchemeSemconv,
		FallbackNamespace:          "snmp",
		SystemNamespaceForDeviceOS: true,
	}
}

// Registry resolves symbols to metric descriptors.
type Registry struct {
	opts Options
	// entries is keyed by symbol name.
	entries map[string]Entry
	// systemEntries hold Tier 2 mappings, applied only when opted in.
	systemEntries map[string]Entry
}

// registryFile is the on-disk shape of registry.yaml.
type registryFile struct {
	Metrics       map[string]Entry `yaml:"metrics"`
	SystemMetrics map[string]Entry `yaml:"system_metrics"`
}

// New builds a registry from the embedded curated data.
func New(opts Options) (*Registry, error) {
	if opts.Scheme == "" {
		opts.Scheme = SchemeSemconv
	}
	if opts.FallbackNamespace == "" {
		opts.FallbackNamespace = "snmp"
	}
	switch opts.Scheme {
	case SchemeSemconv, SchemeDatadogCompat, SchemeBoth:
	default:
		return nil, fmt.Errorf("unknown naming scheme %q", opts.Scheme)
	}

	data, err := registryData.ReadFile("registry.yaml")
	if err != nil {
		return nil, fmt.Errorf("read registry: %w", err)
	}
	var file registryFile
	if err := yaml.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse registry: %w", err)
	}

	r := &Registry{
		opts:          opts,
		entries:       make(map[string]Entry, len(file.Metrics)),
		systemEntries: make(map[string]Entry, len(file.SystemMetrics)),
	}
	for symbol, entry := range file.Metrics {
		normalized, err := normalizeEntry(symbol, entry)
		if err != nil {
			return nil, err
		}
		r.entries[symbol] = normalized
	}
	for symbol, entry := range file.SystemMetrics {
		normalized, err := normalizeEntry(symbol, entry)
		if err != nil {
			return nil, err
		}
		r.systemEntries[symbol] = normalized
	}
	return r, nil
}

func normalizeEntry(symbol string, e Entry) (Entry, error) {
	if e.Metric == "" && e.TypeDispatch == nil {
		return e, fmt.Errorf("registry entry %q: needs a metric or a type_dispatch", symbol)
	}
	if e.Scale == 0 {
		e.Scale = 1
	}
	if e.Instrument == "" {
		e.Instrument = Gauge
	}
	if e.StateSet != nil {
		if e.StateSet.Attribute == "" {
			e.StateSet.Attribute = "hw.state"
		}
		if len(e.StateSet.States) == 0 {
			return e, fmt.Errorf("registry entry %q: state_set needs states", symbol)
		}
	}
	return e, nil
}

// Resolution is what to emit for one symbol.
type Resolution struct {
	// Names holds every metric name to emit; more than one only in "both" mode.
	Names []string
	Entry Entry
	// Generated reports that the name was derived rather than curated, so a
	// caller can count how much of a profile is still unmodelled.
	Generated bool
}

// Resolve returns the mapping for a symbol. It never fails: an unmodelled symbol
// falls back to a generated name, because dropping a metric silently is worse
// than emitting it under a non-semconv name.
func (r *Registry) Resolve(mib string, symbol profiledefinition.SymbolConfig) Resolution {
	entry, curated := r.lookup(symbol.Name)
	fallbackName := r.FallbackName(mib, symbol.Name)

	if !curated {
		entry = Entry{
			Metric:     fallbackName,
			Instrument: Gauge,
			Scale:      1,
		}
	}

	return r.applyScheme(entry, symbol.Name, curated)
}

// applyScheme renders an entry under the configured naming scheme.
func (r *Registry) applyScheme(entry Entry, symbolName string, curated bool) Resolution {
	switch r.opts.Scheme {
	case SchemeDatadogCompat:
		// Every symbol lands on one metric named after itself, keeping the
		// curated entry's unit and attributes.
		compat := entry
		compat.Metric = r.compatName(symbolName)
		return Resolution{Names: []string{compat.Metric}, Entry: compat, Generated: true}

	case SchemeBoth:
		names := []string{entry.Metric}
		if compat := r.compatName(symbolName); compat != entry.Metric {
			names = append(names, compat)
		}
		return Resolution{Names: names, Entry: entry, Generated: !curated}

	default:
		return Resolution{Names: []string{entry.Metric}, Entry: entry, Generated: !curated}
	}
}

// lookup finds a curated entry, honouring the Tier 2 opt-in.
func (r *Registry) lookup(symbolName string) (Entry, bool) {
	if entry, ok := r.entries[symbolName]; ok {
		return entry, true
	}
	if r.opts.SystemNamespaceForDeviceOS {
		if entry, ok := r.systemEntries[symbolName]; ok {
			return entry, true
		}
	}
	return Entry{}, false
}

// ResolveDispatched returns the resolution for a type_dispatch target.
//
// This exists because the metric names depend on the target entry, which is only
// known once a row's type column has been read: resolving the target directly and
// reusing the original Resolution would emit the dispatching symbol's name --
// which for a dispatch-only entry is empty.
func (r *Registry) ResolveDispatched(symbolName, target string) (Resolution, bool) {
	entry, ok := r.entries[target]
	if !ok {
		return Resolution{}, false
	}
	return r.applyScheme(entry, symbolName, true), true
}

// compatName is the Datadog metric name for a symbol.
func (r *Registry) compatName(symbolName string) string {
	return "snmp." + symbolName
}

// Curated reports how many symbols the registry maps, for coverage reporting.
func (r *Registry) Curated() int { return len(r.entries) + len(r.systemEntries) }
