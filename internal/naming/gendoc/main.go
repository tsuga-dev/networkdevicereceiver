// Command gendoc regenerates the per-metric reference in documentation.md from
// registry.yaml, so a mapping and its documentation cannot drift.
//
// It rewrites only the block between the marker comments; everything else in
// documentation.md is hand-written, because the transformations and the fallback
// naming rules are not expressible as registry data.
//
// Run via go generate, from internal/naming:
//
//	go generate ./...
package main

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/tsuga-dev/networkdevicereceiver/internal/naming"
)

const (
	registryPath = "registry.yaml"
	docPath      = "../../documentation.md"
	beginMarker  = "<!-- BEGIN GENERATED: metrics -->"
	endMarker    = "<!-- END GENERATED: metrics -->"

	// gatingNote is appended to a metric whose symbols live in system_metrics. The
	// line breaks keep the generated block within the document's wrap width.
	gatingNote = "Emitted only when `naming.system_namespace_for_device_os` is `true`, its\n" +
		"default. Set it to `false` and these symbols resolve to generated `snmp.*`\n" +
		"names instead."
)

// registryFile is the documentation-facing view of registry.yaml. The runtime
// reads the same file with its own struct and ignores metric_docs.
type registryFile struct {
	Metrics       map[string]naming.Entry `yaml:"metrics"`
	SystemMetrics map[string]naming.Entry `yaml:"system_metrics"`
	// MetricDocs is keyed by emitted metric name, not by symbol: a description
	// describes the metric, and several symbols feed one metric.
	MetricDocs map[string]metricDoc `yaml:"metric_docs"`
}

type metricDoc struct {
	Description string `yaml:"description"`
	Notes       string `yaml:"notes"`
}

// source is one symbol contributing to a metric.
type source struct {
	symbol string
	// condition is non-empty for a type-dispatched sensor, whose target depends
	// on a sibling column.
	condition string
	// scaleNote replaces the numeric scale where the exponent is per-row.
	scaleNote string
	entry     naming.Entry
	// gated marks a symbol that only resolves when the Tier 2 opt-in is on.
	gated bool
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gendoc:", err)
		os.Exit(1)
	}
}

func run() error {
	data, err := os.ReadFile(registryPath)
	if err != nil {
		return err
	}
	var file registryFile
	if err := yaml.Unmarshal(data, &file); err != nil {
		return fmt.Errorf("parse %s: %w", registryPath, err)
	}

	byMetric := collect(file)
	section, err := render(byMetric, file.MetricDocs)
	if err != nil {
		return err
	}
	return splice(docPath, section)
}

// collect groups symbols by the metric they emit.
func collect(file registryFile) map[string][]source {
	// A type_dispatch entry has no metric of its own: it names other entries as
	// targets, and those targets' sources are the dispatching symbol under a
	// condition. Collect that indirection before walking the entries.
	dispatched := map[string][]source{}
	for symbol, entry := range file.Metrics {
		td := entry.TypeDispatch
		if td == nil {
			continue
		}
		var scaleNote string
		if td.ScaleSymbol != "" || td.PrecisionSymbol != "" {
			scaleNote = "per-row, from " + code(td.ScaleSymbol) + " and " + code(td.PrecisionSymbol)
		}
		for value, target := range td.Cases {
			dispatched[target] = append(dispatched[target], source{
				symbol:    symbol,
				condition: code(td.TypeSymbol) + " = " + code(value),
				scaleNote: scaleNote,
			})
		}
	}

	byMetric := map[string][]source{}
	add := func(key string, entry naming.Entry, gated bool) {
		if entry.TypeDispatch != nil {
			return
		}
		sources := dispatched[key]
		if len(sources) == 0 {
			// Not a dispatch target, so the entry key is the symbol name.
			sources = []source{{symbol: key}}
		}
		for _, s := range sources {
			s.entry = entry
			s.gated = gated
			byMetric[entry.Metric] = append(byMetric[entry.Metric], s)
		}
	}
	for key, entry := range file.Metrics {
		add(key, entry, false)
	}
	for key, entry := range file.SystemMetrics {
		add(key, entry, true)
	}

	for _, sources := range byMetric {
		sort.Slice(sources, func(i, j int) bool {
			if sources[i].symbol != sources[j].symbol {
				return sources[i].symbol < sources[j].symbol
			}
			return sources[i].condition < sources[j].condition
		})
	}
	return byMetric
}

