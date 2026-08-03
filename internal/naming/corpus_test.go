package naming

import (
	"testing"

	"github.com/tsuga-dev/networkdevicereceiver/internal/profile"
	"github.com/tsuga-dev/networkdevicereceiver/internal/profiledefinition"
)

// collectedSymbols returns every symbol name the shipped profiles actually
// collect, whether as a metric symbol or as a tag column.
func collectedSymbols(t *testing.T) map[string]struct{} {
	t.Helper()
	store, err := profile.NewStore("")
	if err != nil {
		t.Fatal(err)
	}

	out := map[string]struct{}{}
	record := func(s profiledefinition.SymbolConfig) {
		if s.Name != "" {
			out[s.Name] = struct{}{}
		}
	}
	for _, name := range store.Names() {
		def, err := store.Resolve(name)
		if err != nil {
			continue
		}
		for _, m := range def.Metrics {
			record(m.Symbol)
			for _, s := range m.Symbols {
				record(s)
			}
			for _, tag := range m.MetricTags {
				record(tag.Symbol)
			}
		}
		for _, tag := range def.MetricTags {
			record(tag.Symbol)
		}
		for _, res := range def.Metadata {
			for _, field := range res.Fields {
				record(field.Symbol)
				for _, s := range field.Symbols {
					record(s)
				}
			}
			for _, tag := range res.IDTags {
				record(tag.Symbol)
			}
		}
	}
	return out
}

// dispatchTargets returns the entry names referenced only as type_dispatch
// targets. These are not symbols and are never looked up by symbol name.
func dispatchTargets(r *Registry) map[string]struct{} {
	out := map[string]struct{}{}
	for _, entry := range r.entries {
		if entry.TypeDispatch == nil {
			continue
		}
		for _, target := range entry.TypeDispatch.Cases {
			out[target] = struct{}{}
		}
	}
	return out
}

// TestEveryCuratedSymbolIsCollected keeps the registry honest: an entry for a
// symbol no profile collects is dead weight that overstates coverage, and it will
// never fire no matter how the device is configured.
//
// The same applies to a dispatch's sibling columns, which is the lesson from
// mapping Cisco's entity sensors: dispatching a sensor to hw.temperature without
// its scale and precision columns reports the wrong magnitude, which is worse than
// reporting it under a generated name.
func TestEveryCuratedSymbolIsCollected(t *testing.T) {
	r := newRegistry(t, DefaultOptions())
	collected := collectedSymbols(t)
	targets := dispatchTargets(r)

	mustCollect := func(what, symbol string) {
		if _, ok := collected[symbol]; !ok {
			t.Errorf("%s %q is not collected by any shipped profile, so it can never fire", what, symbol)
		}
	}
	check := func(kind string, entries map[string]Entry) {
		for symbol, entry := range entries {
			if _, isTarget := targets[symbol]; !isTarget {
				mustCollect(kind+" entry", symbol)
			}
			td := entry.TypeDispatch
			if td == nil {
				continue
			}
			mustCollect(symbol+" dispatch column", td.TypeSymbol)
			// A dispatch with no exponent source may only target metrics whose
			// unit does not depend on scaling, which none of ours are.
			if td.ScaleSymbol == "" && td.PrecisionSymbol == "" {
				t.Errorf("%s dispatches without a scale or precision symbol; its magnitude cannot be derived", symbol)
			}
			for _, sibling := range []string{td.ScaleSymbol, td.PrecisionSymbol} {
				if sibling != "" {
					mustCollect(symbol+" exponent column", sibling)
				}
			}
		}
	}
	check("metrics", r.entries)
	check("system_metrics", r.systemEntries)
}
