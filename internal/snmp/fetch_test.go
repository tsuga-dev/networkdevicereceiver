package snmp_test

import (
	"fmt"
	"slices"
	"testing"

	"github.com/gosnmp/gosnmp"

	"github.com/tsuga-dev/networkdevicereceiver/internal/snmp"
	"github.com/tsuga-dev/networkdevicereceiver/internal/snmptest"
)

const (
	sysUpTime     = "1.3.6.1.2.1.1.3.0"
	sysName       = "1.3.6.1.2.1.1.5.0"
	sysObjectID   = "1.3.6.1.2.1.1.2.0"
	ifInOctets    = "1.3.6.1.2.1.2.2.1.10"
	ifOutOctets   = "1.3.6.1.2.1.2.2.1.16"
	ifName        = "1.3.6.1.2.1.31.1.1.1.1"
	ifOperStatus  = "1.3.6.1.2.1.2.2.1.8"
	unsupportedOI = "1.3.6.1.4.1.99999.1.0"
)

func TestFetchScalars(t *testing.T) {
	dev := snmptest.New().
		SetInt(sysUpTime, 12345).
		SetString(sysName, "switch-1").
		SetOID(sysObjectID, "1.3.6.1.4.1.9.1.1")

	store, report, err := snmp.Fetch(dev, []string{sysUpTime, sysName, sysObjectID}, nil, snmp.FetchConfig{})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(report.MissingOIDs) != 0 {
		t.Errorf("unexpected missing OIDs: %v", report.MissingOIDs)
	}

	uptime, err := store.Scalar(sysUpTime)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := uptime.Float(); err != nil || got != 12345 {
		t.Errorf("sysUpTime = %v (%v), want 12345", got, err)
	}

	name, err := store.Scalar(sysName)
	if err != nil {
		t.Fatal(err)
	}
	if got := name.String(); got != "switch-1" {
		t.Errorf("sysName = %q, want switch-1", got)
	}

	oid, err := store.Scalar(sysObjectID)
	if err != nil {
		t.Fatal(err)
	}
	// The leading dot a device sends must be normalised away, or profile
	// matching against undotted patterns fails for every device.
	if got := oid.String(); got != "1.3.6.1.4.1.9.1.1" {
		t.Errorf("sysObjectID = %q, want undotted", got)
	}
}

func TestFetchScalarMissingIsReportedNotFatal(t *testing.T) {
	dev := snmptest.New().SetInt(sysUpTime, 1)

	store, report, err := snmp.Fetch(dev, []string{sysUpTime, unsupportedOI}, nil, snmp.FetchConfig{})
	if err != nil {
		t.Fatalf("an unsupported OID must not fail the poll: %v", err)
	}
	if !slices.Contains(report.MissingOIDs, unsupportedOI) {
		t.Errorf("MissingOIDs = %v, want it to contain %s", report.MissingOIDs, unsupportedOI)
	}
	// The supported OID must still have been collected.
	if _, err := store.Scalar(sysUpTime); err != nil {
		t.Errorf("supported OID was lost alongside the unsupported one: %v", err)
	}
}

// TestFetchScalarV1NoSuchName covers SNMPv1, where one unsupported OID fails the
// entire request. Without per-OID recovery, a single bad OID would suppress
// every scalar metric on the device.
func TestFetchScalarV1NoSuchName(t *testing.T) {
	dev := snmptest.New().SetInt(sysUpTime, 1).SetString(sysName, "sw")
	dev.V1 = true

	store, report, err := snmp.Fetch(dev, []string{sysUpTime, unsupportedOI, sysName}, nil, snmp.FetchConfig{})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !slices.Contains(report.MissingOIDs, unsupportedOI) {
		t.Errorf("MissingOIDs = %v, want %s", report.MissingOIDs, unsupportedOI)
	}
	for _, oid := range []string{sysUpTime, sysName} {
		if _, err := store.Scalar(oid); err != nil {
			t.Errorf("v1 recovery lost %s: %v", oid, err)
		}
	}
}

func TestFetchScalarsShrinkOnTooBig(t *testing.T) {
	dev := snmptest.New()
	var oids []string
	for i := 1; i <= 8; i++ {
		oid := fmt.Sprintf("1.3.6.1.2.1.1.%d.0", i)
		dev.SetInt(oid, i)
		oids = append(oids, oid)
	}
	// The device only tolerates two OIDs per GET.
	dev.TooBigOver = 2

	store, _, err := snmp.Fetch(dev, oids, nil, snmp.FetchConfig{OIDBatchSize: 8})
	if err != nil {
		t.Fatalf("Fetch should recover by shrinking: %v", err)
	}
	for _, oid := range oids {
		if _, err := store.Scalar(oid); err != nil {
			t.Errorf("missing %s after shrink recovery: %v", oid, err)
		}
	}
}

