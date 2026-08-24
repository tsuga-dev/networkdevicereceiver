package networkdevicereceiver

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosnmp/gosnmp"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/extension/xextension/storage"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/receiver/receivertest"

	"github.com/tsuga-dev/networkdevicereceiver/internal/discovery"
	"github.com/tsuga-dev/networkdevicereceiver/internal/snmp"
	"github.com/tsuga-dev/networkdevicereceiver/internal/snmptest"
)

// IF-MIB OIDs the fake fleet answers.
const (
	oidSysDescr     = "1.3.6.1.2.1.1.1.0"
	oidIfName       = "1.3.6.1.2.1.31.1.1.1.1"
	oidIfHCInOctets = "1.3.6.1.2.1.31.1.1.1.6"
	oidIfHighSpeed  = "1.3.6.1.2.1.31.1.1.1.15"
	oidIfOperStatus = "1.3.6.1.2.1.2.2.1.8"
	oidIfAdmin      = "1.3.6.1.2.1.2.2.1.7"

	// A Cisco sysObjectID, so a real vendor profile is selected rather than the
	// generic fallback.
	ciscoSysObjectID = "1.3.6.1.4.1.9.1.1"
)

// fleet is a set of addresses that answer SNMP.
type fleet struct {
	mu        sync.Mutex
	live      map[string]bool
	community string
	dials     int
}

func newFleet(community string, addresses ...string) *fleet {
	f := &fleet{live: map[string]bool{}, community: community}
	for _, address := range addresses {
		f.live[address] = true
	}
	return f
}

func (f *fleet) kill(address string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.live[address] = false
}

func (f *fleet) dial(cfg snmp.ConnectionConfig) (snmp.Session, error) {
	f.mu.Lock()
	f.dials++
	alive := f.live[cfg.Host]
	f.mu.Unlock()

	dev := snmptest.New()
	if !alive || cfg.Credentials.Community != f.community {
		return dev, nil
	}

	dev.SetOID(discovery.OIDSysObjectID, ciscoSysObjectID)
	dev.SetString(discovery.OIDSysName, "switch-"+cfg.Host)
	dev.SetString(oidSysDescr, "Test Switch, Version 15.2")

	for _, index := range []string{"1", "2"} {
		dev.SetString(oidIfName+"."+index, "Gi0/"+index)
		dev.SetCounter64(oidIfHCInOctets+"."+index, 1_000_000)
		dev.SetGauge(oidIfHighSpeed+"."+index, 1000)
		dev.SetInt(oidIfOperStatus+"."+index, 1)
		dev.SetInt(oidIfAdmin+"."+index, 1)
	}
	return dev, nil
}

// startReceiver wires a receiver with an injected dialer.
func startReceiver(t *testing.T, cfg *Config, dial discovery.Dialer) (*snmpReceiver, *consumertest.MetricsSink) {
	t.Helper()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config invalid: %v", err)
	}

	sink := new(consumertest.MetricsSink)
	r, err := newReceiver(cfg, receivertest.NewNopSettings(componentType), sink)
	if err != nil {
		t.Fatalf("newReceiver: %v", err)
	}
	r.dial = dial

	if err := r.Start(context.Background(), componenttest.NewNopHost()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if err := r.Shutdown(context.Background()); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	return r, sink
}

func subnetTestConfig(network, community string) *Config {
	cfg := createDefaultConfig().(*Config)
	cfg.CollectionInterval = 100 * time.Millisecond
	cfg.Timeout = 100 * time.Millisecond
	cfg.Pollers = 8
	cfg.Discovery.Workers = 8
	cfg.Discovery.RediscoveryInterval = 200 * time.Millisecond
	cfg.Subnets = []SubnetConfig{{
		Network:     network,
		Credentials: snmp.Credentials{Version: "v2c", Community: community},
	}}
	return cfg
}

