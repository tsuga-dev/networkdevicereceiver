package discovery_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tsuga-dev/networkdevicereceiver/internal/discovery"
	"github.com/tsuga-dev/networkdevicereceiver/internal/snmp"
	"github.com/tsuga-dev/networkdevicereceiver/internal/snmptest"
)

func TestExpandCIDR(t *testing.T) {
	tests := []struct {
		cidr      string
		wantCount int
		wantFirst string
		wantLast  string
	}{
		// /24 drops the network and broadcast addresses.
		{"10.0.0.0/24", 254, "10.0.0.1", "10.0.0.254"},
		{"192.168.1.0/30", 2, "192.168.1.1", "192.168.1.2"},
		// /31 is a point-to-point link: both addresses are usable.
		{"192.168.1.0/31", 2, "192.168.1.0", "192.168.1.1"},
		{"192.168.1.5/32", 1, "192.168.1.5", "192.168.1.5"},
	}
	for _, tc := range tests {
		got, err := discovery.ExpandCIDR(tc.cidr, 0)
		if err != nil {
			t.Errorf("ExpandCIDR(%s): %v", tc.cidr, err)
			continue
		}
		if len(got) != tc.wantCount {
			t.Errorf("ExpandCIDR(%s) returned %d addresses, want %d", tc.cidr, len(got), tc.wantCount)
			continue
		}
		if got[0] != tc.wantFirst {
			t.Errorf("%s first = %s, want %s", tc.cidr, got[0], tc.wantFirst)
		}
		if got[len(got)-1] != tc.wantLast {
			t.Errorf("%s last = %s, want %s", tc.cidr, got[len(got)-1], tc.wantLast)
		}
	}
}

// TestExpandCIDRRefusesHugeSubnets checks the guard against a configuration
// mistake that would otherwise launch millions of probes.
func TestExpandCIDRRefusesHugeSubnets(t *testing.T) {
	if _, err := discovery.ExpandCIDR("10.0.0.0/8", 0); err == nil {
		t.Error("expected a /8 to be refused")
	} else if !strings.Contains(err.Error(), "max_hosts") {
		t.Errorf("error should point at the limit: %v", err)
	}
	// The limit is configurable for operators who really mean it.
	if _, err := discovery.ExpandCIDR("10.0.0.0/16", 70000); err != nil {
		t.Errorf("a raised limit should permit a /16: %v", err)
	}
}

func TestExpandCIDRRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"", "10.0.0.1", "not-a-cidr", "10.0.0.0/33"} {
		if _, err := discovery.ExpandCIDR(bad, 0); err == nil {
			t.Errorf("ExpandCIDR(%q) should fail", bad)
		}
	}
}

// fakeFleet answers for a chosen set of addresses, with a chosen community.
type fakeFleet struct {
	mu sync.Mutex
	// live maps address -> the community that works for it.
	live map[string]string
	// probes counts dial attempts per address.
	probes map[string]int
}

func newFleet() *fakeFleet {
	return &fakeFleet{live: map[string]string{}, probes: map[string]int{}}
}

func (f *fakeFleet) add(address, community string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.live[address] = community
}

func (f *fakeFleet) remove(address string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.live, address)
}

func (f *fakeFleet) probeCount(address string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.probes[address]
}

func (f *fakeFleet) dial(cfg snmp.ConnectionConfig) (snmp.Session, error) {
	f.mu.Lock()
	f.probes[cfg.Host]++
	community, alive := f.live[cfg.Host]
	f.mu.Unlock()

	if !alive || community != cfg.Credentials.Community {
		// A device that is absent, or present but rejecting this community,
		// looks the same from the scanner's point of view: silence.
		return snmptest.New(), nil
	}
	dev := snmptest.New()
	dev.SetOID(discovery.OIDSysObjectID, "1.3.6.1.4.1.9.1.1")
	dev.SetString(discovery.OIDSysName, "switch-"+cfg.Host)
	return dev, nil
}

func publicSubnet(network string, communities ...string) discovery.SubnetConfig {
	creds := make([]snmp.Credentials, 0, len(communities))
	for _, c := range communities {
		creds = append(creds, snmp.Credentials{Version: "v2c", Community: c})
	}
	return discovery.SubnetConfig{
		Network:     network,
		Port:        161,
		Credentials: creds,
	}
}

