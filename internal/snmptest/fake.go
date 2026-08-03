// Package snmptest provides an in-process fake SNMP agent.
//
// It exists so the fetch strategy -- which is mostly about coping with badly
// behaved devices -- can be tested deterministically, including the failure
// modes real hardware exhibits: tooBig rejections, absent GETBULK support,
// truncated repetitions and timeouts.
package snmptest

import (
	"fmt"
	"sort"

	"github.com/gosnmp/gosnmp"

	"github.com/tsuga-dev/networkdevicereceiver/internal/snmp"
)

// Fake is a scripted SNMP agent implementing snmp.Session.
type Fake struct {
	values map[string]gosnmp.SnmpPDU
	oids   []string // sorted in OID order; rebuilt lazily
	sorted bool

	// TooBigOver makes the agent reject any request with more than this many
	// OIDs, as agents with small response buffers do. Zero disables it.
	TooBigOver int
	// AlwaysTooBig rejects every request, however small -- the pathological case
	// where shrinking cannot help and the poller must give up.
	AlwaysTooBig bool
	// NoGetBulk makes GETBULK fail outright, forcing the GETNEXT fallback.
	NoGetBulk bool
	// MaxRepetitions caps how many repetitions the agent actually returns,
	// regardless of what was asked. Zero means honour the request.
	MaxRepetitions uint32
	// FailNext makes the next N requests return a transport error.
	FailNext int
	// V1 makes the agent behave like SNMPv1: no GETBULK, and an unknown OID
	// fails the whole request with noSuchName rather than a per-OID sentinel.
	V1 bool

	// Counters observed by tests.
	Gets     int
	GetBulks int
	GetNexts int
}

// New returns an empty fake agent.
func New() *Fake {
	return &Fake{values: map[string]gosnmp.SnmpPDU{}}
}

// Set stores a raw PDU at an OID.
func (f *Fake) Set(oid string, pdu gosnmp.SnmpPDU) *Fake {
	oid = snmp.CanonicalOID(oid)
	pdu.Name = "." + oid
	f.values[oid] = pdu
	f.sorted = false
	return f
}

// SetInt stores an Integer.
func (f *Fake) SetInt(oid string, v int) *Fake {
	return f.Set(oid, gosnmp.SnmpPDU{Type: gosnmp.Integer, Value: v})
}

// SetGauge stores a Gauge32.
func (f *Fake) SetGauge(oid string, v uint) *Fake {
	return f.Set(oid, gosnmp.SnmpPDU{Type: gosnmp.Gauge32, Value: v})
}

// SetCounter32 stores a Counter32.
func (f *Fake) SetCounter32(oid string, v uint) *Fake {
	return f.Set(oid, gosnmp.SnmpPDU{Type: gosnmp.Counter32, Value: v})
}

// SetCounter64 stores a Counter64.
func (f *Fake) SetCounter64(oid string, v uint64) *Fake {
	return f.Set(oid, gosnmp.SnmpPDU{Type: gosnmp.Counter64, Value: v})
}

// SetString stores an OctetString.
func (f *Fake) SetString(oid, v string) *Fake {
	return f.Set(oid, gosnmp.SnmpPDU{Type: gosnmp.OctetString, Value: []byte(v)})
}

// SetBytes stores an OctetString from raw octets, for formats such as a packed
// MAC address.
func (f *Fake) SetBytes(oid string, v []byte) *Fake {
	return f.Set(oid, gosnmp.SnmpPDU{Type: gosnmp.OctetString, Value: v})
}

// SetOID stores an ObjectIdentifier, as sysObjectID returns.
func (f *Fake) SetOID(oid, v string) *Fake {
	return f.Set(oid, gosnmp.SnmpPDU{Type: gosnmp.ObjectIdentifier, Value: "." + snmp.CanonicalOID(v)})
}

// SetColumn stores one column of a table: index -> value, as counters.
func (f *Fake) SetColumn(columnOID string, rows map[string]uint64) *Fake {
	for index, v := range rows {
		f.SetCounter64(columnOID+"."+index, v)
	}
	return f
}

// SetColumnStrings stores one column of a table with string values.
func (f *Fake) SetColumnStrings(columnOID string, rows map[string]string) *Fake {
	for index, v := range rows {
		f.SetString(columnOID+"."+index, v)
	}
	return f
}

func (f *Fake) sortedOIDs() []string {
	if !f.sorted {
		f.oids = make([]string, 0, len(f.values))
		for oid := range f.values {
			f.oids = append(f.oids, oid)
		}
		sort.Slice(f.oids, func(i, j int) bool { return snmp.OIDLess(f.oids[i], f.oids[j]) })
		f.sorted = true
	}
	return f.oids
}

