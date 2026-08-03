// Command snmpprofilecheck validates SNMP device profiles.
//
// It loads the embedded profile library plus an optional user directory, then
// resolves, validates and compiles every profile, reporting what each would
// collect and how much of it the naming registry maps to a semantic-convention
// metric. Use it before deploying a hand-written profile: an unparseable or
// unmapped profile is much cheaper to find here than in a poll loop.
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/tsuga-dev/networkdevicereceiver/internal/naming"
	"github.com/tsuga-dev/networkdevicereceiver/internal/profile"
	"github.com/tsuga-dev/networkdevicereceiver/internal/profiledefinition"
)

func main() {
	userDir := flag.String("user-dir", "", "directory of user profiles, which shadow embedded ones by name")
	only := flag.String("profile", "", "check just this profile")
	sysObjectID := flag.String("sysobjectid", "", "report which profile this sysObjectID selects")
	showCoverage := flag.Bool("coverage", false, "report per-profile semantic-convention coverage")
	quiet := flag.Bool("quiet", false, "only report problems")
	flag.Parse()

	if err := run(*userDir, *only, *sysObjectID, *showCoverage, *quiet); err != nil {
		fmt.Fprintf(os.Stderr, "snmpprofilecheck: %v\n", err)
		os.Exit(1)
	}
}

func run(userDir, only, sysObjectID string, showCoverage, quiet bool) error {
	store, err := profile.NewStore(userDir)
	if err != nil {
		return err
	}
	registry, err := naming.New(naming.DefaultOptions())
	if err != nil {
		return err
	}

	if sysObjectID != "" {
		matched, err := store.Match(sysObjectID)
		if err != nil {
			return err
		}
		fmt.Printf("%s selects profile %s\n", sysObjectID, matched)
		return nil
	}

	names := store.Names()
	if only != "" {
		if _, err := store.Resolve(only); err != nil {
			return err
		}
		names = []string{only}
	}
	sort.Strings(names)

	var failures, checked int
	var totalSymbols, mappedSymbols int

	for _, name := range names {
		def, err := store.Resolve(name)
		if err != nil {
			fmt.Printf("FAIL  %s: %v\n", name, err)
			failures++
			continue
		}
		if err := def.Validate(); err != nil {
			fmt.Printf("FAIL  %s:\n%s\n", name, indent(err.Error()))
			failures++
			continue
		}
		compiled, err := profile.Compile(def)
		if err != nil {
			fmt.Printf("FAIL  %s: %v\n", name, err)
			failures++
			continue
		}
		checked++

		total, mapped, unmapped := coverage(registry, def)
		totalSymbols += total
		mappedSymbols += mapped

		if !quiet {
			kind := "profile"
			if profile.IsAbstract(name) {
				kind = "abstract"
			}
			fmt.Printf("ok    %-42s %-8s %4d scalar %5d column %4d/%-4d symbols mapped\n",
				name, kind, len(compiled.ScalarOIDs), len(compiled.ColumnOIDs), mapped, total)
		}
		if showCoverage && len(unmapped) > 0 {
			fmt.Printf("      unmapped: %s\n", strings.Join(truncate(unmapped, 12), ", "))
		}
	}

	fmt.Printf("\n%d profiles checked, %d failed\n", checked, failures)
	if totalSymbols > 0 {
		fmt.Printf("semantic-convention coverage: %d/%d symbol references (%.1f%%)\n",
			mappedSymbols, totalSymbols, 100*float64(mappedSymbols)/float64(totalSymbols))
	}
	if failures > 0 {
		return fmt.Errorf("%d profiles failed validation", failures)
	}
	return nil
}

// coverage reports how many of a profile's symbol references the naming registry
// maps to a curated metric rather than a generated fallback name.
func coverage(registry *naming.Registry, def *profiledefinition.ProfileDefinition) (total, mapped int, unmapped []string) {
	seen := map[string]struct{}{}
	for _, m := range def.Metrics {
		symbols := m.Symbols
		if !m.IsColumn() {
			symbols = []profiledefinition.SymbolConfig{m.Symbol}
		}
		for _, sym := range symbols {
			if sym.Name == "" {
				continue
			}
			total++
			if registry.Resolve(m.MIB, sym).Generated {
				if _, dup := seen[sym.Name]; !dup {
					seen[sym.Name] = struct{}{}
					unmapped = append(unmapped, sym.Name)
				}
				continue
			}
			mapped++
		}
	}
	sort.Strings(unmapped)
	return total, mapped, unmapped
}

func truncate(items []string, limit int) []string {
	if len(items) <= limit {
		return items
	}
	out := append([]string(nil), items[:limit]...)
	return append(out, fmt.Sprintf("... and %d more", len(items)-limit))
}

func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, line := range lines {
		lines[i] = "      " + line
	}
	return strings.Join(lines, "\n")
}
