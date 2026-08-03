// Package report turns a poll's value store into OTLP metrics.
//
// It is the half of the design that is deliberately not driven by the profile
// format: profiles say what to collect, the naming registry says what to call
// it, and this package joins the two and produces pmetric.
package report

import (
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

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

	return sanitizeText(text), nil
}

// sanitizeText makes a value safe to put in an OTLP string field.
//
// SNMP OCTET STRING is arbitrary bytes, and plenty of columns hold binary rather
// than text -- a six-byte MAC address being the common case. OTLP string fields
// must be valid UTF-8, and a backend that validates rejects the *entire* request,
// so one binary column silently destroys every metric for that device rather
// than just its own datapoint.
//
// Invalid values are hex-encoded, matching how such columns are conventionally
// rendered (f47f3593af80). A profile wanting colon-separated octets should
// declare format: mac_address, which runs before this.
func sanitizeText(text string) string {
	if utf8.ValidString(text) {
		return text
	}
	return hex.EncodeToString([]byte(text))
}

// bareGroupRef matches a $1 style capture reference that is not already braced.
var bareGroupRef = regexp.MustCompile(`\$(\d+)`)

// expandTemplate rewrites $1 style references to ${1} so adjacent literal text
// cannot be absorbed into the group name. "$$" in the replacement emits a literal
// "$", so the output is "${" + digits + "}".
func expandTemplate(template string) string {
	return bareGroupRef.ReplaceAllString(template, "$${${1}}")
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

// formatMAC renders packed octets as colon-separated lowercase hex.
func formatMAC(raw []byte) string {
	return net.HardwareAddr(raw).String()
}

// formatIP renders 4 or 16 packed octets as an address. IPv6 comes out in the
// canonical compressed form (2001:db8::1) that consumers expect.
func formatIP(raw []byte) (string, error) {
	addr, ok := netip.AddrFromSlice(raw)
	if !ok {
		return "", fmt.Errorf("ip_address expects 4 or 16 octets, got %d", len(raw))
	}
	return addr.String(), nil
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
	arcs := strings.Split(index, ".")
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
	arcs := strings.Split(index, ".")
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