func TestScanFindsLiveDevices(t *testing.T) {
	fleet := newFleet()
	fleet.add("10.0.0.5", "public")
	fleet.add("10.0.0.9", "public")

	scanner := &discovery.Scanner{Workers: 8, Dial: fleet.dial}
	found, err := scanner.Scan(context.Background(), publicSubnet("10.0.0.0/29", "public"))
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	// Only 10.0.0.5 is inside 10.0.0.0/29.
	if len(found) != 1 {
		t.Fatalf("found %d devices, want 1: %+v", len(found), found)
	}
	dev := found[0]
	if dev.Address != "10.0.0.5" {
		t.Errorf("address = %s", dev.Address)
	}
	if dev.SysObjectID != "1.3.6.1.4.1.9.1.1" {
		t.Errorf("sysObjectID = %q", dev.SysObjectID)
	}
	if dev.Subnet != "10.0.0.0/29" {
		t.Errorf("subnet = %q", dev.Subnet)
	}
	if dev.ID == "" {
		t.Error("device needs a stable ID")
	}
}

// TestScanIteratesCredentials covers the multi-credential path and checks the
// index of the set that answered is recorded.
func TestScanIteratesCredentials(t *testing.T) {
	fleet := newFleet()
	fleet.add("10.0.0.1", "secret")

	scanner := &discovery.Scanner{Workers: 4, Dial: fleet.dial}
	found, err := scanner.Scan(context.Background(), publicSubnet("10.0.0.0/30", "public", "secret"))
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 {
		t.Fatalf("found %d devices, want 1", len(found))
	}
	// The second credential set answered.
	if found[0].AuthIndex != 1 {
		t.Errorf("AuthIndex = %d, want 1", found[0].AuthIndex)
	}
}

func TestScanSkipsIgnoredAddresses(t *testing.T) {
	fleet := newFleet()
	fleet.add("10.0.0.1", "public")
	fleet.add("10.0.0.2", "public")

	subnet := publicSubnet("10.0.0.0/29", "public")
	subnet.IgnoredAddresses = []string{"10.0.0.2"}

	scanner := &discovery.Scanner{Workers: 4, Dial: fleet.dial}
	found, err := scanner.Scan(context.Background(), subnet)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].Address != "10.0.0.1" {
		t.Errorf("got %+v, want only 10.0.0.1", found)
	}
	if fleet.probeCount("10.0.0.2") != 0 {
		t.Error("an ignored address must not be probed at all")
	}
}

func TestScanRequiresCredentials(t *testing.T) {
	scanner := &discovery.Scanner{Dial: newFleet().dial}
	if _, err := scanner.Scan(context.Background(), discovery.SubnetConfig{Network: "10.0.0.0/30"}); err == nil {
		t.Error("expected an error with no credentials configured")
	}
}

