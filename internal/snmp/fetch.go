package snmp

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/gosnmp/gosnmp"
)

// FetchConfig tunes how a poll is broken into PDUs. It carries mapstructure tags
// so the receiver's `fetch:` block decodes straight into it, as Credentials does
// for the credential fields.
type FetchConfig struct {
	// OIDBatchSize is how many OIDs go into one GET or GETBULK.
	OIDBatchSize int `mapstructure:"oid_batch_size"`
	// BulkMaxRepetitions is GETBULK's max-repetitions.
	BulkMaxRepetitions uint32 `mapstructure:"bulk_max_repetitions"`
	// MaxRowsPerColumn bounds a single column walk. A device with a pathological
	// or looping table would otherwise produce unbounded series; see the
	// cardinality risk in the plan.
	MaxRowsPerColumn int `mapstructure:"max_rows_per_column"`
}

// Defaults mirror the Datadog agent, whose values are tuned against real
// hardware: small GET batches because some agents reject large ones, and 10
// repetitions as a compromise between PDU count and response size.
const (
	DefaultOIDBatchSize       = 10
	DefaultBulkMaxRepetitions = 10
	DefaultMaxRowsPerColumn   = 10000
)

func (c FetchConfig) withDefaults() FetchConfig {
	if c.OIDBatchSize <= 0 {
		c.OIDBatchSize = DefaultOIDBatchSize
	}
	if c.BulkMaxRepetitions == 0 {
		c.BulkMaxRepetitions = DefaultBulkMaxRepetitions
	}
	if c.MaxRowsPerColumn <= 0 {
		c.MaxRowsPerColumn = DefaultMaxRowsPerColumn
	}
	return c
}

// FetchReport records what happened during a poll, for self-telemetry and for
// pruning OIDs the device does not implement.
type FetchReport struct {
	// MissingOIDs are OIDs the device explicitly has no value for. The caller
	// prunes these so they are not requested again.
	MissingOIDs []string
	// PDUs counts request packets sent, the metric that matters for scale.
	PDUs int
	// TruncatedColumns are columns that hit MaxRowsPerColumn.
	TruncatedColumns []string
}

// Fetch collects the given OIDs into a value store. Partial failure is normal
// with SNMP, so it returns whatever it read along with an error describing what
// it could not: the caller reports the metrics it did get.
//
// Cancellation is honoured between requests: an in-flight request still runs to
// its own timeout, but a multi-PDU poll against a dead device stops at the next
// PDU boundary instead of retrying through the whole OID list.
func Fetch(ctx context.Context, sess Session, scalarOIDs, columnOIDs []string, cfg FetchConfig) (*ValueStore, *FetchReport, error) {
	cfg = cfg.withDefaults()
	store := NewValueStore()
	report := &FetchReport{}
	var errs []error

	if len(scalarOIDs) > 0 {
		if err := fetchScalars(ctx, sess, scalarOIDs, cfg, store, report); err != nil {
			errs = append(errs, fmt.Errorf("scalars: %w", err))
		}
	}
	if len(columnOIDs) > 0 {
		if err := fetchColumns(ctx, sess, columnOIDs, cfg, store, report); err != nil {
			errs = append(errs, fmt.Errorf("columns: %w", err))
		}
	}
	return store, report, errors.Join(errs...)
}

