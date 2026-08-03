package networkdevicereceiver

import (
	"strings"
	"testing"
	"time"

	"github.com/tsuga-dev/networkdevicereceiver/internal/snmp"
)

func TestDefaultConfigIsNotUsableAlone(t *testing.T) {
	cfg := createDefaultConfig().(*Config)
	// Defaults must be sane, but with no subnets or devices there is nothing to
	// poll, so the config is incomplete rather than valid.
	if err := cfg.Validate(); err == nil {
		t.Error("expected the default config to require subnets or devices")
	}
	if cfg.CollectionInterval != defaultCollectionInterval {
		t.Errorf("collection_interval = %v", cfg.CollectionInterval)
	}
	if cfg.Discovery.RediscoveryInterval != defaultRediscoveryInterval {
		t.Errorf("rediscovery_interval = %v", cfg.Discovery.RediscoveryInterval)
	}
	if cfg.Naming.Scheme != "semconv" {
		t.Errorf("naming.scheme = %q, want semconv", cfg.Naming.Scheme)
	}
	if cfg.Fetch.MaxRowsPerColumn != snmp.DefaultMaxRowsPerColumn {
		t.Errorf("max_rows_per_column = %d", cfg.Fetch.MaxRowsPerColumn)
	}
}

func TestConfigValidate(t *testing.T) {
	valid := func() *Config {
		cfg := createDefaultConfig().(*Config)
		cfg.Subnets = []SubnetConfig{{
			Network:     "10.0.0.0/24",
			Credentials: snmp.Credentials{Version: "v2c", Community: "public"},
		}}
		return cfg
	}

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{name: "valid subnet", mutate: func(*Config) {}},
		{
			name:    "no collection interval",
			mutate:  func(c *Config) { c.CollectionInterval = 0 },
			wantErr: "collection_interval must be positive",
		},
		{
			name:    "negative pollers",
			mutate:  func(c *Config) { c.Pollers = -1 },
			wantErr: "pollers cannot be negative",
		},
		{
			name:    "unknown naming scheme",
			mutate:  func(c *Config) { c.Naming.Scheme = "invented" },
			wantErr: "naming.scheme",
		},
		{
			name:    "subnet without credentials",
			mutate:  func(c *Config) { c.Subnets[0].Credentials = snmp.Credentials{} },
			wantErr: "set a community or user inline",
		},
		{
			name:    "subnet with a bad CIDR",
			mutate:  func(c *Config) { c.Subnets[0].Network = "10.0.0.1" },
			wantErr: "parse subnet",
		},
		{
			name:    "subnet too wide",
			mutate:  func(c *Config) { c.Subnets[0].Network = "10.0.0.0/8" },
			wantErr: "max_hosts",
		},
		{
			name: "subnet with a bad ignored address",
			mutate: func(c *Config) {
				c.Subnets[0].IgnoredAddresses = []string{"not-an-ip"}
			},
			wantErr: "is not an IP address",
		},
		{
			name: "v3 subnet missing a key",
			mutate: func(c *Config) {
				c.Subnets[0].Credentials = snmp.Credentials{
					Version: "v3", User: "admin", AuthProtocol: "SHA256",
				}
			},
			wantErr: "without auth_key",
		},
		{
			name: "device without an endpoint",
			mutate: func(c *Config) {
				c.Devices = []DeviceConfig{{
					Credentials: snmp.Credentials{Version: "v2c", Community: "public"},
				}}
			},
			wantErr: "endpoint is required",
		},
		{
			name: "device with a non-udp scheme",
			mutate: func(c *Config) {
				c.Devices = []DeviceConfig{{
					Endpoint:    "tcp://10.0.0.1:161",
					Credentials: snmp.Credentials{Version: "v2c", Community: "public"},
				}}
			},
			wantErr: "only the udp scheme",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid()
			tc.mutate(cfg)
			err := cfg.Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Errorf("expected an error containing %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestValidateReportsEveryProblem checks a user fixing a config sees the whole
// list rather than one error per run.
func TestValidateReportsEveryProblem(t *testing.T) {
	cfg := createDefaultConfig().(*Config)
	cfg.CollectionInterval = 0
	cfg.Naming.Scheme = "nope"
	cfg.Subnets = []SubnetConfig{{Network: "bad"}}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"collection_interval", "naming.scheme", "parse subnet", "community or user"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error is missing %q:\n%v", want, err)
		}
	}
}

func TestSubnetCredentialOrdering(t *testing.T) {
	subnet := SubnetConfig{
		Network:     "10.0.0.0/24",
		Credentials: snmp.Credentials{Version: "v2c", Community: "inline"},
		Authentications: []snmp.Credentials{
			{Version: "v2c", Community: "listed-one"},
			{Version: "v2c", Community: "listed-two"},
		},
	}
	creds := subnet.credentials()
	if len(creds) != 3 {
		t.Fatalf("got %d credential sets, want 3", len(creds))
	}
	// The inline set is tried first, so the simple case stays fast.
	if creds[0].Community != "inline" {
		t.Errorf("first credential = %q, want inline", creds[0].Community)
	}
	if creds[2].Community != "listed-two" {
		t.Errorf("last credential = %q", creds[2].Community)
	}
}

func TestParseEndpoint(t *testing.T) {
	tests := []struct {
		endpoint string
		wantHost string
		wantPort uint16
		wantErr  bool
	}{
		{endpoint: "10.0.0.1", wantHost: "10.0.0.1", wantPort: 161},
		{endpoint: "10.0.0.1:1161", wantHost: "10.0.0.1", wantPort: 1161},
		{endpoint: "udp://10.0.0.1:161", wantHost: "10.0.0.1", wantPort: 161},
		{endpoint: "switch.example.com", wantHost: "switch.example.com", wantPort: 161},
		{endpoint: "udp://switch.example.com:161", wantHost: "switch.example.com", wantPort: 161},
		// IPv6 needs brackets to be unambiguous.
		{endpoint: "[2001:db8::1]:161", wantHost: "2001:db8::1", wantPort: 161},
		{endpoint: "", wantErr: true},
		{endpoint: "tcp://10.0.0.1:161", wantErr: true},
		{endpoint: "10.0.0.1:notaport", wantErr: true},
		{endpoint: ":161", wantErr: true},
	}
	for _, tc := range tests {
		host, port, err := parseEndpoint(tc.endpoint)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseEndpoint(%q) should fail", tc.endpoint)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseEndpoint(%q): %v", tc.endpoint, err)
			continue
		}
		if host != tc.wantHost || port != tc.wantPort {
			t.Errorf("parseEndpoint(%q) = %s:%d, want %s:%d",
				tc.endpoint, host, port, tc.wantHost, tc.wantPort)
		}
	}
}

