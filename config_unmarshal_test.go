package networkdevicereceiver

import (
	"testing"
	"time"

	"go.opentelemetry.io/collector/confmap/confmaptest"
)

// TestUnmarshalConfig checks the config actually maps from YAML. It matters
// because the inline-credentials shorthand relies on mapstructure squashing, and
// a wrong tag there would silently drop every credential.
func TestUnmarshalConfig(t *testing.T) {
	conf, err := confmaptest.LoadConf("testdata/config.yaml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	sub, err := conf.Sub("receivers::networkdevice")
	if err != nil {
		t.Fatalf("sub: %v", err)
	}

	cfg := createDefaultConfig().(*Config)
	if err := sub.Unmarshal(cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the example config must be valid: %v", err)
	}

	if cfg.CollectionInterval != 30*time.Second {
		t.Errorf("collection_interval = %v", cfg.CollectionInterval)
	}
	if cfg.Pollers != 25 {
		t.Errorf("pollers = %d", cfg.Pollers)
	}
	if cfg.Discovery.RediscoveryInterval != 15*time.Minute {
		t.Errorf("rediscovery_interval = %v", cfg.Discovery.RediscoveryInterval)
	}
	if cfg.Discovery.Workers != 4 {
		t.Errorf("discovery.workers = %d, want 4", cfg.Discovery.Workers)
	}
	if cfg.Discovery.AllowedFailures != 5 {
		t.Errorf("discovery.allowed_failures = %d, want 5", cfg.Discovery.AllowedFailures)
	}

	if len(cfg.Subnets) != 2 {
		t.Fatalf("got %d subnets", len(cfg.Subnets))
	}

	// The inline credential form must survive squashing.
	first := cfg.Subnets[0]
	if first.Community != "public" {
		t.Errorf("inline community = %q, want public; mapstructure squash is not working", first.Community)
	}
	if first.Version != "v2c" {
		t.Errorf("inline version = %q", first.Version)
	}
	if got := first.credentials(); len(got) != 1 {
		t.Errorf("first subnet has %d credential sets, want 1", len(got))
	}

	second := cfg.Subnets[1]
	if second.Port != 1161 {
		t.Errorf("port = %d", second.Port)
	}
	if second.MaxHosts != 32 {
		t.Errorf("max_hosts = %d", second.MaxHosts)
	}
	if len(second.IgnoredAddresses) != 1 || second.IgnoredAddresses[0] != "10.20.0.1" {
		t.Errorf("ignored_addresses = %v", second.IgnoredAddresses)
	}
	creds := second.credentials()
	if len(creds) != 2 {
		t.Fatalf("second subnet has %d credential sets, want 2", len(creds))
	}
	if creds[0].User != "monitor" || creds[0].AuthProtocol != "SHA256" || creds[0].PrivKey != "privpass" {
		t.Errorf("v3 credentials not read: %+v", creds[0])
	}
	if creds[1].Community != "fallback" {
		t.Errorf("second credential set = %+v", creds[1])
	}

	if len(cfg.Devices) != 1 {
		t.Fatalf("got %d devices", len(cfg.Devices))
	}
	device := cfg.Devices[0]
	if device.Endpoint != "udp://10.9.9.1:161" {
		t.Errorf("endpoint = %q", device.Endpoint)
	}
	if device.Profile != "cisco-nexus" {
		t.Errorf("profile = %q", device.Profile)
	}
	if device.Community != "public" {
		t.Errorf("device community = %q; squash is not working on devices", device.Community)
	}

	if cfg.Profiles.UserDir != "/etc/otelcol/snmp.d/profiles" {
		t.Errorf("user_dir = %q", cfg.Profiles.UserDir)
	}
	if cfg.Naming.Scheme != "both" || cfg.Naming.FallbackNamespace != "network.device" {
		t.Errorf("naming = %+v", cfg.Naming)
	}
	if !cfg.Naming.SystemNamespaceForDeviceOS {
		t.Error("system_namespace_for_device_os not read")
	}
	if cfg.Fetch.OIDBatchSize != 5 || cfg.Fetch.BulkMaxRepetitions != 25 || cfg.Fetch.MaxRowsPerColumn != 500 {
		t.Errorf("fetch = %+v", cfg.Fetch)
	}
}

// TestUnmarshalExampleConfig keeps the shipped example honest: it must load and
// validate, so a documented config never fails on a user's first run.
func TestUnmarshalExampleConfig(t *testing.T) {
	conf, err := confmaptest.LoadConf("examples/two-subnets.yaml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	sub, err := conf.Sub("receivers::networkdevice")
	if err != nil {
		t.Fatalf("sub: %v", err)
	}

	cfg := createDefaultConfig().(*Config)
	if err := sub.Unmarshal(cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// The example uses ${env:...} placeholders, which confmaptest leaves
	// unexpanded; they still satisfy "a community is set".
	if err := cfg.Validate(); err != nil {
		t.Errorf("the shipped example must validate: %v", err)
	}
	if len(cfg.Subnets) != 2 || len(cfg.Devices) != 1 {
		t.Errorf("example has %d subnets and %d devices", len(cfg.Subnets), len(cfg.Devices))
	}
	if cfg.Storage == nil || cfg.Storage.String() != "file_storage" {
		t.Errorf("storage = %v, want file_storage", cfg.Storage)
	}
}
