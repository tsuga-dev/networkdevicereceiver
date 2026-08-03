package profile

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sync"

	"github.com/tsuga-dev/networkdevicereceiver/internal/profiledefinition"
	"github.com/tsuga-dev/networkdevicereceiver/internal/snmp"
)

// Compiled is a resolved profile prepared for collection: the OIDs to ask for
// are pre-split and every regex is compiled once, rather than per poll.
type Compiled struct {
	Name       string
	Definition *profiledefinition.ProfileDefinition

	// ScalarOIDs are fetched with GET.
	ScalarOIDs []string
	// ColumnOIDs are table columns fetched with GETBULK.
	ColumnOIDs []string

	regexes map[string]*regexp.Regexp

	// missing tracks OIDs a device answered NoSuchObject for. They are dropped
	// from subsequent polls so an unsupported OID is asked for once, not
	// forever. Guarded because pruning happens on the poll path.
	mu      sync.RWMutex
	missing map[string]struct{}
}

// Compile prepares a resolved profile definition for collection.
func Compile(def *profiledefinition.ProfileDefinition) (*Compiled, error) {
	c := &Compiled{
		Name:       def.Name,
		Definition: def,
		regexes:    map[string]*regexp.Regexp{},
		missing:    map[string]struct{}{},
	}

	scalars := map[string]struct{}{}
	columns := map[string]struct{}{}
	var errs []error

	compileRegex := func(pattern, what string) {
		if pattern == "" {
			return
		}
		if _, done := c.regexes[pattern]; done {
			return
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: invalid regex %q: %w", what, pattern, err))
			return
		}
		c.regexes[pattern] = re
	}

	addSymbol := func(s profiledefinition.SymbolConfig, into map[string]struct{}) {
		// A constant_value_one symbol reports 1 per row and has no OID to ask
		// for; including it would produce a NoSuchObject every poll.
		if s.OID == "" {
			return
		}
		into[snmp.CanonicalOID(s.OID)] = struct{}{}
		compileRegex(s.ExtractValue, "extract_value")
		compileRegex(s.MatchPattern, "match_pattern")
	}

	addTags := func(tags profiledefinition.MetricTagConfigList, into map[string]struct{}) {
		for _, tag := range tags {
			addSymbol(tag.Symbol, into)
			compileRegex(tag.Match, "match")
		}
	}

	for i, m := range def.Metrics {
		switch {
		case m.IsColumn():
			// The table root is deliberately not fetched. Walking it would
			// collect every column of the table rather than the ones asked for,
			// and because the root is a prefix of all of them it would also
			// swallow their results during response attribution.
			for _, s := range m.Symbols {
				addSymbol(s, columns)
			}
			// Row tags come from columns, including columns of other tables
			// joined via index_transform.
			addTags(m.MetricTags, columns)
		case m.Symbol.OID != "":
			addSymbol(m.Symbol, scalars)
			// A scalar metric's tags are themselves scalars.
			addTags(m.MetricTags, scalars)
		default:
			errs = append(errs, fmt.Errorf("metrics[%d]: nothing to collect", i))
		}
	}

	// Device-level tags (for example snmp_host from sysName) are scalars.
	addTags(def.MetricTags, scalars)

	for resName, res := range def.Metadata {
		into := columns
		if isScalarResource(resName) {
			into = scalars
		}
		for _, field := range res.Fields {
			addSymbol(field.Symbol, into)
			for _, s := range field.Symbols {
				addSymbol(s, into)
			}
		}
		addTags(res.IDTags, into)
	}

	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("compile profile %q: %w", def.Name, err)
	}

	c.ScalarOIDs = slices.Sorted(maps.Keys(scalars))
	c.ColumnOIDs = slices.Sorted(maps.Keys(columns))
	return c, nil
}

// isScalarResource reports whether a metadata resource describes the device
// itself (one value per device) rather than a table of components.
func isScalarResource(name string) bool {
	return name == "device"
}

// Regexp returns the precompiled regex for a pattern, or nil if the profile did
// not declare it.
func (c *Compiled) Regexp(pattern string) *regexp.Regexp {
	if pattern == "" {
		return nil
	}
	return c.regexes[pattern]
}

// MarkMissing records that a device does not implement these OIDs.
func (c *Compiled) MarkMissing(oids ...string) {
	if len(oids) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, oid := range oids {
		c.missing[snmp.CanonicalOID(oid)] = struct{}{}
	}
}

// FetchOIDs returns the OIDs still worth asking this device for.
func (c *Compiled) FetchOIDs() (scalars, columns []string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.missing) == 0 {
		return c.ScalarOIDs, c.ColumnOIDs
	}
	return c.filter(c.ScalarOIDs), c.filter(c.ColumnOIDs)
}

func (c *Compiled) filter(oids []string) []string {
	out := make([]string, 0, len(oids))
	for _, oid := range oids {
		if _, gone := c.missing[oid]; !gone {
			out = append(out, oid)
		}
	}
	return out
}

// MissingCount reports how many OIDs have been pruned, for diagnostics.
func (c *Compiled) MissingCount() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.missing)
}
