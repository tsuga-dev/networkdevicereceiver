// Package report turns a poll's value store into OTLP metrics.
//
// It is the half of the design that is deliberately not driven by the profile
// format: profiles say what to collect, the naming registry says what to call
// it, and this package joins the two and produces pmetric.
package report

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/tsuga-dev/networkdevicereceiver/internal/profile"
	"github.com/tsuga-dev/networkdevicereceiver/internal/profiledefinition"
	"github.com/tsuga-dev/networkdevicereceiver/internal/snmp"
)

// stringValue applies a symbol's string-shaping transforms, in the order the
// Datadog agent applies them: raw formatting first, then extraction, then
// pattern rewriting.
func stringValue(compiled *profile.Compiled, sym profiledefinition.SymbolConfig, value snmp.ResultValue) (string, error) {
	text := value.String()

	if sym.Format != "" {
		formatted, err := applyFormat(sym.Format, value)
		if err != nil {
			return "", err
		}
		text = formatted
	}

	if sym.ExtractValue != "" {
		re := compiled.Regexp(sym.ExtractValue)
		if re == nil {
			return "", fmt.Errorf("extract_value %q was not compiled", sym.ExtractValue)
		}
		match := re.FindStringSubmatch(text)
		if len(match) < 2 {
			return "", fmt.Errorf("extract_value %q did not match %q", sym.ExtractValue, text)
		}
		text = match[1]
	}

	if sym.MatchPattern != "" {
		re := compiled.Regexp(sym.MatchPattern)
		if re == nil {
			return "", fmt.Errorf("match_pattern %q was not compiled", sym.MatchPattern)
		}
		match := re.FindStringSubmatchIndex(text)
		if match == nil {
			return "", fmt.Errorf("match_pattern %q did not match %q", sym.MatchPattern, text)
		}
		// Profiles write templates as "$1"; Go's Expand wants "${1}", and the
		// bare form would otherwise be read as a variable named "1x" when
		// followed by text.
		template := expandTemplate(sym.MatchValue)
		text = string(re.ExpandString(nil, template, text, match))
	}

	return text, nil
}

// expandTemplate rewrites $1 style references to ${1} so adjacent literal text
// cannot be absorbed into the group name.
func expandTemplate(template string) string {
	var out strings.Builder
	out.Grow(len(template) + 4)

	for i := 0; i < len(template); i++ {
		c := template[i]
		if c != '$' || i+1 >= len(template) {
			out.WriteByte(c)
			continue
		}
		next := template[i+1]
		if next == '{' || next == '$' {
			out.WriteByte(c)
			continue
		}
		j := i + 1
		for j < len(template) && template[j] >= '0' && template[j] <= '9' {
			j++
		}
		if j == i+1 {
			out.WriteByte(c)
			continue
		}
		out.WriteString("${")
		out.WriteString(template[i+1 : j])
		out.WriteString("}")
		i = j - 1
	}
	return out.String()
}

// applyFormat renders a raw value in a declared representation.
func applyFormat(format string, value snmp.ResultValue) (string, error) {
	switch format {
	case "mac_address":
		raw, ok := value.Bytes()
		if !ok {
			// Some agents return the address already formatted as text.
			return value.String(), nil
		}
		return formatMAC(raw), nil
	case "ip_address":
		raw, ok := value.Bytes()
		if !ok {
			return value.String(), nil
		}
		return formatIP(raw)
	default:
		return "", fmt.Errorf("unsupported format %q", format)
	}
}

func formatMAC(raw []byte) string {
	parts := make([]string, 0, len(raw))
	for _, b := range raw {
		parts = append(parts, fmt.Sprintf("%02x", b))
	}
	return strings.Join(parts, ":")
}

