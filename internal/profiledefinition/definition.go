// Package profiledefinition implements the Datadog SNMP profile schema.
//
// The schema is parsed natively rather than converted to an intermediate
// format, so the ~240 upstream profiles and any user profiles from a Datadog
// deployment load unchanged. Output metric naming is deliberately NOT part of
// this package -- see internal/naming.
//
// Ported from DataDog/datadog-agent pkg/networkdevice/profile/profiledefinition
// (Apache-2.0). Field set verified empirically against the 240 shipped profiles
// in DataDog/integrations-core.
package profiledefinition

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"
)

// ProfileMetricType is the metric type declared by a profile. It constrains how
// a value is reported, though the naming registry may override it where the
// semantic conventions fix an instrument type.
type ProfileMetricType string

const (
	// MetricTypeGauge reports the value as-is.
	MetricTypeGauge ProfileMetricType = "gauge"
	// MetricTypeMonotonicCount reports a cumulative monotonic counter.
	MetricTypeMonotonicCount ProfileMetricType = "monotonic_count"
	// MetricTypeMonotonicCountAndRate reports a cumulative counter; Datadog also
	// derives a rate agent-side, which we leave to the pipeline.
	MetricTypeMonotonicCountAndRate ProfileMetricType = "monotonic_count_and_rate"
	// MetricTypeRate reports a per-second rate.
	MetricTypeRate ProfileMetricType = "rate"
	// MetricTypeFlagStream reports one 0/1 metric per bit position of a string
	// of flags, selected by Options.Placement.
	MetricTypeFlagStream ProfileMetricType = "flag_stream"

	// MetricTypeCounter is a legacy alias normalized to rate.
	MetricTypeCounter ProfileMetricType = "counter"
	// MetricTypePercent is a legacy alias normalized to gauge.
	MetricTypePercent ProfileMetricType = "percent"
)

// StringArray accepts either a single YAML scalar or a sequence. `sysobjectid`
// appears as a string in 36 of the shipped profiles and a list in 126.
type StringArray []string

// UnmarshalYAML implements yaml.Unmarshaler.
func (a *StringArray) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		var single string
		if err := node.Decode(&single); err != nil {
			return err
		}
		*a = StringArray{single}
		return nil
	}
	var many []string
	if err := node.Decode(&many); err != nil {
		return err
	}
	*a = many
	return nil
}

// SymbolConfig identifies one OID to collect, plus the per-symbol value
// transforms applied before reporting.
type SymbolConfig struct {
	// OID is empty for ConstantValueOne symbols, which are not fetched.
	OID  string `yaml:"OID"`
	Name string `yaml:"name"`

	// ExtractValue pulls a submatch out of a string value before parsing.
	ExtractValue string `yaml:"extract_value"`
	// MatchPattern and MatchValue rewrite a string value via regex template.
	MatchPattern string `yaml:"match_pattern"`
	MatchValue   string `yaml:"match_value"`

	// ScaleFactor multiplies the parsed value.
	ScaleFactor float64 `yaml:"scale_factor"`
	// Format reinterprets raw bytes, e.g. "mac_address".
	Format string `yaml:"format"`
	// ConstantValueOne reports 1 for every row instead of reading an OID. Used
	// to count entities such as fans or power supplies.
	ConstantValueOne bool `yaml:"constant_value_one"`

	// MetricType set on a symbol overrides the enclosing metric's type.
	MetricType ProfileMetricType `yaml:"metric_type"`
}

// UnmarshalYAML accepts the legacy bare-string form (`symbols: [ifInOctets]`)
// alongside the mapping form.
func (s *SymbolConfig) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		var name string
		if err := node.Decode(&name); err != nil {
			return err
		}
		s.Name = name
		return nil
	}
	type plain SymbolConfig
	var p plain
	if err := node.Decode(&p); err != nil {
		return err
	}
	*s = SymbolConfig(p)
	return nil
}

// MetricIndexTransform is an inclusive range of OID index positions. A list of
// them re-slices a row index so a tag from one table can be joined onto rows of
// another whose index is a prefix or subset.
type MetricIndexTransform struct {
	Start uint `yaml:"start"`
	End   uint `yaml:"end"`
}