func TestScanHonoursCancellation(t *testing.T) {
	fleet := newFleet()
	for i := 1; i < 250; i++ {
		fleet.add(fmt.Sprintf("10.0.0.%d", i), "public")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	scanner := &discovery.Scanner{Workers: 4, Dial: fleet.dial}
	_, err := scanner.Scan(ctx, publicSubnet("10.0.0.0/24", "public"))
	if err == nil {
		t.Error("expected the cancelled context to be reported")
	}
}

func TestRegistryLifecycle(t *testing.T) {
	reg := discovery.NewRegistry(3, nil)
	now := time.Now()

	reg.Upsert(discovery.Device{ID: "a", Address: "10.0.0.1", LastSeen: now})
	if reg.Len() != 1 {
		t.Fatalf("Len = %d, want 1", reg.Len())
	}

	// Three failures are tolerated; the fourth drops the device.
	for i := 1; i <= 3; i++ {
		if dropped := reg.MarkFailure("a"); dropped {
			t.Fatalf("dropped after %d failures, want tolerance of 3", i)
		}
	}
	if dropped := reg.MarkFailure("a"); !dropped {
		t.Error("expected the device to be dropped after exceeding allowed failures")
	}
	if reg.Len() != 0 {
		t.Errorf("Len = %d after drop, want 0", reg.Len())
	}
}

// TestRegistrySuccessResetsFailures covers a flapping device: it must not be
// dropped by failures accumulated across unrelated outages.
func TestRegistrySuccessResetsFailures(t *testing.T) {
	reg := discovery.NewRegistry(2, nil)
	reg.Upsert(discovery.Device{ID: "a", Address: "10.0.0.1"})

	reg.MarkFailure("a")
	reg.MarkFailure("a")
	reg.MarkSuccess("a", time.Now())
	reg.MarkFailure("a")
	reg.MarkFailure("a")

	if reg.Len() != 1 {
		t.Error("a device that recovered in between must not be dropped")
	}
	if dropped := reg.MarkFailure("a"); !dropped {
		t.Error("expected a drop once the budget is exhausted again")
	}
}

// TestRegistryNeverDropsStaticDevices pins the rule that an explicitly
// configured device stays configured even when unreachable.
func TestRegistryNeverDropsStaticDevices(t *testing.T) {
	reg := discovery.NewRegistry(1, nil)
	reg.Upsert(discovery.Device{ID: "pinned", Address: "10.9.9.1", Static: true})

	for range 10 {
		if dropped := reg.MarkFailure("pinned"); dropped {
			t.Fatal("a statically configured device must never be dropped")
		}
	}
	if reg.Len() != 1 {
		t.Error("static device disappeared")
	}
}

func TestRegistryUpsertRefreshesWithoutLosingProfile(t *testing.T) {
	reg := discovery.NewRegistry(3, nil)
	reg.Upsert(discovery.Device{ID: "a", Address: "10.0.0.1", AuthIndex: 1})
	reg.SetProfile("a", "1.3.6.1.4.1.9.1.1", "cisco-catalyst")

	// A rediscovery re-reports the device without profile information.
	reg.Upsert(discovery.Device{ID: "a", Address: "10.0.0.1", AuthIndex: 1})

	dev, ok := reg.Get("a")
	if !ok {
		t.Fatal("device lost")
	}
	if dev.ProfileName != "cisco-catalyst" {
		t.Errorf("profile = %q, want it retained across rediscovery", dev.ProfileName)
	}
}

// memoryStore is an in-memory Persistence.
type memoryStore struct {
	mu   sync.Mutex
	data map[string][]byte
}

func newMemoryStore() *memoryStore {
	return &memoryStore{data: map[string][]byte{}}
}

func (m *memoryStore) Get(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.data[key], nil
}

func (m *memoryStore) Set(_ context.Context, key string, value []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = value
	return nil
}

// TestRegistryPersistenceSurvivesRestart is WS3's restart criterion: the device
// set comes back without rescanning.
func TestRegistryPersistenceSurvivesRestart(t *testing.T) {
	store := newMemoryStore()
	ctx := context.Background()

	first := discovery.NewRegistry(3, store)
	first.Upsert(discovery.Device{
		ID: "10.0.0.5|10.0.0.0/24", Address: "10.0.0.5", Subnet: "10.0.0.0/24",
		AuthIndex: 2, SysObjectID: "1.3.6.1.4.1.9.1.1", LastSeen: time.Now(),
	})
	first.SetProfile("10.0.0.5|10.0.0.0/24", "1.3.6.1.4.1.9.1.1", "cisco-catalyst")
	if err := first.Save(ctx); err != nil {
		t.Fatalf("Save: %v", err)
	}

	second := discovery.NewRegistry(3, store)
	n, err := second.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if n != 1 {
		t.Fatalf("loaded %d devices, want 1", n)
	}
	dev, ok := second.Get("10.0.0.5|10.0.0.0/24")
	if !ok {
		t.Fatal("device not restored")
	}
	// The credential index and profile must survive, otherwise a restart pays
	// the full credential-iteration and profile-detection cost again.
	if dev.AuthIndex != 2 {
		t.Errorf("AuthIndex = %d, want 2", dev.AuthIndex)
	}
	if dev.ProfileName != "cisco-catalyst" {
		t.Errorf("profile = %q, want cisco-catalyst", dev.ProfileName)
	}
}

// TestRegistryDoesNotPersistStaticDevices keeps configuration authoritative: a
// device removed from the config must not be resurrected from storage.
func TestRegistryDoesNotPersistStaticDevices(t *testing.T) {
	store := newMemoryStore()
	ctx := context.Background()

	first := discovery.NewRegistry(3, store)
	first.Upsert(discovery.Device{ID: "static", Address: "10.9.9.1", Static: true})
	first.Upsert(discovery.Device{ID: "found", Address: "10.0.0.1", Subnet: "10.0.0.0/24"})
	if err := first.Save(ctx); err != nil {
		t.Fatal(err)
	}

	second := discovery.NewRegistry(3, store)
	if _, err := second.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := second.Get("static"); ok {
		t.Error("a statically configured device must not be restored from storage")
	}
	if _, ok := second.Get("found"); !ok {
		t.Error("a discovered device should be restored")
	}
}

func TestRegistryLoadToleratesEmptyAndCorruptState(t *testing.T) {
	ctx := context.Background()

	empty := discovery.NewRegistry(3, newMemoryStore())
	if n, err := empty.Load(ctx); err != nil || n != 0 {
		t.Errorf("empty store: n=%d err=%v", n, err)
	}

	corrupt := newMemoryStore()
	_ = corrupt.Set(ctx, "devices", []byte("{not json"))
	reg := discovery.NewRegistry(3, corrupt)
	if _, err := reg.Load(ctx); err == nil {
		t.Error("corrupt state should be reported so it can be logged and reset")
	}
}

func TestRegistryNilStoreIsInMemoryOnly(t *testing.T) {
	reg := discovery.NewRegistry(3, nil)
	reg.Upsert(discovery.Device{ID: "a", Address: "10.0.0.1"})
	if err := reg.Save(context.Background()); err != nil {
		t.Errorf("Save with no store should be a no-op: %v", err)
	}
	if n, err := reg.Load(context.Background()); err != nil || n != 0 {
		t.Errorf("Load with no store: n=%d err=%v", n, err)
	}
}

// TestScanThenRegistryConverges is the WS3 end-to-end shape: scan a subnet,
// register what was found, lose a device, and have it age out.
func TestScanThenRegistryConverges(t *testing.T) {
	fleet := newFleet()
	fleet.add("10.0.0.1", "public")
	fleet.add("10.0.0.2", "public")
	fleet.add("10.0.0.3", "public")

	scanner := &discovery.Scanner{Workers: 8, Dial: fleet.dial}
	subnet := publicSubnet("10.0.0.0/28", "public")
	reg := discovery.NewRegistry(2, nil)

	found, err := scanner.Scan(context.Background(), subnet)
	if err != nil {
		t.Fatal(err)
	}
	for _, dev := range found {
		reg.Upsert(dev)
	}
	if reg.Len() != 3 {
		t.Fatalf("registered %d devices, want 3", reg.Len())
	}

	// One device is switched off. Rediscovery no longer sees it, and it ages out
	// only after the failure budget is spent.
	fleet.remove("10.0.0.2")
	lost := discovery.DeviceID("10.0.0.2", subnet.Network)

	for range 2 {
		found, err = scanner.Scan(context.Background(), subnet)
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, dev := range found {
			reg.Upsert(dev)
			seen[dev.ID] = true
		}
		for _, dev := range reg.Devices() {
			if !seen[dev.ID] {
				reg.MarkFailure(dev.ID)
			}
		}
		if _, still := reg.Get(lost); !still {
			t.Fatal("device dropped too eagerly")
		}
	}

	reg.MarkFailure(lost)
	if _, still := reg.Get(lost); still {
		t.Error("expected the absent device to be dropped once its budget was spent")
	}

	// A new device appears and is picked up by the next scan.
	fleet.add("10.0.0.7", "public")
	found, err = scanner.Scan(context.Background(), subnet)
	if err != nil {
		t.Fatal(err)
	}
	var addresses []string
	for _, dev := range found {
		addresses = append(addresses, dev.Address)
	}
	if !slices.Contains(addresses, "10.0.0.7") {
		t.Errorf("new device not discovered; found %v", addresses)
	}
}
