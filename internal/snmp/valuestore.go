// Package snmp implements the collection engine: sessions, batched fetching
// with GETBULK/GETNEXT fallback, adaptive batch sizing, and the value store
// that decouples fetching from reporting.
package snmp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/gosnmp/gosnmp"
)

// ResultValue is one value read from a device, retaining the SNMP type so the
// reporting stage can infer an instrument when the profile does not state one.
type ResultValue struct {
	// Value is float64 for numeric types, string for OIDs and addresses, and
	// []byte for octet strings (which may be text or packed binary such as a
	// MAC address).
	Value any
	// Type is the ASN.1 tag the device answered with.
	Type gosnmp.Asn1BER
}

// ErrNotFound reports an OID absent from the store.
var ErrNotFound = errors.New("oid not found")

// IsCounter reports whether the device typed this value as a counter, which
// implies a monotonically increasing total rather than a point-in-time reading.
func (v ResultValue) IsCounter() bool {
	return v.Type == gosnmp.Counter32 || v.Type == gosnmp.Counter64
}

// Float returns the value as a float64, parsing strings where possible. Devices
// not infrequently return a number as an octet string.
func (v ResultValue) Float() (float64, error) {
	switch val := v.Value.(type) {
	case float64:
		return val, nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(val), 64)
		if err != nil {
			return 0, fmt.Errorf("value %q is not numeric: %w", val, err)
		}
		return f, nil
	case []byte:
		f, err := strconv.ParseFloat(strings.TrimSpace(string(val)), 64)
		if err != nil {
			return 0, fmt.Errorf("value %q is not numeric: %w", val, err)
		}
		return f, nil
	default:
		return 0, fmt.Errorf("value of type %T is not numeric", v.Value)
	}
}

// String returns the value as text. Octet strings are returned verbatim; use
// Bytes when a format such as mac_address needs the raw octets.
func (v ResultValue) String() string {
	switch val := v.Value.(type) {
	case string:
		return val
	case []byte:
		return string(val)
	case float64:
		// Avoid scientific notation and a trailing ".0" on integral values,
		// since these strings become metric attribute values.
		if val == math.Trunc(val) && math.Abs(val) < 1e15 {
			return strconv.FormatInt(int64(val), 10)
		}
		return strconv.FormatFloat(val, 'f', -1, 64)
	default:
		return fmt.Sprint(v.Value)
	}
}

// Bytes returns the raw octets when the value is an octet string.
func (v ResultValue) Bytes() ([]byte, bool) {
	b, ok := v.Value.([]byte)
	return b, ok
}

// ScalarValues maps a canonical OID to its value.
type ScalarValues map[string]ResultValue

// ColumnValues maps a column OID to that column's values keyed by row index.
type ColumnValues map[string]map[string]ResultValue

// ValueStore holds one poll's results, indexed by OID and decoupled from any
// profile, so the reporting stage can be tested without a device.
type ValueStore struct {
	Scalars ScalarValues
	Columns ColumnValues
}

// NewValueStore returns an empty store.
func NewValueStore() *ValueStore {
	return &ValueStore{Scalars: ScalarValues{}, Columns: ColumnValues{}}
}

// Count returns how many values were collected, across scalars and table rows.
//
// Zero means the device answered nothing at all, which is how an unreachable or
// wholly unsupported device is distinguished from one that merely omitted some
// OIDs. Absent OIDs are not errors, so without this a silent device would look
// like a successful poll that happened to produce no metrics.
func (s *ValueStore) Count() int {
	total := len(s.Scalars)
	for _, rows := range s.Columns {
		total += len(rows)
	}
	return total
}

// Scalar returns a scalar value.
func (s *ValueStore) Scalar(oid string) (ResultValue, error) {
	v, ok := s.Scalars[CanonicalOID(oid)]
	if !ok {
		return ResultValue{}, fmt.Errorf("%w: scalar %s", ErrNotFound, oid)
	}
	return v, nil
}

