package profiledefinition

import (
	"errors"
	"fmt"
)

// Normalize upconverts legacy spellings to the current schema so the rest of
// the receiver only handles one shape. It is idempotent.
//
// Datadog kept several older forms working; 36 of the shipped profiles still
// use the top-level `device.vendor` block and a handful use inline tag columns.
func (p *ProfileDefinition) Normalize() {
	for i := range p.Metrics {
		p.Metrics[i].normalize()
	}
	p.MetricTags.normalize()

	for name, res := range p.Metadata {
		res.IDTags.normalize()
		p.Metadata[name] = res
	}

	// Legacy `device: {vendor: x}` is the same statement as a constant
	// metadata field, so fold it in and let one code path read it.
	if p.Device.Vendor != "" {
		if p.Metadata == nil {
			p.Metadata = MetadataConfig{}
		}
		dev := p.Metadata["device"]
		if dev.Fields == nil {
			dev.Fields = map[string]MetadataField{}
		}
		if _, ok := dev.Fields["vendor"]; !ok {
			dev.Fields["vendor"] = MetadataField{Value: p.Device.Vendor}
		}
		p.Metadata["device"] = dev
		p.Device.Vendor = ""
	}
}

func (m *MetricsConfig) normalize() {
	if m.MetricType == "" && m.ForcedType != "" {
		m.MetricType = m.ForcedType
	}
	m.ForcedType = ""
	m.MetricType = m.MetricType.normalize()

	// Legacy scalar form: OID and name directly on the metric entry.
	if m.Symbol.OID == "" && m.OID != "" {
		m.Symbol = SymbolConfig{OID: m.OID, Name: m.Name}
		m.OID, m.Name = "", ""
	}

	for i := range m.Symbols {
		m.Symbols[i].MetricType = m.Symbols[i].MetricType.normalize()
	}
	m.Symbol.MetricType = m.Symbol.MetricType.normalize()
	m.MetricTags.normalize()
}

func (l MetricTagConfigList) normalize() {
	for i := range l {
		l[i].normalize()
	}
}

func (t *MetricTagConfig) normalize() {
	// `column:` is an older spelling of `symbol:`.
	if t.Symbol.OID == "" && t.Column.OID != "" {
		t.Symbol = t.Column
		t.Column = SymbolConfig{}
	}
	// Inline OID/name on the tag itself.
	if t.Symbol.OID == "" && t.OID != "" {
		t.Symbol = SymbolConfig{OID: t.OID, Name: t.Name}
		t.OID = ""
	}
}

// normalize maps legacy metric types onto their modern equivalents.
func (t ProfileMetricType) normalize() ProfileMetricType {
	switch t {
	case MetricTypeCounter:
		// Datadog's `counter` submitted a per-second rate.
		return MetricTypeRate
	case MetricTypePercent:
		// `percent` was a plain gauge whose value happened to be a percentage.
		// Unit handling belongs to the naming registry, not here.
		return MetricTypeGauge
	default:
		return t
	}
}

// Validate reports every structural problem in the profile at once, so a user
// fixing a hand-written profile sees the whole list rather than the first error.
//
// It is intentionally not called on abstract profiles in isolation: a profile
// that only supplies metadata or is extended by others is legitimately without
// metrics, and its sysobjectid is legitimately empty.
func (p *ProfileDefinition) Validate() error {
	var errs []error
	for i := range p.Metrics {
		if err := p.Metrics[i].validate(); err != nil {
			errs = append(errs, fmt.Errorf("metrics[%d]: %w", i, err))
		}
	}
	for i := range p.MetricTags {
		if err := p.MetricTags[i].validate(); err != nil {
			errs = append(errs, fmt.Errorf("metric_tags[%d]: %w", i, err))
		}
	}
	for name, res := range p.Metadata {
		for field, def := range res.Fields {
			if def.Value == "" && def.Symbol.OID == "" && len(def.Symbols) == 0 {
				errs = append(errs, fmt.Errorf("metadata.%s.fields.%s: needs value, symbol or symbols", name, field))
			}
		}
		for i := range res.IDTags {
			if err := res.IDTags[i].validate(); err != nil {
				errs = append(errs, fmt.Errorf("metadata.%s.id_tags[%d]: %w", name, i, err))
			}
		}
	}
	return errors.Join(errs...)
}

func (m *MetricsConfig) validate() error {
	var errs []error

	switch {
	case m.IsColumn():
		if m.Table.OID == "" {
			errs = append(errs, errors.New("table metric needs table.OID"))
		}
		for i, s := range m.Symbols {
			if err := s.validate(); err != nil {
				errs = append(errs, fmt.Errorf("symbols[%d]: %w", i, err))
			}
		}
	case m.Symbol.OID != "" || m.Symbol.Name != "":
		if err := m.Symbol.validate(); err != nil {
			errs = append(errs, fmt.Errorf("symbol: %w", err))
		}
		if m.Symbol.OID == "" {
			errs = append(errs, errors.New("scalar metric needs symbol.OID"))
		}
	default:
		errs = append(errs, errors.New("needs either symbol or symbols"))
	}

	if m.MetricType == MetricTypeFlagStream {
		// Without a placement there is no bit to read, and without a suffix
		// every flag of the same symbol would collide on one metric name.
		if m.Options.Placement == 0 {
			errs = append(errs, errors.New("flag_stream needs options.placement"))
		}
		if m.Options.MetricSuffix == "" {
			errs = append(errs, errors.New("flag_stream needs options.metric_suffix"))
		}
	}

	for i := range m.MetricTags {
		if err := m.MetricTags[i].validate(); err != nil {
			errs = append(errs, fmt.Errorf("metric_tags[%d]: %w", i, err))
		}
	}
	return errors.Join(errs...)
}

func (s *SymbolConfig) validate() error {
	if s.Name == "" {
		return errors.New("needs name")
	}
	// A symbol with no OID is only meaningful as a per-row constant.
	if s.OID == "" && !s.ConstantValueOne {
		return errors.New("needs OID unless constant_value_one is set")
	}
	if s.MatchPattern != "" && s.MatchValue == "" {
		return errors.New("match_pattern needs match_value")
	}
	return nil
}

func (t *MetricTagConfig) validate() error {
	// A tag is identified either by a single key, or by a regex that yields
	// several keys at once.
	if t.Tag == "" && len(t.Tags) == 0 {
		return errors.New("needs tag or tags")
	}
	if t.Match != "" && len(t.Tags) == 0 {
		return errors.New("match needs tags")
	}
	if len(t.Tags) > 0 && t.Match == "" {
		return errors.New("tags needs match")
	}
	// The value comes from a column, or from a position in the row index.
	if t.Symbol.OID == "" && t.Index == 0 {
		return errors.New("needs symbol or index")
	}
	return nil
}