// waitFor polls a condition, which is more reliable than a fixed sleep.
func waitFor(t *testing.T, timeout time.Duration, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestReceiverDiscoversAndPolls is WS5's exit criterion: subnet in, OTLP out.
func TestReceiverDiscoversAndPolls(t *testing.T) {
	f := newFleet("public", "10.0.0.1", "10.0.0.2")
	r, sink := startReceiver(t, subnetTestConfig("10.0.0.0/29", "public"), f.dial)

	waitFor(t, 5*time.Second, "devices to be discovered", func() bool {
		return r.devices.Len() == 2
	})
	waitFor(t, 5*time.Second, "metrics to arrive", func() bool {
		return len(sink.AllMetrics()) >= 2
	})

	// Each batch is one device, so one resource per batch.
	var sawCisco bool
	for _, md := range sink.AllMetrics() {
		if md.ResourceMetrics().Len() != 1 {
			t.Errorf("batch has %d resources, want 1 per device", md.ResourceMetrics().Len())
		}
		res := md.ResourceMetrics().At(0).Resource().Attributes()
		profileAttr, ok := res.Get("snmp.profile")
		if !ok {
			t.Fatal("resource is missing snmp.profile")
		}
		if strings.HasPrefix(profileAttr.Str(), "cisco") {
			sawCisco = true
		}
		if _, ok := res.Get("host.name"); !ok {
			t.Error("resource is missing host.name")
		}
	}
	if !sawCisco {
		t.Error("expected a cisco profile to be selected from the sysObjectID")
	}

	// The interface metrics must actually be present.
	names := map[string]bool{}
	for _, md := range sink.AllMetrics() {
		for i := 0; i < md.ResourceMetrics().Len(); i++ {
			sms := md.ResourceMetrics().At(i).ScopeMetrics()
			for j := 0; j < sms.Len(); j++ {
				ms := sms.At(j).Metrics()
				for k := 0; k < ms.Len(); k++ {
					names[ms.At(k).Name()] = true
				}
			}
		}
	}
	for _, want := range []string{"hw.network.io", "hw.network.up", "hw.network.bandwidth.limit"} {
		if !names[want] {
			t.Errorf("%s was never emitted; got %v", want, keysOf(names))
		}
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestReceiverPollsStaticDevice covers the no-discovery path, including an
// explicitly pinned profile.
func TestReceiverPollsStaticDevice(t *testing.T) {
	f := newFleet("public", "10.9.9.1")

	cfg := createDefaultConfig().(*Config)
	cfg.CollectionInterval = 100 * time.Millisecond
	cfg.Timeout = 100 * time.Millisecond
	cfg.Devices = []DeviceConfig{{
		Endpoint:    "udp://10.9.9.1:161",
		Credentials: snmp.Credentials{Version: "v2c", Community: "public"},
		Profile:     "cisco-catalyst",
	}}

	r, sink := startReceiver(t, cfg, f.dial)
	if r.devices.Len() != 1 {
		t.Fatalf("static device not registered: %d", r.devices.Len())
	}
	waitFor(t, 5*time.Second, "metrics from the static device", func() bool {
		return len(sink.AllMetrics()) >= 1
	})

	md := sink.AllMetrics()[0]
	profileAttr, _ := md.ResourceMetrics().At(0).Resource().Attributes().Get("snmp.profile")
	if profileAttr.Str() != "cisco-catalyst" {
		t.Errorf("profile = %q, want the pinned cisco-catalyst", profileAttr.Str())
	}
}

// TestReceiverDetectsProfileForStaticDeviceWithoutPin checks sysObjectID
// detection happens on the poll path too, not only during discovery.
func TestReceiverDetectsProfileForStaticDeviceWithoutPin(t *testing.T) {
	f := newFleet("public", "10.9.9.2")

	cfg := createDefaultConfig().(*Config)
	cfg.CollectionInterval = 100 * time.Millisecond
	cfg.Timeout = 100 * time.Millisecond
	cfg.Devices = []DeviceConfig{{
		Endpoint:    "10.9.9.2",
		Credentials: snmp.Credentials{Version: "v2c", Community: "public"},
	}}

	_, sink := startReceiver(t, cfg, f.dial)
	waitFor(t, 5*time.Second, "metrics with a detected profile", func() bool {
		return len(sink.AllMetrics()) >= 1
	})

	attrs := sink.AllMetrics()[0].ResourceMetrics().At(0).Resource().Attributes()
	profileAttr, ok := attrs.Get("snmp.profile")
	if !ok || profileAttr.Str() == "" {
		t.Fatal("no profile was detected for the unpinned static device")
	}
	if !strings.HasPrefix(profileAttr.Str(), "cisco") {
		t.Errorf("profile = %q, want one matching the cisco sysObjectID", profileAttr.Str())
	}
}

// TestReceiverDropsDeadDeviceAfterFailures covers the lifecycle: a device that
// stops answering is dropped once its failure budget is spent.
func TestReceiverDropsDeadDeviceAfterFailures(t *testing.T) {
	f := newFleet("public", "10.0.0.1", "10.0.0.2")

	cfg := subnetTestConfig("10.0.0.0/29", "public")
	cfg.Discovery.AllowedFailures = 1
	// Disable rediscovery so the dead device is not simply re-added.
	cfg.Discovery.RediscoveryInterval = time.Hour

	r, _ := startReceiver(t, cfg, f.dial)
	waitFor(t, 5*time.Second, "both devices to be discovered", func() bool {
		return r.devices.Len() == 2
	})

	f.kill("10.0.0.2")
	waitFor(t, 10*time.Second, "the dead device to be dropped", func() bool {
		return r.devices.Len() == 1
	})

	remaining := r.devices.Devices()
	if len(remaining) != 1 || remaining[0].Address != "10.0.0.1" {
		t.Errorf("remaining devices = %+v, want only 10.0.0.1", remaining)
	}
}

// TestReceiverPicksUpNewDeviceOnRediscovery covers the other half of the
// lifecycle.
func TestReceiverPicksUpNewDeviceOnRediscovery(t *testing.T) {
	f := newFleet("public", "10.0.0.1")
	r, _ := startReceiver(t, subnetTestConfig("10.0.0.0/29", "public"), f.dial)

	waitFor(t, 5*time.Second, "the first device", func() bool {
		return r.devices.Len() == 1
	})

	f.mu.Lock()
	f.live["10.0.0.3"] = true
	f.mu.Unlock()

	waitFor(t, 10*time.Second, "the new device to be discovered", func() bool {
		return r.devices.Len() == 2
	})
}

// TestReceiverSurvivesRestartWithStorage is WS3's restart criterion at receiver
// level: with a storage extension the device set is not rescanned from cold.
//
// The two receivers share one backing store, standing in for a process restart.
func TestReceiverSurvivesRestartWithStorage(t *testing.T) {
	f := newFleet("public", "10.0.0.1", "10.0.0.2")
	shared := newSharedStore()
	storageID := component.MustNewID("file_storage")
	host := &storageHost{extensions: map[component.ID]component.Component{
		storageID: &storageExtension{client: &storageClient{store: shared}},
	}}

	start := func() *snmpReceiver {
		cfg := subnetTestConfig("10.0.0.0/29", "public")
		cfg.Storage = &storageID
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
		r, err := newReceiver(cfg, receivertest.NewNopSettings(componentType), new(consumertest.MetricsSink))
		if err != nil {
			t.Fatal(err)
		}
		r.dial = f.dial
		if err := r.Start(context.Background(), host); err != nil {
			t.Fatalf("Start: %v", err)
		}
		return r
	}

	first := start()
	waitFor(t, 5*time.Second, "discovery", func() bool { return first.devices.Len() == 2 })
	// Shutdown persists the registry.
	if err := first.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	// A second receiver over the same storage must come up already knowing the
	// fleet, rather than waiting for a scan.
	second := start()
	defer func() {
		if err := second.Shutdown(context.Background()); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	}()

	if got := second.devices.Len(); got != 2 {
		t.Fatalf("restored %d devices, want 2 immediately after start", got)
	}
	for _, dev := range second.devices.Devices() {
		// Without the sysObjectID and credential index the restart would pay for
		// profile detection and credential iteration all over again.
		if dev.SysObjectID == "" {
			t.Errorf("device %s restored without a sysObjectID", dev.Address)
		}
		if dev.ProfileName == "" {
			t.Errorf("device %s restored without a profile", dev.Address)
		}
	}
}

// storageHost is a component.Host exposing a storage extension.
type storageHost struct {
	extensions map[component.ID]component.Component
}

func (h *storageHost) GetExtensions() map[component.ID]component.Component {
	return h.extensions
}

// storageExtension is a minimal storage.Extension over an in-memory map.
type storageExtension struct {
	client *storageClient
}

func (e *storageExtension) Start(context.Context, component.Host) error { return nil }
func (e *storageExtension) Shutdown(context.Context) error              { return nil }

func (e *storageExtension) GetClient(context.Context, component.Kind, component.ID, string) (storage.Client, error) {
	return e.client, nil
}

type storageClient struct {
	store *sharedStore
}

func (c *storageClient) Get(ctx context.Context, key string) ([]byte, error) {
	return c.store.Get(ctx, key)
}

func (c *storageClient) Set(ctx context.Context, key string, value []byte) error {
	return c.store.Set(ctx, key, value)
}

func (c *storageClient) Delete(ctx context.Context, key string) error {
	return c.store.Set(ctx, key, nil)
}

func (c *storageClient) Batch(context.Context, ...*storage.Operation) error { return nil }
func (c *storageClient) Close(context.Context) error                        { return nil }

type sharedStore struct {
	mu   sync.Mutex
	data map[string][]byte
}

func newSharedStore() *sharedStore {
	return &sharedStore{data: map[string][]byte{}}
}

func (s *sharedStore) Get(_ context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data[key], nil
}

func (s *sharedStore) Set(_ context.Context, key string, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = value
	return nil
}

// TestReceiverSpreadsPollsAcrossInterval checks the scheduler does not issue
// every device's poll in the same instant, which is the behaviour that keeps a
// large fleet's PDU rate flat.
func TestReceiverSpreadsPollsAcrossInterval(t *testing.T) {
	const interval = 2 * time.Second
	offsets := map[time.Duration]bool{}
	for i := 1; i <= 50; i++ {
		id := discovery.DeviceID(fmt.Sprintf("10.0.0.%d", i), "10.0.0.0/24")
		offsets[offsetFor(id, interval)] = true
	}
	// Distinct offsets mean the load is spread rather than bursty.
	if len(offsets) < 40 {
		t.Errorf("only %d distinct offsets for 50 devices; polls are not spread", len(offsets))
	}
	for offset := range offsets {
		if offset < 0 || offset >= interval {
			t.Errorf("offset %v is outside the interval", offset)
		}
	}
	// The offset must be stable, so a device keeps its slot across restarts.
	id := discovery.DeviceID("10.0.0.7", "10.0.0.0/24")
	first, second := offsetFor(id, interval), offsetFor(id, interval)
	if first != second {
		t.Errorf("offset is not deterministic: %v then %v", first, second)
	}
}

func TestReceiverShutdownIsClean(t *testing.T) {
	f := newFleet("public", "10.0.0.1")
	cfg := subnetTestConfig("10.0.0.0/30", "public")

	sink := new(consumertest.MetricsSink)
	r, err := newReceiver(cfg, receivertest.NewNopSettings(componentType), sink)
	if err != nil {
		t.Fatal(err)
	}
	r.dial = f.dial
	if err := r.Start(context.Background(), componenttest.NewNopHost()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "a poll", func() bool { return len(sink.AllMetrics()) > 0 })

	// Shutdown must return promptly and without error.
	done := make(chan error, 1)
	go func() { done <- r.Shutdown(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown did not return")
	}
}

// TestReceiverUnreachableSubnetProducesNoMetrics checks a silent network is
// handled quietly rather than producing phantom devices.
func TestReceiverUnreachableSubnetProducesNoMetrics(t *testing.T) {
	f := newFleet("public") // nothing alive
	r, sink := startReceiver(t, subnetTestConfig("10.0.0.0/30", "public"), f.dial)

	time.Sleep(700 * time.Millisecond)
	if r.devices.Len() != 0 {
		t.Errorf("registered %d devices on a silent subnet", r.devices.Len())
	}
	if got := len(sink.AllMetrics()); got != 0 {
		t.Errorf("got %d metric batches from a silent subnet", got)
	}
}

// TestReceiverWrongCommunityFindsNothing checks credentials are actually
// enforced rather than assumed.
func TestReceiverWrongCommunityFindsNothing(t *testing.T) {
	f := newFleet("secret", "10.0.0.1")
	r, _ := startReceiver(t, subnetTestConfig("10.0.0.0/30", "public"), f.dial)

	time.Sleep(700 * time.Millisecond)
	if r.devices.Len() != 0 {
		t.Errorf("a device answered with the wrong community")
	}
}

// TestReceiverMissingOIDsArePruned checks the pruning path is wired: after a
// poll, OIDs the device does not implement are no longer requested.
func TestReceiverMissingOIDsArePruned(t *testing.T) {
	f := newFleet("public", "10.9.9.1")

	cfg := createDefaultConfig().(*Config)
	cfg.CollectionInterval = 100 * time.Millisecond
	cfg.Timeout = 100 * time.Millisecond
	cfg.Devices = []DeviceConfig{{
		Endpoint:    "10.9.9.1",
		Credentials: snmp.Credentials{Version: "v2c", Community: "public"},
		Profile:     "cisco-catalyst",
	}}

	r, sink := startReceiver(t, cfg, f.dial)
	waitFor(t, 5*time.Second, "at least two polls", func() bool {
		return len(sink.AllMetrics()) >= 2
	})

	r.mu.Lock()
	defer r.mu.Unlock()
	for id, state := range r.state {
		// The fake answers only a handful of the profile's OIDs, so most should
		// have been pruned after the first poll.
		if state.compiled.MissingCount() == 0 {
			t.Errorf("device %s pruned no OIDs despite answering few of them", id)
		}
		scalars, columns := state.compiled.FetchOIDs()
		if len(scalars)+len(columns) >= len(state.compiled.ScalarOIDs)+len(state.compiled.ColumnOIDs) {
			t.Error("pruning did not reduce the fetch set")
		}
	}
}

func TestMetricsAreValid(t *testing.T) {
	f := newFleet("public", "10.0.0.1")
	_, sink := startReceiver(t, subnetTestConfig("10.0.0.0/30", "public"), f.dial)
	waitFor(t, 5*time.Second, "metrics", func() bool { return len(sink.AllMetrics()) > 0 })

	for _, md := range sink.AllMetrics() {
		for i := 0; i < md.ResourceMetrics().Len(); i++ {
			sms := md.ResourceMetrics().At(i).ScopeMetrics()
			for j := 0; j < sms.Len(); j++ {
				if got := sms.At(j).Scope().Name(); got == "" {
					t.Error("scope name is empty")
				}
				ms := sms.At(j).Metrics()
				for k := 0; k < ms.Len(); k++ {
					m := ms.At(k)
					if m.Name() == "" {
						t.Error("metric with an empty name")
					}
					if m.Type() == pmetric.MetricTypeEmpty {
						t.Errorf("metric %s has no data", m.Name())
					}
				}
			}
		}
	}
}

// slowSession delays every request, standing in for a lossy or distant device
// whose poll outlasts the collection interval.
type slowSession struct {
	snmp.Session
	delay time.Duration
	enter func()
	exit  func()
}

func (s *slowSession) Connect() error { s.enter(); return s.Session.Connect() }
func (s *slowSession) Close() error   { s.exit(); return s.Session.Close() }

func (s *slowSession) Get(oids []string) (*gosnmp.SnmpPacket, error) {
	time.Sleep(s.delay)
	return s.Session.Get(oids)
}

func (s *slowSession) GetBulk(oids []string, maxRepetitions uint32) (*gosnmp.SnmpPacket, error) {
	time.Sleep(s.delay)
	return s.Session.GetBulk(oids, maxRepetitions)
}

// TestReceiverNeverPollsDeviceConcurrently: a device whose poll runs longer
// than the collection interval must not be dispatched to a second worker while
// the first is still running -- two concurrent polls race on the device's
// builder state and emit duplicate cumulative streams.
func TestReceiverNeverPollsDeviceConcurrently(t *testing.T) {
	f := newFleet("public", "10.9.9.5")

	var mu sync.Mutex
	active, maxActive, finished := 0, 0, 0

	cfg := createDefaultConfig().(*Config)
	cfg.CollectionInterval = 50 * time.Millisecond
	cfg.Timeout = 50 * time.Millisecond
	cfg.Devices = []DeviceConfig{{
		Endpoint:    "10.9.9.5",
		Credentials: snmp.Credentials{Version: "v2c", Community: "public"},
		Profile:     "cisco-catalyst",
	}}

	dial := func(c snmp.ConnectionConfig) (snmp.Session, error) {
		inner, err := f.dial(c)
		if err != nil {
			return nil, err
		}
		return &slowSession{
			Session: inner,
			delay:   20 * time.Millisecond,
			enter: func() {
				mu.Lock()
				active++
				if active > maxActive {
					maxActive = active
				}
				mu.Unlock()
			},
			exit: func() {
				mu.Lock()
				active--
				finished++
				mu.Unlock()
			},
		}, nil
	}

	startReceiver(t, cfg, dial)
	waitFor(t, 20*time.Second, "several polls to complete", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return finished >= 3
	})

	mu.Lock()
	defer mu.Unlock()
	if maxActive != 1 {
		t.Errorf("%d polls of the same device ran concurrently, want 1", maxActive)
	}
}

// TestReceiverStaticDeviceIsNotDuplicatedByScan: a devices: entry that also
// lives inside a scanned subnet must keep a single registry identity, or it is
// polled twice per interval under two different resources.
func TestReceiverStaticDeviceIsNotDuplicatedByScan(t *testing.T) {
	f := newFleet("public", "10.0.0.1", "10.0.0.2")

	cfg := subnetTestConfig("10.0.0.0/29", "public")
	cfg.Devices = []DeviceConfig{{
		Endpoint:    "10.0.0.1",
		Credentials: snmp.Credentials{Version: "v2c", Community: "public"},
	}}

	r, _ := startReceiver(t, cfg, f.dial)
	waitFor(t, 5*time.Second, "the scan to register the other device", func() bool {
		_, ok := r.devices.Get(discovery.DeviceID("10.0.0.2", "10.0.0.0/29"))
		return ok
	})

	entries := 0
	for _, dev := range r.devices.Devices() {
		if dev.Address != "10.0.0.1" {
			continue
		}
		entries++
		if !dev.Static {
			t.Errorf("scan registered a duplicate identity %q for the static device", dev.ID)
		}
	}
	if entries != 1 {
		t.Errorf("static device has %d registry entries, want exactly 1", entries)
	}
}

// TestReceiverAgesOutDeviceWhoseSubnetWasRemoved: a restored device whose
// subnet is no longer configured can never poll successfully; it must spend its
// failure budget and be dropped rather than being error-logged forever.
func TestReceiverAgesOutDeviceWhoseSubnetWasRemoved(t *testing.T) {
	f := newFleet("public", "10.0.0.1")
	cfg := subnetTestConfig("10.0.0.0/30", "public")
	cfg.Discovery.AllowedFailures = 1
	cfg.Discovery.RediscoveryInterval = time.Hour

	r, _ := startReceiver(t, cfg, f.dial)

	zombie := discovery.Device{
		ID:       discovery.DeviceID("10.99.0.1", "10.99.0.0/24"),
		Address:  "10.99.0.1",
		Subnet:   "10.99.0.0/24",
		LastSeen: time.Now(),
	}
	r.devices.Upsert(zombie)

	waitFor(t, 10*time.Second, "the orphaned device to be dropped", func() bool {
		_, ok := r.devices.Get(zombie.ID)
		return !ok
	})
}