// fetchScalars issues batched GETs, shrinking the batch when a device complains
// that the response would be too big.
func fetchScalars(ctx context.Context, sess Session, oids []string, cfg FetchConfig, store *ValueStore, report *FetchReport) error {
	batch := newBatchSizer(cfg.OIDBatchSize)
	remaining := append([]string(nil), oids...)
	var errs []error

	for len(remaining) > 0 {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		size := min(batch.size(), len(remaining))
		chunk := remaining[:size]

		packet, err := sess.Get(prefixed(chunk))
		report.PDUs++
		if err != nil {
			// A transport error is not attributable to a specific OID, so
			// shrinking is the only useful response before giving up.
			if batch.shrink() {
				continue
			}
			errs = append(errs, fmt.Errorf("get %d oids: %w", len(chunk), err))
			remaining = remaining[size:]
			continue
		}

		switch packet.Error {
		case gosnmp.TooBig:
			if batch.shrink() {
				continue
			}
			errs = append(errs, fmt.Errorf("device rejects even a single-OID GET as too big"))
			remaining = remaining[size:]
			continue

		case gosnmp.NoSuchName:
			// SNMPv1 fails the whole request and points at the offending OID.
			// Drop just that OID and retry the rest, otherwise one unsupported
			// OID would suppress every metric in its batch.
			if idx := int(packet.ErrorIndex); idx >= 1 && idx <= len(chunk) {
				bad := chunk[idx-1]
				report.MissingOIDs = append(report.MissingOIDs, bad)
				remaining = append(remaining[:idx-1:idx-1], remaining[idx:]...)
				continue
			}
			errs = append(errs, fmt.Errorf("noSuchName with unusable error index %d", packet.ErrorIndex))
			remaining = remaining[size:]
			continue

		case gosnmp.NoError:
		default:
			errs = append(errs, fmt.Errorf("get returned %s", packet.Error))
			remaining = remaining[size:]
			continue
		}

		for _, pdu := range packet.Variables {
			oid := CanonicalOID(pdu.Name)
			value, ok := decodePDU(pdu)
			if !ok {
				// Only noSuchObject proves the device does not implement the
				// OID. noSuchInstance and Null can be transient -- an agent
				// still populating its tables after a reboot -- and pruning on
				// them would silence the metric until the receiver restarts.
				if pdu.Type == gosnmp.NoSuchObject {
					report.MissingOIDs = append(report.MissingOIDs, oid)
				}
				continue
			}
			store.Scalars[oid] = value
		}
		batch.success()
		remaining = remaining[size:]
	}
	return errors.Join(errs...)
}

// fetchColumns walks each column. Columns are walked together in batches so one
// PDU advances several columns at once, which is what keeps PDU count per device
// low enough to poll thousands of devices.
func fetchColumns(ctx context.Context, sess Session, columnOIDs []string, cfg FetchConfig, store *ValueStore, report *FetchReport) error {
	// next tracks the walk position per column, starting at the column root.
	next := make(map[string]string, len(columnOIDs))
	for _, oid := range columnOIDs {
		c := CanonicalOID(oid)
		next[c] = c
		store.Columns[c] = map[string]ResultValue{}
	}

	batch := newBatchSizer(cfg.OIDBatchSize)
	useBulk := sess.SupportsGetBulk()
	var errs []error

	for len(next) > 0 {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		active := slices.Sorted(maps.Keys(next))
		size := min(batch.size(), len(active))
		chunk := active[:size]

		request := make([]string, 0, len(chunk))
		for _, column := range chunk {
			request = append(request, next[column])
		}

		var packet *gosnmp.SnmpPacket
		var err error
		if useBulk {
			packet, err = sess.GetBulk(prefixed(request), cfg.BulkMaxRepetitions)
		} else {
			packet, err = sess.GetNext(prefixed(request))
		}
		report.PDUs++

		if err != nil || (packet != nil && packet.Error == gosnmp.TooBig) {
			if batch.shrink() {
				continue
			}
			// A device that cannot answer GETBULK at all still usually answers
			// GETNEXT, so fall back once before giving up on it.
			if useBulk {
				useBulk = false
				batch.reset()
				continue
			}
			if err == nil {
				err = fmt.Errorf("device reports tooBig for a single-column walk")
			}
			errs = append(errs, err)
			break
		}
		if packet.Error == gosnmp.NoSuchName {
			// SNMPv1 fails the whole GETNEXT when any varbind has no successor
			// and echoes the request back, so letting consume see it would
			// advance nothing and end every column in the chunk -- silently
			// dropping the other columns' remaining rows. The error index names
			// the column that ran off the end of the MIB; end just that one.
			// The request was built in chunk order, so the index maps directly.
			if idx := int(packet.ErrorIndex); idx >= 1 && idx <= len(chunk) {
				delete(next, chunk[idx-1])
				continue
			}
			errs = append(errs, fmt.Errorf("walk returned noSuchName with unusable error index %d", packet.ErrorIndex))
			break
		}
		if packet.Error != gosnmp.NoError {
			errs = append(errs, fmt.Errorf("walk returned %s", packet.Error))
			break
		}

		if !consume(packet, chunk, next, store, cfg, report) {
			// No column advanced: continuing would spin forever.
			for _, column := range chunk {
				delete(next, column)
			}
		}
		batch.success()
	}
	return errors.Join(errs...)
}