// MetricTagConfig derives one tag for each row of a table metric, or one
// device-level tag when used at profile scope.
type MetricTagConfig struct {
	// Tag is the emitted tag key.
	Tag string `yaml:"tag"`

	// Symbol is the column (or scalar) supplying the tag value.
	Symbol SymbolConfig `yaml:"symbol"`
	// Table names the table Symbol belongs to when it differs from the metric's
	// own table, which requires an index join.
	Table string `yaml:"table"`
	MIB   string `yaml:"MIB"`

	// Index takes the value from a position within the row's OID index
	// (1-based) rather than from a column.
	Index uint `yaml:"index"`
	// IndexTransform re-slices the row index before lookup in Table.
	IndexTransform []MetricIndexTransform `yaml:"index_transform"`

	// Mapping translates an enum value to a human-readable string.
	Mapping map[string]string `yaml:"mapping"`

	// Match and Tags split one value into several tags via named regex groups.
	Match string            `yaml:"match"`
	Tags  map[string]string `yaml:"tags"`

	// OID and Name are the legacy inline column form, normalized into Symbol.
	OID  string `yaml:"OID"`
	Name string `yaml:"name"`
	// Column is an older spelling of Symbol, also normalized.
	Column SymbolConfig `yaml:"column"`
}

// MetricsConfigOption carries flag_stream parameters.
type MetricsConfigOption struct {
	// Placement is the 1-based bit position within the flag string.
	Placement uint `yaml:"placement"`
	// MetricSuffix is appended to the symbol name for this flag.
	MetricSuffix string `yaml:"metric_suffix"`
}

// MetricsConfig is one entry of a profile's `metrics` list: either a scalar
// symbol or a table walk producing one datapoint set per row.
type MetricsConfig struct {
	MIB string `yaml:"MIB"`

	// Table and Symbols describe a table walk.
	Table   SymbolConfig   `yaml:"table"`
	Symbols []SymbolConfig `yaml:"symbols"`

	// Symbol describes a single scalar OID.
	Symbol SymbolConfig `yaml:"symbol"`

	MetricTags MetricTagConfigList `yaml:"metric_tags"`
	StaticTags []string            `yaml:"static_tags"`

	MetricType ProfileMetricType   `yaml:"metric_type"`
	Options    MetricsConfigOption `yaml:"options"`

	// ForcedType is the legacy spelling of MetricType.
	ForcedType ProfileMetricType `yaml:"forced_type"`

	// OID and Name are the legacy top-level scalar form, normalized into Symbol.
	OID  string `yaml:"OID"`
	Name string `yaml:"name"`
}

// MetricTagConfigList is a list of tag configs.
type MetricTagConfigList []MetricTagConfig

// IsScalar reports whether this entry collects a single OID rather than a table.
func (m *MetricsConfig) IsScalar() bool {
	return m.Symbol.OID != "" && len(m.Symbols) == 0
}

// IsColumn reports whether this entry walks a table.
func (m *MetricsConfig) IsColumn() bool {
	return len(m.Symbols) > 0
}

// MetadataField is one inventory field: a constant, a single symbol, or a list
// of candidate symbols where the first to yield a value wins.
type MetadataField struct {
	Symbol  SymbolConfig   `yaml:"symbol"`
	Symbols []SymbolConfig `yaml:"symbols"`
	Value   string         `yaml:"value"`
}

// MetadataResourceConfig describes the inventory for one resource kind, keyed
// in the profile by "device" or "interface".
type MetadataResourceConfig struct {
	Fields map[string]MetadataField `yaml:"fields"`
	IDTags MetricTagConfigList      `yaml:"id_tags"`
}

// MetadataConfig maps a resource kind to its inventory definition.
type MetadataConfig map[string]MetadataResourceConfig

// DeviceMeta is the legacy top-level `device:` block, normalized into
// Metadata["device"].Fields["vendor"].
type DeviceMeta struct {
	Vendor string `yaml:"vendor"`
}

// ProfileDefinition is one profile document.
type ProfileDefinition struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`

	// SysObjectIDs are glob patterns matched against a device's sysObjectID.
	// Abstract profiles (78 of the 240 shipped) declare none.
	SysObjectIDs StringArray `yaml:"sysobjectid"`

	// Extends names parent profiles merged in before this profile's own content.
	Extends []string `yaml:"extends"`

	Metrics    []MetricsConfig     `yaml:"metrics"`
	MetricTags MetricTagConfigList `yaml:"metric_tags"`
	StaticTags []string            `yaml:"static_tags"`
	Metadata   MetadataConfig      `yaml:"metadata"`

	Device DeviceMeta `yaml:"device"`
}

// Unmarshal parses a profile document. Unknown fields are rejected so that a
// schema drift in upstream profiles surfaces at load time rather than as
// silently missing metrics.
func Unmarshal(data []byte) (*ProfileDefinition, error) {
	var def ProfileDefinition
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&def); err != nil {
		return nil, fmt.Errorf("parse profile: %w", err)
	}
	return &def, nil
}
