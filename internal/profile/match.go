package profile

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// sysObjectIDIndex maps a sysObjectID glob pattern to the profiles declaring it.
type sysObjectIDIndex struct {
	// entries is sorted by pattern for deterministic iteration.
	entries []indexEntry
	// conflicts records patterns claimed by two profiles of equal precedence.
	// These are reported when a device actually matches them, rather than at
	// load time, so one bad user profile cannot stop the receiver starting.
	conflicts map[string][]string
}

type indexEntry struct {
	pattern string
	profile string
	// user profiles outrank embedded ones on an otherwise equal match.
	user bool
}

func buildSysObjectIDIndex(s *Store) *sysObjectIDIndex {
	byPattern := map[string][]indexEntry{}
	for name, def := range s.raw {
		if IsAbstract(name) {
			continue
		}
		for _, pattern := range def.SysObjectIDs {
			pattern = strings.TrimSpace(pattern)
			if pattern == "" {
				continue
			}
			byPattern[pattern] = append(byPattern[pattern], indexEntry{
				pattern: pattern,
				profile: name,
				user:    s.fromUser[name],
			})
		}
	}

	idx := &sysObjectIDIndex{conflicts: map[string][]string{}}
	for pattern, candidates := range byPattern {
		winner, conflicting := pickForPattern(candidates)
		idx.entries = append(idx.entries, winner)
		if len(conflicting) > 0 {
			idx.conflicts[pattern] = conflicting
		}
	}
	sort.Slice(idx.entries, func(i, j int) bool {
		return idx.entries[i].pattern < idx.entries[j].pattern
	})
	return idx
}

// pickForPattern resolves several profiles claiming one pattern: a user profile
// wins outright, otherwise the claim is ambiguous.
func pickForPattern(candidates []indexEntry) (indexEntry, []string) {
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].user != candidates[j].user {
			return candidates[i].user
		}
		return candidates[i].profile < candidates[j].profile
	})
	winner := candidates[0]
	if len(candidates) == 1 {
		return winner, nil
	}
	// A single user profile beating any number of embedded ones is the
	// documented override path, not a conflict.
	if winner.user && !candidates[1].user {
		return winner, nil
	}
	names := make([]string, 0, len(candidates))
	for _, c := range candidates {
		names = append(names, c.profile)
	}
	return winner, names
}

// Match returns the profile for a device's sysObjectID. Among matching glob
// patterns the most specific wins; see comparePatterns.
func (s *Store) Match(sysObjectID string) (string, error) {
	sysObjectID = strings.TrimSpace(strings.TrimPrefix(sysObjectID, "."))
	if sysObjectID == "" {
		return "", fmt.Errorf("empty sysObjectID")
	}

	var best *indexEntry
	for i := range s.index.entries {
		e := &s.index.entries[i]
		ok, err := filepath.Match(e.pattern, sysObjectID)
		if err != nil {
			// A malformed glob in a user profile should not mask good matches.
			continue
		}
		if !ok {
			continue
		}
		if best == nil || comparePatterns(e.pattern, best.pattern) > 0 {
			best = e
		}
	}
	if best == nil {
		return "", fmt.Errorf("no profile matches sysObjectID %s", sysObjectID)
	}
	if conflicting := s.index.conflicts[best.pattern]; len(conflicting) > 0 {
		return "", fmt.Errorf("sysObjectID %s matches pattern %s claimed by several profiles of equal precedence: %s",
			sysObjectID, best.pattern, strings.Join(conflicting, ", "))
	}
	return best.profile, nil
}

// comparePatterns orders two globs by specificity, returning >0 if a is more
// specific than b.
//
// A longer OID prefix is more specific, so part count dominates. At equal
// length, a literal component beats a wildcard at the first position they
// differ: 1.3.6.1.4.1.9.1.* is more specific than 1.3.6.1.4.1.9.*.*.
func comparePatterns(a, b string) int {
	if a == b {
		return 0
	}
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	if len(pa) != len(pb) {
		return len(pa) - len(pb)
	}
	for i := range pa {
		if pa[i] == pb[i] {
			continue
		}
		aWild, bWild := strings.Contains(pa[i], "*"), strings.Contains(pb[i], "*")
		if aWild != bWild {
			if bWild {
				return 1
			}
			return -1
		}
		// Two differing literals cannot both match the same OID, so this is
		// only reached for differing wildcards; fall through to a stable order.
		break
	}
	return strings.Compare(a, b)
}