func render(byMetric map[string][]source, docs map[string]metricDoc) (string, error) {
	metrics := make([]string, 0, len(byMetric))
	for metric := range byMetric {
		metrics = append(metrics, metric)
	}
	sort.Strings(metrics)

	// A doc entry naming no metric is a typo that would otherwise go unnoticed.
	for metric := range docs {
		if _, ok := byMetric[metric]; !ok {
			return "", fmt.Errorf("metric_docs has an entry for %q, which no registry entry emits", metric)
		}
	}

	var out strings.Builder
	for _, metric := range metrics {
		doc, ok := docs[metric]
		if !ok || doc.Description == "" {
			return "", fmt.Errorf("metric %s has no metric_docs description", metric)
		}
		if err := renderMetric(&out, metric, byMetric[metric], doc); err != nil {
			return "", err
		}
	}
	return out.String(), nil
}

func renderMetric(out *strings.Builder, metric string, sources []source, doc metricDoc) error {
	first := sources[0].entry
	for _, s := range sources[1:] {
		// pool.go rejects this at runtime, per device. Catching it here makes it a
		// build failure instead.
		if s.entry.Instrument != first.Instrument {
			return fmt.Errorf("metric %s is both %s (%s) and %s (%s)",
				metric, first.Instrument, sources[0].symbol, s.entry.Instrument, s.symbol)
		}
		if s.entry.Unit != first.Unit {
			return fmt.Errorf("metric %s has units %q (%s) and %q (%s)",
				metric, first.Unit, sources[0].symbol, s.entry.Unit, s.symbol)
		}
	}

	fmt.Fprintf(out, "### %s\n\n%s\n\n", metric, strings.TrimSpace(doc.Description))

	kind, monotonic := instrumentShape(first.Instrument)
	out.WriteString("| Unit | Metric Type | Value Type | Monotonic |\n")
	out.WriteString("| ---- | ----------- | ---------- | --------- |\n")
	fmt.Fprintf(out, "| %s | %s | Double | %s |\n\n", orDash(first.Unit), kind, monotonic)

	writeSourceTable(out, sources)

	if gated(sources) {
		fmt.Fprintf(out, "\n%s\n", gatingNote)
	}
	if notes := strings.TrimSpace(doc.Notes); notes != "" {
		fmt.Fprintf(out, "\n%s\n", notes)
	}
	out.WriteString("\n")
	return nil
}

// writeSourceTable writes one row per contributing symbol. Columns that no row
// uses are omitted, so a plain mapping is not padded with empty cells.
func writeSourceTable(out *strings.Builder, sources []source) {
	type column struct {
		header string
		cell   func(source) string
	}
	candidates := []column{
		{"Source symbol", func(s source) string { return code(s.symbol) }},
		{"Condition", func(s source) string { return s.condition }},
		{"Attributes", attributesCell},
		{"Scale", scaleCell},
		{"Value mapping", valueMapCell},
		{"State mapping", stateMapCell},
		{"Priority", func(s source) string {
			if s.entry.Priority == 0 {
				return ""
			}
			return strconv.Itoa(s.entry.Priority)
		}},
	}

	rows := make([]map[string]string, len(sources))
	var used []column
	for _, col := range candidates {
		inUse := false
		for i, s := range sources {
			if rows[i] == nil {
				rows[i] = map[string]string{}
			}
			cell := col.cell(s)
			rows[i][col.header] = cell
			if cell != "" {
				inUse = true
			}
		}
		// The symbol column always carries a value, so this only ever drops
		// genuinely unused columns.
		if inUse {
			used = append(used, col)
		}
	}

	headers := make([]string, len(used))
	dividers := make([]string, len(used))
	for i, col := range used {
		headers[i] = col.header
		dividers[i] = "---"
	}
	fmt.Fprintf(out, "| %s |\n| %s |\n", strings.Join(headers, " | "), strings.Join(dividers, " | "))

	for _, row := range rows {
		cells := make([]string, len(used))
		for i, col := range used {
			cells[i] = row[col.header]
		}
		fmt.Fprintf(out, "| %s |\n", strings.Join(cells, " | "))
	}
}

