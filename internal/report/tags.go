package report

import (
	"errors"
	"fmt"

	"github.com/tsuga-dev/networkdevicereceiver/internal/profile"
	"github.com/tsuga-dev/networkdevicereceiver/internal/profiledefinition"
	"github.com/tsuga-dev/networkdevicereceiver/internal/snmp"
)

// tagResolver resolves profile tag configurations against one poll's values.
type tagResolver struct {
	compiled *profile.Compiled
	store    *snmp.ValueStore
}

// rowTags resolves a table metric's tags for one row.
//
// A tag that cannot be resolved is skipped rather than failing the row: a device
// that omits ifAlias should still yield interface traffic. The errors are
// returned so the caller can log them once per poll instead of per datapoint.
func (r *tagResolver) rowTags(tags profiledefinition.MetricTagConfigList, index string) (map[string]string, error) {
	out := make(map[string]string, len(tags))
	var errs []error

	for i := range tags {
		tag := &tags[i]
		if err := r.resolveTag(tag, index, true, out); err != nil {
			errs = append(errs, fmt.Errorf("tag %q: %w", tagLabel(tag), err))
		}
	}
	return out, errors.Join(errs...)
}

// deviceTags resolves profile-level tags, whose symbols are scalars.
func (r *tagResolver) deviceTags(tags profiledefinition.MetricTagConfigList) (map[string]string, error) {
	out := make(map[string]string, len(tags))
	var errs []error

	for i := range tags {
		tag := &tags[i]
		if err := r.resolveTag(tag, "", false, out); err != nil {
			errs = append(errs, fmt.Errorf("tag %q: %w", tagLabel(tag), err))
		}
	}
	return out, errors.Join(errs...)
}

func tagLabel(tag *profiledefinition.MetricTagConfig) string {
	if tag.Tag != "" {
		return tag.Tag
	}
	if tag.Symbol.Name != "" {
		return tag.Symbol.Name
	}
	return "<unnamed>"
}

func (r *tagResolver) resolveTag(tag *profiledefinition.MetricTagConfig, index string, isRow bool, out map[string]string) error {
	// A tag reading a position out of the row index needs no column at all.
	if tag.Symbol.OID == "" {
		if !isRow {
			return fmt.Errorf("index-based tag is only meaningful on a table row")
		}
		raw, err := indexPosition(index, tag.Index)
		if err != nil {
			return err
		}
		out[tag.Tag] = applyMapping(tag.Mapping, raw)
		return nil
	}

	value, err := r.tagValue(tag, index, isRow)
	if err != nil {
		return err
	}

	text, err := stringValue(r.compiled, tag.Symbol, value)
	if err != nil {
		return err
	}
	text = applyMapping(tag.Mapping, text)

	// A match/tags pair splits one value into several tags via capture groups.
	if tag.Match != "" && len(tag.Tags) > 0 {
		return r.expandMatchTags(tag, text, out)
	}
	if tag.Tag == "" {
		return fmt.Errorf("no tag name")
	}
	// An empty value carries no information and would otherwise appear on every
	// datapoint: real devices routinely leave optional columns such as ifAlias
	// blank.
	if text == "" {
		return nil
	}
	out[tag.Tag] = text
	return nil
}

// tagValue reads the tag's source value, joining across tables when the tag
// declares an index_transform.
func (r *tagResolver) tagValue(tag *profiledefinition.MetricTagConfig, index string, isRow bool) (snmp.ResultValue, error) {
	if !isRow {
		return r.store.Scalar(tag.Symbol.OID)
	}

	target, err := transformIndex(index, tag.IndexTransform)
	if err != nil {
		return snmp.ResultValue{}, err
	}
	value, err := r.store.ColumnValue(tag.Symbol.OID, target)
	if err == nil {
		return value, nil
	}
	// A tag column may legitimately be a scalar in a profile that mixes them.
	if scalar, scalarErr := r.store.Scalar(tag.Symbol.OID); scalarErr == nil {
		return scalar, nil
	}
	return snmp.ResultValue{}, err
}

// expandMatchTags applies a regex to one value and emits a tag per named entry
// in the profile's tags map, whose values are templates over the capture groups.
func (r *tagResolver) expandMatchTags(tag *profiledefinition.MetricTagConfig, text string, out map[string]string) error {
	re := r.compiled.Regexp(tag.Match)
	if re == nil {
		return fmt.Errorf("match %q was not compiled", tag.Match)
	}
	match := re.FindStringSubmatchIndex(text)
	if match == nil {
		return fmt.Errorf("match %q did not match %q", tag.Match, text)
	}
	for name, template := range tag.Tags {
		out[name] = string(re.ExpandString(nil, expandTemplate(template), text, match))
	}
	return nil
}

// mergeTags copies src over dst without mutating src. Later sources win, which
// gives row tags precedence over device tags where they collide.
func mergeTags(dst map[string]string, sources ...map[string]string) map[string]string {
	if dst == nil {
		dst = map[string]string{}
	}
	for _, src := range sources {
		for k, v := range src {
			dst[k] = v
		}
	}
	return dst
}

// staticTags parses profile static_tags, which are written as "key:value".
func staticTags(tags []string) map[string]string {
	if len(tags) == 0 {
		return nil
	}
	out := make(map[string]string, len(tags))
	for _, tag := range tags {
		key, value, found := splitTag(tag)
		if !found {
			// A bare static tag has no natural attribute value; record its
			// presence rather than dropping the operator's declaration.
			out[key] = "true"
			continue
		}
		out[key] = value
	}
	return out
}

func splitTag(tag string) (key, value string, found bool) {
	for i := 0; i < len(tag); i++ {
		if tag[i] == ':' {
			return tag[:i], tag[i+1:], true
		}
	}
	return tag, "", false
}