// Column returns a column's values keyed by row index.
func (s *ValueStore) Column(oid string) (map[string]ResultValue, error) {
	v, ok := s.Columns[CanonicalOID(oid)]
	if !ok {
		return nil, fmt.Errorf("%w: column %s", ErrNotFound, oid)
	}
	return v, nil
}

// ColumnValue returns one cell of a table.
func (s *ValueStore) ColumnValue(oid, index string) (ResultValue, error) {
	col, err := s.Column(oid)
	if err != nil {
		return ResultValue{}, err
	}
	v, ok := col[index]
	if !ok {
		return ResultValue{}, fmt.Errorf("%w: column %s index %s", ErrNotFound, oid, index)
	}
	return v, nil
}

// CanonicalOID strips a leading dot. gosnmp returns OIDs dotted, profiles write
// them undotted; one spelling is used internally.
func CanonicalOID(oid string) string {
	return strings.TrimPrefix(strings.TrimSpace(oid), ".")
}

// rowIndex returns the part of oid below column, or false if oid is not within
// the column's subtree -- which is how a walk detects it has run past the end.
func rowIndex(column, oid string) (string, bool) {
	column, oid = CanonicalOID(column), CanonicalOID(oid)
	if !strings.HasPrefix(oid, column+".") {
		return "", false
	}
	return oid[len(column)+1:], true
}

// decodePDU converts a gosnmp variable into a ResultValue. It reports false for
// the three "no value here" sentinels, which callers treat as an absent OID
// rather than an error.
func decodePDU(pdu gosnmp.SnmpPDU) (ResultValue, bool) {
	switch pdu.Type {
	case gosnmp.NoSuchObject, gosnmp.NoSuchInstance, gosnmp.EndOfMibView, gosnmp.Null:
		return ResultValue{}, false

	case gosnmp.OctetString, gosnmp.BitString:
		b, ok := pdu.Value.([]byte)
		if !ok {
			return ResultValue{Value: fmt.Sprint(pdu.Value), Type: pdu.Type}, true
		}
		return ResultValue{Value: b, Type: pdu.Type}, true

	case gosnmp.ObjectIdentifier:
		s, _ := pdu.Value.(string)
		return ResultValue{Value: CanonicalOID(s), Type: pdu.Type}, true

	case gosnmp.IPAddress:
		s, ok := pdu.Value.(string)
		if !ok {
			return ResultValue{}, false
		}
		return ResultValue{Value: s, Type: pdu.Type}, true

	case gosnmp.OpaqueFloat:
		f, ok := pdu.Value.(float32)
		if !ok {
			return ResultValue{}, false
		}
		return ResultValue{Value: float64(f), Type: pdu.Type}, true

	case gosnmp.OpaqueDouble:
		f, ok := pdu.Value.(float64)
		if !ok {
			return ResultValue{}, false
		}
		return ResultValue{Value: f, Type: pdu.Type}, true

	default:
		// Integer, Counter32, Counter64, Gauge32, TimeTicks, Uinteger32.
		return decodeNumeric(pdu)
	}
}

func decodeNumeric(pdu gosnmp.SnmpPDU) (ResultValue, bool) {
	switch val := pdu.Value.(type) {
	case int:
		return ResultValue{Value: float64(val), Type: pdu.Type}, true
	case int64:
		return ResultValue{Value: float64(val), Type: pdu.Type}, true
	case uint:
		return ResultValue{Value: float64(val), Type: pdu.Type}, true
	case uint64:
		return ResultValue{Value: float64(val), Type: pdu.Type}, true
	case uint32:
		return ResultValue{Value: float64(val), Type: pdu.Type}, true
	case float64:
		return ResultValue{Value: val, Type: pdu.Type}, true
	case []byte:
		// Some agents return a Counter64 as 8 raw octets.
		if len(val) == 8 {
			return ResultValue{Value: float64(binary.BigEndian.Uint64(val)), Type: pdu.Type}, true
		}
		return ResultValue{}, false
	case nil:
		return ResultValue{}, false
	default:
		return ResultValue{}, false
	}
}