// attributesCell lists the datapoint attributes the entry declares, plus the
// state attribute a state set fans out over.
func attributesCell(s source) string {
	parts := make([]string, 0, len(s.entry.Attributes)+1)
	for key, value := range s.entry.Attributes {
		parts = append(parts, code(key+"="+value))
	}
	sort.Strings(parts)

	if set := s.entry.StateSet; set != nil {
		states := make([]string, len(set.States))
		for i, state := range set.States {
			states[i] = code(state)
		}
		parts = append(parts, fmt.Sprintf("%s ∈ {%s}", code(set.Attribute), strings.Join(states, ", ")))
	}
	return strings.Join(parts, ", ")
}

// scaleCell renders the multiplier applied to the raw value. An absent or unity
// scale is no scaling; registry.go normalises 0 to 1.
func scaleCell(s source) string {
	if s.scaleNote != "" {
		return s.scaleNote
	}
	if s.entry.Scale == 0 || s.entry.Scale == 1 {
		return ""
	}
	return "× " + strconv.FormatFloat(s.entry.Scale, 'f', -1, 64)
}

func valueMapCell(s source) string {
	if len(s.entry.ValueMap) == 0 {
		return ""
	}
	keys := make([]string, 0, len(s.entry.ValueMap))
	for key := range s.entry.ValueMap {
		if key != "default" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys)+1)
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s → %s", code(key), number(s.entry.ValueMap[key])))
	}
	if fallback, ok := s.entry.ValueMap["default"]; ok {
		parts = append(parts, "anything else → "+number(fallback))
	}
	return strings.Join(parts, ", ")
}

func stateMapCell(s source) string {
	set := s.entry.StateSet
	if set == nil || len(set.Map) == 0 {
		return ""
	}
	keys := make([]string, 0, len(set.Map))
	for key := range set.Map {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	parts := make([]string, len(keys))
	for i, key := range keys {
		parts[i] = fmt.Sprintf("%s → %s", code(key), code(set.Map[key]))
	}
	return strings.Join(parts, ", ")
}

// instrumentShape maps an instrument onto the OTLP metric type and monotonicity
// that pool.go creates it with.
func instrumentShape(instrument naming.Instrument) (kind, monotonic string) {
	switch instrument {
	case naming.Sum:
		return "Sum", "true"
	case naming.UpDownCounter:
		return "Sum", "false"
	default:
		// registry.go defaults an unstated instrument to a gauge.
		return "Gauge", "n/a"
	}
}

func gated(sources []source) bool {
	for _, s := range sources {
		if s.gated {
			return true
		}
	}
	return false
}

func number(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

func code(s string) string {
	if s == "" {
		return ""
	}
	return "`" + s + "`"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// splice replaces the marked block in the document, leaving the hand-written
// text either side of it untouched.
func splice(path, section string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	doc := string(data)

	begin := strings.Index(doc, beginMarker)
	end := strings.Index(doc, endMarker)
	if begin < 0 || end < 0 {
		return fmt.Errorf("%s is missing the %s / %s markers", path, beginMarker, endMarker)
	}
	if end < begin {
		return fmt.Errorf("%s has the markers in the wrong order", path)
	}

	updated := doc[:begin+len(beginMarker)] + "\n\n" + section + doc[end:]
	if updated == doc {
		return nil
	}
	return os.WriteFile(path, []byte(updated), 0o644)
}