func formatIP(raw []byte) (string, error) {
	switch len(raw) {
	case 4:
		return fmt.Sprintf("%d.%d.%d.%d", raw[0], raw[1], raw[2], raw[3]), nil
	case 16:
		groups := make([]string, 0, 8)
		for i := 0; i < 16; i += 2 {
			groups = append(groups, fmt.Sprintf("%x", int(raw[i])<<8|int(raw[i+1])))
		}
		return strings.Join(groups, ":"), nil
	default:
		return "", fmt.Errorf("ip_address expects 4 or 16 octets, got %d", len(raw))
	}
}

// floatValue converts a value to the number to report, applying the symbol's
// scale_factor and the registry's unit scale.
//
// constant_value_one symbols report 1 regardless of any value, which is how
// profiles count entities such as fans.
func floatValue(sym profiledefinition.SymbolConfig, value snmp.ResultValue, registryScale float64) (float64, error) {
	if sym.ConstantValueOne {
		return 1, nil
	}

	f, err := value.Float()
	if err != nil {
		return 0, err
	}
	if sym.ScaleFactor != 0 {
		f *= sym.ScaleFactor
	}
	if registryScale != 0 && registryScale != 1 {
		f *= registryScale
	}
	return f, nil
}

// flagValue reads one bit of a flag stream. Profiles express these as a string
// of "0"/"1" characters with a 1-based placement.
func flagValue(value snmp.ResultValue, placement uint) (float64, error) {
	flags := strings.TrimSpace(value.String())
	if placement == 0 {
		return 0, fmt.Errorf("flag placement must be 1-based")
	}
	idx := int(placement) - 1
	if idx >= len(flags) {
		return 0, fmt.Errorf("flag placement %d beyond a %d-character flag stream", placement, len(flags))
	}
	switch flags[idx] {
	case '1':
		return 1, nil
	case '0':
		return 0, nil
	default:
		return 0, fmt.Errorf("flag stream character %q is not 0 or 1", flags[idx])
	}
}

// indexArcs splits a row index into its arcs.
func indexArcs(index string) []string {
	if index == "" {
		return nil
	}
	return strings.Split(index, ".")
}

// transformIndex re-slices a row index using inclusive arc ranges, which is how
// a tag from one table is joined onto rows of another whose index is a subset.
//
// Cisco's WLC profiles use this: a radio table's index is an access point's
// six-arc MAC address plus a radio slot, and taking arcs 0-5 yields the key into
// the access point table.
func transformIndex(index string, transforms []profiledefinition.MetricIndexTransform) (string, error) {
	if len(transforms) == 0 {
		return index, nil
	}
	arcs := indexArcs(index)
	var out []string
	for _, tr := range transforms {
		if tr.Start > tr.End {
			return "", fmt.Errorf("index_transform start %d is after end %d", tr.Start, tr.End)
		}
		if int(tr.End) >= len(arcs) {
			return "", fmt.Errorf("index_transform end %d is beyond a %d-arc index %q", tr.End, len(arcs), index)
		}
		out = append(out, arcs[tr.Start:tr.End+1]...)
	}
	return strings.Join(out, "."), nil
}

// indexPosition extracts a 1-based arc of a row index, used by tags that read a
// value out of the index itself rather than from a column.
func indexPosition(index string, position uint) (string, error) {
	arcs := indexArcs(index)
	if position == 0 {
		return "", fmt.Errorf("index position must be 1-based")
	}
	if int(position) > len(arcs) {
		return "", fmt.Errorf("index position %d is beyond a %d-arc index %q", position, len(arcs), index)
	}
	return arcs[position-1], nil
}

// applyMapping translates an enum value through a profile's mapping table.
// Numeric keys are matched after normalising, since a value read as a float
// renders as "4" while the mapping may be keyed "4".
func applyMapping(mapping map[string]string, value string) string {
	if len(mapping) == 0 {
		return value
	}
	if mapped, ok := mapping[value]; ok {
		return mapped
	}
	if f, err := strconv.ParseFloat(value, 64); err == nil {
		if mapped, ok := mapping[strconv.FormatInt(int64(f), 10)]; ok {
			return mapped
		}
	}
	return value
}
