// Package profile loads, resolves and matches SNMP device profiles.
//
// Profiles come from two sources: the ~240 Datadog profiles embedded in the
// binary, and an optional user directory. A user profile shadows an embedded
// one of the same name entirely, matching Datadog's behaviour so an existing
// snmp.d/profiles directory keeps working.
package profile

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/tsuga-dev/networkdevicereceiver/internal/profiledefinition"
)

//go:embed default_profiles/*.yaml
var defaultProfiles embed.FS

const defaultProfilesDir = "default_profiles"

// Store holds raw profile documents and resolves them on demand.
type Store struct {
	// raw holds unresolved documents, keyed by profile name.
	raw map[string]*profiledefinition.ProfileDefinition
	// fromUser marks names supplied by the user directory. Used to break ties
	// during sysObjectID matching.
	fromUser map[string]bool

	mu       sync.Mutex
	resolved map[string]*profiledefinition.ProfileDefinition

	index *sysObjectIDIndex
}

// NewStore loads the embedded profiles, then overlays userDir if non-empty.
func NewStore(userDir string) (*Store, error) {
	s := &Store{
		raw:      map[string]*profiledefinition.ProfileDefinition{},
		fromUser: map[string]bool{},
		resolved: map[string]*profiledefinition.ProfileDefinition{},
	}

	entries, err := fs.ReadDir(defaultProfiles, defaultProfilesDir)
	if err != nil {
		return nil, fmt.Errorf("read embedded profiles: %w", err)
	}
	var errs []error
	for _, e := range entries {
		data, err := defaultProfiles.ReadFile(filepath.Join(defaultProfilesDir, e.Name()))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := s.add(profileName(e.Name()), data, false); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("load embedded profiles: %w", err)
	}

	if userDir != "" {
		if err := s.loadUserDir(userDir); err != nil {
			return nil, err
		}
	}

	s.index = buildSysObjectIDIndex(s)
	return s, nil
}

func (s *Store) loadUserDir(dir string) error {
	paths, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return fmt.Errorf("scan user profile dir: %w", err)
	}
	ymlPaths, err := filepath.Glob(filepath.Join(dir, "*.yml"))
	if err != nil {
		return fmt.Errorf("scan user profile dir: %w", err)
	}
	paths = append(paths, ymlPaths...)

	var errs []error
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p, err))
			continue
		}
		if err := s.add(profileName(filepath.Base(p)), data, true); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p, err))
		}
	}
	return errors.Join(errs...)
}

func (s *Store) add(name string, data []byte, user bool) error {
	def, err := profiledefinition.Unmarshal(data)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	def.Normalize()
	if def.Name == "" {
		def.Name = name
	}
	s.raw[name] = def
	if user {
		s.fromUser[name] = true
	}
	return nil
}

// profileName strips the extension; a profile is referenced by bare name in
// `extends` and in user configuration.
func profileName(filename string) string {
	return strings.TrimSuffix(strings.TrimSuffix(filename, ".yaml"), ".yml")
}

// Names returns every known profile name.
func (s *Store) Names() []string {
	names := make([]string, 0, len(s.raw))
	for n := range s.raw {
		names = append(names, n)
	}
	return names
}

// IsAbstract reports whether a profile exists only to be extended. Datadog
// marks these with a leading underscore and never matches them to a device.
func IsAbstract(name string) bool {
	return strings.HasPrefix(name, "_")
}

// Resolve returns the profile with all `extends` parents merged in. Results are
// cached; the returned value must not be mutated by callers.
func (s *Store) Resolve(name string) (*profiledefinition.ProfileDefinition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resolveLocked(name)
}