// TestFetchScalarsGivesUpWhenSingleOIDTooBig checks the poller terminates
// instead of shrinking forever against a device that rejects everything.
func TestFetchScalarsGivesUpWhenSingleOIDTooBig(t *testing.T) {
	dev := snmptest.New().SetInt(sysUpTime, 1).SetString(sysName, "sw")
	dev.AlwaysTooBig = true

	_, _, err := snmp.Fetch(dev, []string{sysUpTime, sysName}, nil, snmp.FetchConfig{OIDBatchSize: 4})
	if err == nil {
		t.Fatal("expected an error when even a single-OID GET is rejected")
	}
}

func TestFetchColumnsGivesUpWhenAlwaysTooBig(t *testing.T) {
	dev := snmptest.New()
	dev.SetCounter64(ifInOctets+".1", 1)
	dev.AlwaysTooBig = true

	if _, _, err := snmp.Fetch(dev, nil, []string{ifInOctets}, snmp.FetchConfig{}); err == nil {
		t.Fatal("expected an error when a column walk is always rejected")
	}
}

// TestFetchColumnsManyRows is the regression test for OID ordering: with ten or
// more rows, a lexicographic comparison decides row 10 precedes row 9 and the
// walk terminates early.
func TestFetchColumnsManyRows(t *testing.T) {
	const rows = 25
	dev := snmptest.New()
	want := map[string]uint64{}
	for i := 1; i <= rows; i++ {
		index := fmt.Sprint(i)
		want[index] = uint64(i * 1000)
		dev.SetCounter64(ifInOctets+"."+index, uint64(i*1000))
	}

	store, report, err := snmp.Fetch(dev, nil, []string{ifInOctets}, snmp.FetchConfig{})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	col, err := store.Column(ifInOctets)
	if err != nil {
		t.Fatal(err)
	}
	if len(col) != rows {
		t.Fatalf("collected %d rows, want %d -- walk stopped early", len(col), rows)
	}
	for index, expected := range want {
		got, err := col[index].Float()
		if err != nil {
			t.Errorf("row %s: %v", index, err)
			continue
		}
		if got != float64(expected) {
			t.Errorf("row %s = %v, want %v", index, got, expected)
		}
	}
	t.Logf("%d rows in %d PDUs", rows, report.PDUs)
}

func TestFetchColumnsMultipleColumnsAndTypes(t *testing.T) {
	dev := snmptest.New()
	for i := 1; i <= 3; i++ {
		index := fmt.Sprint(i)
		dev.SetCounter64(ifInOctets+"."+index, uint64(100*i))
		dev.SetCounter64(ifOutOctets+"."+index, uint64(200*i))
		dev.SetString(ifName+"."+index, "Gi0/"+index)
		dev.SetInt(ifOperStatus+"."+index, 1)
	}

	store, _, err := snmp.Fetch(dev, nil,
		[]string{ifInOctets, ifOutOctets, ifName, ifOperStatus}, snmp.FetchConfig{})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	for _, oid := range []string{ifInOctets, ifOutOctets, ifName, ifOperStatus} {
		col, err := store.Column(oid)
		if err != nil {
			t.Fatalf("column %s: %v", oid, err)
		}
		if len(col) != 3 {
			t.Errorf("column %s has %d rows, want 3", oid, len(col))
		}
	}

	name, err := store.ColumnValue(ifName, "2")
	if err != nil {
		t.Fatal(err)
	}
	if got := name.String(); got != "Gi0/2" {
		t.Errorf("ifName.2 = %q, want Gi0/2", got)
	}

	octets, err := store.ColumnValue(ifInOctets, "3")
	if err != nil {
		t.Fatal(err)
	}
	if !octets.IsCounter() {
		t.Error("Counter64 should be reported as a counter so the reporter can infer a Sum")
	}
}

// TestFetchColumnsWalkStaysInSubtree guards against collecting neighbouring
// columns: ifInOctets and ifOutOctets are siblings, and a walk that ignores the
// subtree boundary would attribute one to the other.
func TestFetchColumnsWalkStaysInSubtree(t *testing.T) {
	dev := snmptest.New()
	dev.SetCounter64(ifInOctets+".1", 111)
	dev.SetCounter64(ifOutOctets+".1", 999)

	store, _, err := snmp.Fetch(dev, nil, []string{ifInOctets}, snmp.FetchConfig{})
	if err != nil {
		t.Fatal(err)
	}
	col, err := store.Column(ifInOctets)
	if err != nil {
		t.Fatal(err)
	}
	if len(col) != 1 {
		t.Fatalf("collected %d rows, want only the one in-subtree row: %v", len(col), col)
	}
	if got, _ := col["1"].Float(); got != 111 {
		t.Errorf("value = %v, want 111 (not the sibling column's 999)", got)
	}
}