// consume applies one response to the walk state, returning whether any column
// made progress. Results are matched to columns by OID prefix rather than by
// position, because GETBULK interleaves repetitions and devices may omit
// columns that have ended.
func consume(packet *gosnmp.SnmpPacket, chunk []string, next map[string]string,
	store *ValueStore, cfg FetchConfig, report *FetchReport) bool {

	progressed := false
	// Columns not mentioned in this response have no more rows.
	answered := make(map[string]bool, len(chunk))

	for _, pdu := range packet.Variables {
		oid := CanonicalOID(pdu.Name)

		// Attribute to the longest matching column, so a column that is a prefix
		// of another cannot swallow the other's rows.
		column, index, matched := longestMatch(chunk, oid)
		if !matched {
			continue
		}
		position, walking := next[column]
		if !walking {
			// The column ended earlier in this packet (it hit the row cap); a
			// later repetition must not resurrect the walk or re-report the
			// truncation.
			continue
		}
		answered[column] = true

		// The walk must advance strictly, or a device echoing our request would
		// loop forever. Compared numerically: a string compare would treat row
		// 10 as preceding row 9.
		if position != column && CompareOIDs(oid, position) <= 0 {
			continue
		}

		if value, ok := decodePDU(pdu); ok {
			rows := store.Columns[column]
			if len(rows) >= cfg.MaxRowsPerColumn {
				report.TruncatedColumns = append(report.TruncatedColumns, column)
				delete(next, column)
				continue
			}
			rows[index] = value
		}
		next[column] = oid
		progressed = true
	}

	for _, column := range chunk {
		if !answered[column] {
			// Walked past the end of this column's subtree.
			delete(next, column)
		}
	}
	return progressed
}

// longestMatch finds which requested column an answered OID belongs to,
// preferring the longest prefix. Columns are normally siblings, but a profile
// may legitimately request both a subtree and something beneath it.
func longestMatch(columns []string, oid string) (column, index string, ok bool) {
	for _, candidate := range columns {
		idx, within := rowIndex(candidate, oid)
		if !within {
			continue
		}
		if !ok || len(candidate) > len(column) {
			column, index, ok = candidate, idx, true
		}
	}
	return column, index, ok
}

// prefixed returns OIDs in the leading-dot form gosnmp expects on the wire.
func prefixed(oids []string) []string {
	out := make([]string, len(oids))
	for i, oid := range oids {
		out[i] = "." + CanonicalOID(oid)
	}
	return out
}

// batchSizer implements the adaptive batch sizing the Datadog agent uses: shrink
// on a device complaint, then creep back up, so one bad response does not pin a
// device at a tiny batch size for the process lifetime.
type batchSizer struct {
	max     int
	current int
	streak  int
}

// growAfter is how many consecutive good responses justify a larger batch.
const growAfter = 10

func newBatchSizer(max int) *batchSizer {
	if max < 1 {
		max = 1
	}
	return &batchSizer{max: max, current: max}
}

func (b *batchSizer) size() int { return b.current }

// shrink halves the batch, reporting false once it cannot shrink further.
func (b *batchSizer) shrink() bool {
	if b.current <= 1 {
		return false
	}
	b.current = max(1, b.current/2)
	b.streak = 0
	return true
}

func (b *batchSizer) success() {
	if b.current >= b.max {
		return
	}
	b.streak++
	if b.streak >= growAfter {
		b.current = min(b.max, b.current*2)
		b.streak = 0
	}
}

func (b *batchSizer) reset() {
	b.current = b.max
	b.streak = 0
}
