package naming

import (
	"strings"
	"unicode"
)

// FallbackName derives a deterministic name for a symbol the registry does not
// model: <namespace>.<mib>.<symbol>, each part snake_cased.
//
//	IF-MIB / ifHCInOctets              -> snmp.if.if_hc_in_octets
//	CISCO-MEMORY-POOL-MIB / memPoolUsed -> snmp.cisco_memory_pool.mem_pool_used
//
// It depends only on the MIB and symbol names, so a name stays stable across
// upstream profile syncs. Renaming one later is a breaking change and must go
// behind a feature gate.
func (r *Registry) FallbackName(mib, symbol string) string {
	parts := []string{r.opts.FallbackNamespace}
	if m := normalizeMIB(mib); m != "" {
		parts = append(parts, m)
	}
	if s := normalizeSymbol(symbol); s != "" {
		parts = append(parts, s)
	}
	return strings.Join(parts, ".")
}

// normalizeMIB lowercases a MIB name and drops the conventional -MIB suffix,
// which carries no information.
func normalizeMIB(mib string) string {
	mib = strings.TrimSpace(mib)
	if mib == "" {
		return ""
	}
	mib = strings.TrimSuffix(strings.ToUpper(mib), "-MIB")
	mib = strings.ToLower(mib)
	return sanitize(strings.ReplaceAll(mib, "-", "_"))
}

// normalizeSymbol snake_cases a symbol name. Symbol names in the corpus are
// mostly camelCase MIB object names, but some are already dotted paths such as
// "cpu.usage"; dots are kept as namespace separators.
func normalizeSymbol(symbol string) string {
	symbol = strings.TrimSpace(symbol)
	if symbol == "" {
		return ""
	}
	segments := strings.Split(symbol, ".")
	for i, seg := range segments {
		segments[i] = sanitize(camelToSnake(seg))
	}
	return strings.Join(segments, ".")
}

// Snake converts camelCase to snake_case and drops characters that are not valid
// in an OTel metric name or attribute value: ifHCInOctets becomes
// if_hc_in_octets, entPhySensor becomes ent_phy_sensor.
//
// Exported because the reporting stage derives component identifiers (hw.id) from
// the same MIB table and symbol names, and the two must agree.
func Snake(s string) string {
	return sanitize(camelToSnake(s))
}

// camelToSnake converts camelCase to snake_case, keeping acronym runs together:
// ifHCInOctets becomes if_hc_in_octets rather than if_h_c_in_octets.
func camelToSnake(s string) string {
	runes := []rune(s)
	var out strings.Builder
	out.Grow(len(runes) + 4)

	for i, r := range runes {
		if !unicode.IsUpper(r) {
			out.WriteRune(r)
			continue
		}
		if i > 0 {
			prev := runes[i-1]
			// Break when leaving a lowercase or digit run, or at the end of an
			// acronym run that is followed by a new word.
			endOfAcronym := unicode.IsUpper(prev) && i+1 < len(runes) && unicode.IsLower(runes[i+1])
			if unicode.IsLower(prev) || unicode.IsDigit(prev) || endOfAcronym {
				out.WriteRune('_')
			}
		}
		out.WriteRune(unicode.ToLower(r))
	}
	return out.String()
}

// sanitize replaces characters that are not valid in an OTel metric name.
func sanitize(s string) string {
	var out strings.Builder
	out.Grow(len(s))
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '.' || r == '-' || r == '/':
			out.WriteRune(r)
		default:
			out.WriteRune('_')
		}
	}
	return strings.Trim(out.String(), "_")
}