// TestFetchColumnsGetNextFallback covers devices whose GETBULK is broken, which
// the plan calls out as a real-hardware failure mode.
func TestFetchColumnsGetNextFallback(t *testing.T) {
	dev := snmptest.New()
	for i := 1; i <= 5; i++ {
		dev.SetCounter64(ifInOctets+"."+fmt.Sprint(i), uint64(i))
	}
	dev.NoGetBulk = true

	store, _, err := snmp.Fetch(dev, nil, []string{ifInOctets}, snmp.FetchConfig{})
	if err != nil {
		t.Fatalf("Fetch should fall back to GETNEXT: %v", err)
	}
	col, err := store.Column(ifInOctets)
	if err != nil {
		t.Fatal(err)
	}
	if len(col) != 5 {
		t.Errorf("collected %d rows via GETNEXT, want 5", len(col))
	}
	if dev.GetNexts == 0 {
		t.Error("expected the GETNEXT fallback to be used")
	}
}

func TestFetchColumnsV1UsesGetNext(t *testing.T) {
	dev := snmptest.New()
	for i := 1; i <= 3; i++ {
		dev.SetCounter32(ifInOctets+"."+fmt.Sprint(i), uint(i))
	}
	dev.V1 = true

	_, _, err := snmp.Fetch(dev, nil, []string{ifInOctets}, snmp.FetchConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if dev.GetBulks != 0 {
		t.Error("v1 must never issue GETBULK")
	}
	if dev.GetNexts == 0 {
		t.Error("v1 should walk with GETNEXT")
	}
}

func TestFetchColumnsTruncatesRunawayTable(t *testing.T) {
	dev := snmptest.New()
	for i := 1; i <= 50; i++ {
		dev.SetCounter64(ifInOctets+"."+fmt.Sprint(i), uint64(i))
	}

	store, report, err := snmp.Fetch(dev, nil, []string{ifInOctets},
		snmp.FetchConfig{MaxRowsPerColumn: 10})
	if err != nil {
		t.Fatal(err)
	}
	col, _ := store.Column(ifInOctets)
	if len(col) > 10 {
		t.Errorf("collected %d rows, want the limit of 10 enforced", len(col))
	}
	if !slices.Contains(report.TruncatedColumns, ifInOctets) {
		t.Error("truncation must be reported, not silent")
	}
}

func TestFetchColumnsEmptyTable(t *testing.T) {
	dev := snmptest.New().SetInt(sysUpTime, 1)

	store, _, err := snmp.Fetch(dev, nil, []string{ifInOctets}, snmp.FetchConfig{})
	if err != nil {
		t.Fatalf("an absent table is not an error: %v", err)
	}
	col, err := store.Column(ifInOctets)
	if err != nil {
		t.Fatalf("column should exist but be empty: %v", err)
	}
	if len(col) != 0 {
		t.Errorf("got %d rows for an absent table", len(col))
	}
}

func TestFetchTransportFailureIsReported(t *testing.T) {
	dev := snmptest.New().SetInt(sysUpTime, 1)
	dev.FailNext = 100

	_, _, err := snmp.Fetch(dev, []string{sysUpTime}, nil, snmp.FetchConfig{OIDBatchSize: 1})
	if err == nil {
		t.Error("a persistent transport failure must surface")
	}
}

func TestFetchBulkRepetitionsReduceePDUs(t *testing.T) {
	build := func() *snmptest.Fake {
		dev := snmptest.New()
		for i := 1; i <= 40; i++ {
			dev.SetCounter64(ifInOctets+"."+fmt.Sprint(i), uint64(i))
		}
		return dev
	}

	small := build()
	_, reportSmall, err := snmp.Fetch(small, nil, []string{ifInOctets},
		snmp.FetchConfig{BulkMaxRepetitions: 2})
	if err != nil {
		t.Fatal(err)
	}
	large := build()
	_, reportLarge, err := snmp.Fetch(large, nil, []string{ifInOctets},
		snmp.FetchConfig{BulkMaxRepetitions: 20})
	if err != nil {
		t.Fatal(err)
	}

	if reportLarge.PDUs >= reportSmall.PDUs {
		t.Errorf("more repetitions should mean fewer PDUs: %d with 20 vs %d with 2",
			reportLarge.PDUs, reportSmall.PDUs)
	}
	t.Logf("40 rows: %d PDUs at 2 reps, %d PDUs at 20 reps", reportSmall.PDUs, reportLarge.PDUs)
}

func TestDecodeCounter64FromOctets(t *testing.T) {
	// Some agents answer a Counter64 as eight raw octets.
	dev := snmptest.New().Set("1.2.3.4.0", gosnmp.SnmpPDU{
		Type:  gosnmp.Counter64,
		Value: []byte{0, 0, 0, 0, 0, 0, 1, 0},
	})
	store, _, err := snmp.Fetch(dev, []string{"1.2.3.4.0"}, nil, snmp.FetchConfig{})
	if err != nil {
		t.Fatal(err)
	}
	v, err := store.Scalar("1.2.3.4.0")
	if err != nil {
		t.Fatal(err)
	}
	got, err := v.Float()
	if err != nil {
		t.Fatal(err)
	}
	if got != 256 {
		t.Errorf("decoded %v, want 256", got)
	}
}