// Connect is a no-op.
func (f *Fake) Connect() error { return nil }

// Close is a no-op.
func (f *Fake) Close() error { return nil }

// SupportsGetBulk reports v2c/v3 capability.
func (f *Fake) SupportsGetBulk() bool { return !f.V1 }

func (f *Fake) checkFaults(oidCount int) (*gosnmp.SnmpPacket, error) {
	if f.FailNext > 0 {
		f.FailNext--
		return nil, fmt.Errorf("simulated transport failure")
	}
	if f.AlwaysTooBig || (f.TooBigOver > 0 && oidCount > f.TooBigOver) {
		return &gosnmp.SnmpPacket{Error: gosnmp.TooBig}, nil
	}
	return nil, nil
}

// Get returns exact matches. Unknown OIDs yield NoSuchObject on v2c, or fail the
// whole request with noSuchName on v1.
func (f *Fake) Get(oids []string) (*gosnmp.SnmpPacket, error) {
	f.Gets++
	if packet, err := f.checkFaults(len(oids)); packet != nil || err != nil {
		return packet, err
	}

	out := &gosnmp.SnmpPacket{}
	for i, oid := range oids {
		canonical := snmp.CanonicalOID(oid)
		pdu, ok := f.values[canonical]
		if !ok {
			if f.V1 {
				return &gosnmp.SnmpPacket{
					Error:      gosnmp.NoSuchName,
					ErrorIndex: uint8(i + 1),
				}, nil
			}
			out.Variables = append(out.Variables, gosnmp.SnmpPDU{
				Name: "." + canonical,
				Type: gosnmp.NoSuchObject,
			})
			continue
		}
		out.Variables = append(out.Variables, pdu)
	}
	return out, nil
}

// next returns the first stored OID strictly after the given one.
func (f *Fake) next(after string) (gosnmp.SnmpPDU, bool) {
	for _, oid := range f.sortedOIDs() {
		if snmp.CompareOIDs(oid, after) > 0 {
			return f.values[oid], true
		}
	}
	return gosnmp.SnmpPDU{}, false
}

// GetNext returns the successor of each requested OID.
func (f *Fake) GetNext(oids []string) (*gosnmp.SnmpPacket, error) {
	f.GetNexts++
	if packet, err := f.checkFaults(len(oids)); packet != nil || err != nil {
		return packet, err
	}

	out := &gosnmp.SnmpPacket{}
	for _, oid := range oids {
		pdu, ok := f.next(snmp.CanonicalOID(oid))
		if !ok {
			out.Variables = append(out.Variables, gosnmp.SnmpPDU{
				Name: oid,
				Type: gosnmp.EndOfMibView,
			})
			continue
		}
		out.Variables = append(out.Variables, pdu)
	}
	return out, nil
}

// GetBulk returns up to maxRepetitions successors per requested OID, in
// repetition-major order as a real agent does.
func (f *Fake) GetBulk(oids []string, maxRepetitions uint32) (*gosnmp.SnmpPacket, error) {
	f.GetBulks++
	if f.NoGetBulk {
		return nil, fmt.Errorf("simulated: device does not support GETBULK")
	}
	if packet, err := f.checkFaults(len(oids)); packet != nil || err != nil {
		return packet, err
	}

	reps := maxRepetitions
	if f.MaxRepetitions > 0 && f.MaxRepetitions < reps {
		reps = f.MaxRepetitions
	}

	cursors := make([]string, len(oids))
	for i, oid := range oids {
		cursors[i] = snmp.CanonicalOID(oid)
	}
	exhausted := make([]bool, len(oids))

	out := &gosnmp.SnmpPacket{}
	for r := uint32(0); r < reps; r++ {
		for i := range oids {
			if exhausted[i] {
				continue
			}
			pdu, ok := f.next(cursors[i])
			if !ok {
				exhausted[i] = true
				out.Variables = append(out.Variables, gosnmp.SnmpPDU{
					Name: "." + cursors[i],
					Type: gosnmp.EndOfMibView,
				})
				continue
			}
			out.Variables = append(out.Variables, pdu)
			cursors[i] = snmp.CanonicalOID(pdu.Name)
		}
	}
	return out, nil
}

// Requests is the total number of request packets served.
func (f *Fake) Requests() int { return f.Gets + f.GetBulks + f.GetNexts }

// Interface assertion: a Fake must remain usable wherever a session is.
var _ snmp.Session = (*Fake)(nil)