func TestNamingOptionsFillDefaults(t *testing.T) {
	cfg := &Config{}
	opts := cfg.namingOptions()
	if opts.Scheme != "semconv" {
		t.Errorf("scheme = %q", opts.Scheme)
	}
	if opts.FallbackNamespace != "snmp" {
		t.Errorf("fallback namespace = %q", opts.FallbackNamespace)
	}
}

func TestSubnetConfigsCarryDefaultPort(t *testing.T) {
	cfg := &Config{
		Timeout: 2 * time.Second,
		Retries: 1,
		Subnets: []SubnetConfig{{
			Network:     "10.0.0.0/24",
			Credentials: snmp.Credentials{Version: "v2c", Community: "public"},
		}},
	}
	subnets := cfg.subnetConfigs()
	if len(subnets) != 1 {
		t.Fatal("expected one subnet")
	}
	if subnets[0].Port != defaultPort {
		t.Errorf("port = %d, want %d", subnets[0].Port, defaultPort)
	}
	if subnets[0].Timeout != 2*time.Second {
		t.Errorf("timeout not propagated: %v", subnets[0].Timeout)
	}
}

func TestFactoryCreatesReceiver(t *testing.T) {
	factory := NewFactory()
	if factory.Type() != componentType {
		t.Errorf("type = %v", factory.Type())
	}
	if _, ok := factory.CreateDefaultConfig().(*Config); !ok {
		t.Error("default config has the wrong type")
	}
}