func (s *Store) resolveLocked(name string) (*profiledefinition.ProfileDefinition, error) {
	if def, ok := s.resolved[name]; ok {
		return def, nil
	}
	base, ok := s.raw[name]
	if !ok {
		return nil, fmt.Errorf("unknown profile %q", name)
	}

	out := copyDefinition(base)
	// Seed with the profile's own name so a profile that (directly or via a
	// cycle) extends itself merges its content once rather than twice.
	seen := map[string]bool{name: true}
	if err := s.expand(out, base.Extends, seen); err != nil {
		return nil, fmt.Errorf("resolve %q: %w", name, err)
	}
	out.Normalize()

	s.resolved[name] = out
	return out, nil
}

// expand merges each parent, depth first, in declaration order. Parents already
// merged are skipped, so diamond inheritance -- common because most profiles
// reach _base through several paths -- contributes its content once.
func (s *Store) expand(dst *profiledefinition.ProfileDefinition, extends []string, seen map[string]bool) error {
	var errs []error
	for _, ref := range extends {
		// The shipped profiles all reference parents by filename
		// ("_base.yaml"); user profiles may use the bare name.
		parentName := profileName(ref)
		if seen[parentName] {
			continue
		}
		seen[parentName] = true

		parent, ok := s.raw[parentName]
		if !ok {
			errs = append(errs, fmt.Errorf("extends unknown profile %q", ref))
			continue
		}
		merge(dst, parent)
		if err := s.expand(dst, parent.Extends, seen); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// merge appends a parent's collection content after the child's, and fills in
// only metadata fields the child has not already defined -- so the child wins
// on conflicts while inheriting everything it did not mention.
func merge(dst, parent *profiledefinition.ProfileDefinition) {
	dst.Metrics = append(dst.Metrics, parent.Metrics...)
	dst.MetricTags = append(dst.MetricTags, parent.MetricTags...)
	dst.StaticTags = append(dst.StaticTags, parent.StaticTags...)

	if len(parent.Metadata) == 0 {
		return
	}
	if dst.Metadata == nil {
		dst.Metadata = profiledefinition.MetadataConfig{}
	}
	for resName, parentRes := range parent.Metadata {
		dstRes, exists := dst.Metadata[resName]
		if !exists {
			dstRes = profiledefinition.MetadataResourceConfig{}
		}
		if dstRes.Fields == nil && len(parentRes.Fields) > 0 {
			dstRes.Fields = map[string]profiledefinition.MetadataField{}
		}
		for field, def := range parentRes.Fields {
			if _, taken := dstRes.Fields[field]; !taken {
				dstRes.Fields[field] = def
			}
		}
		dstRes.IDTags = append(dstRes.IDTags, parentRes.IDTags...)
		dst.Metadata[resName] = dstRes
	}
}

// copyDefinition deep-copies the parts merge mutates, so resolving one profile
// never corrupts the raw document shared with other profiles that extend it.
func copyDefinition(in *profiledefinition.ProfileDefinition) *profiledefinition.ProfileDefinition {
	out := *in
	out.Metrics = append([]profiledefinition.MetricsConfig(nil), in.Metrics...)
	for i := range out.Metrics {
		out.Metrics[i].Symbols = append([]profiledefinition.SymbolConfig(nil), in.Metrics[i].Symbols...)
		out.Metrics[i].MetricTags = append(profiledefinition.MetricTagConfigList(nil), in.Metrics[i].MetricTags...)
		out.Metrics[i].StaticTags = append([]string(nil), in.Metrics[i].StaticTags...)
	}
	out.MetricTags = append(profiledefinition.MetricTagConfigList(nil), in.MetricTags...)
	out.StaticTags = append([]string(nil), in.StaticTags...)

	if in.Metadata != nil {
		out.Metadata = profiledefinition.MetadataConfig{}
		for name, res := range in.Metadata {
			cp := profiledefinition.MetadataResourceConfig{
				IDTags: append(profiledefinition.MetricTagConfigList(nil), res.IDTags...),
			}
			if res.Fields != nil {
				cp.Fields = make(map[string]profiledefinition.MetadataField, len(res.Fields))
				maps.Copy(cp.Fields, res.Fields)
			}
			out.Metadata[name] = cp
		}
	}
	return &out
}
