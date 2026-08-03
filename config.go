// Package networkdevicereceiver is an OpenTelemetry Collector receiver that
// discovers SNMP devices on configured subnets, selects a device profile from
// sysObjectID, and reports metrics under OpenTelemetry semantic conventions.
//
// It exists because the collection model of the existing snmpreceiver -- one
// receiver block per device, every metric hand-declared -- does not reach fleet
// scale. Here a subnet and a credential set are enough, and the ~240 embedded
// device profiles supply the OIDs.
package networkdevicereceiver

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/collector/component"

	"github.com/tsuga-dev/networkdevicereceiver/internal/discovery"
	"github.com/tsuga-dev/networkdevicereceiver/internal/naming"
	"github.com/tsuga-dev/networkdevicereceiver/internal/snmp"
)

// Config is the receiver's configuration.
type Config struct {
	// CollectionInterval is how often each device is polled. Polls are spread
	// across the interval rather than issued together.
	CollectionInterval time.Duration `mapstructure:"collection_interval"`

	// Timeout and Retries apply to each SNMP request.
	Timeout time.Duration `mapstructure:"timeout"`
	Retries int           `mapstructure:"retries"`

	// Pollers bounds how many devices are polled concurrently.
	Pollers int `mapstructure:"pollers"`

	Discovery DiscoveryConfig `mapstructure:"discovery"`

	// Subnets are scanned for devices.
	Subnets []SubnetConfig `mapstructure:"subnets"`
	// Devices are polled without discovery.
	Devices []DeviceConfig `mapstructure:"devices"`

	Profiles ProfilesConfig `mapstructure:"profiles"`
	Naming   NamingConfig   `mapstructure:"naming"`
	Fetch    FetchConfig    `mapstructure:"fetch"`

	// Storage names a storage extension used to persist the discovered device
	// set. Without it, a restart rescans every subnet before monitoring resumes.
	Storage *component.ID `mapstructure:"storage"`
}

// DiscoveryConfig tunes subnet scanning.
type DiscoveryConfig struct {
	// RediscoveryInterval is how often subnets are rescanned, which is how new
	// devices are found and dead ones age out.
	RediscoveryInterval time.Duration `mapstructure:"rediscovery_interval"`
	// Workers bounds concurrent probes during a scan.
	Workers int `mapstructure:"workers"`
	// AllowedFailures is how many consecutive failures a device tolerates before
	// being forgotten.
	AllowedFailures int `mapstructure:"allowed_failures"`
	// Dedupe drops devices reachable at more than one address.
	Dedupe bool `mapstructure:"dedupe"`
}

// SubnetConfig is one subnet to scan. A single credential set can be given
// inline; several are given under authentications and tried in order.
type SubnetConfig struct {
	Network string `mapstructure:"network"`
	Port    uint16 `mapstructure:"port"`

	snmp.Credentials `mapstructure:",squash"`

	// Authentications are tried in order per device, so one subnet can hold
	// devices with different credentials.
	Authentications []snmp.Credentials `mapstructure:"authentications"`

	// IgnoredAddresses are never probed.
	IgnoredAddresses []string `mapstructure:"ignored_addresses"`
	// MaxHosts overrides the guard against over-wide subnets.
	MaxHosts int `mapstructure:"max_hosts"`
}

// DeviceConfig is one explicitly configured device.
type DeviceConfig struct {
	// Endpoint is host, host:port, or udp://host:port.
	Endpoint string `mapstructure:"endpoint"`

	snmp.Credentials `mapstructure:",squash"`

	// Profile pins a profile by name instead of detecting one from sysObjectID.
	Profile string `mapstructure:"profile"`
}

// ProfilesConfig locates user profiles.
type ProfilesConfig struct {
	// UserDir holds profiles that shadow the embedded ones by name. A Datadog
	// snmp.d/profiles directory can be pointed at directly.
	UserDir string `mapstructure:"user_dir"`
}

// NamingConfig selects how metrics are named.
type NamingConfig struct {
	// Scheme is semconv, datadog_compat or both.
	Scheme string `mapstructure:"scheme"`
	// FallbackNamespace prefixes generated names for unmodelled symbols.
	FallbackNamespace string `mapstructure:"fallback_namespace"`
	// SystemNamespaceForDeviceOS puts device cpu and memory metrics under
	// system.* rather than the fallback namespace.
	SystemNamespaceForDeviceOS bool `mapstructure:"system_namespace_for_device_os"`
}

// FetchConfig tunes SNMP request shaping.
type FetchConfig struct {
	OIDBatchSize       int    `mapstructure:"oid_batch_size"`
	BulkMaxRepetitions uint32 `mapstructure:"bulk_max_repetitions"`
	// MaxRowsPerColumn bounds one table walk, guarding against a runaway table
	// producing unbounded series.
	MaxRowsPerColumn int `mapstructure:"max_rows_per_column"`
}

// Defaults chosen to be safe on a large fleet rather than fastest on one device.
const (
	defaultCollectionInterval  = 60 * time.Second
	defaultRediscoveryInterval = time.Hour
	defaultTimeout             = 5 * time.Second
	defaultRetries             = 3
	defaultPollers             = 50
	defaultDiscoveryWorkers    = 10
	defaultAllowedFailures     = 3
	defaultPort                = 161
)

func createDefaultConfig() component.Config {
	return &Config{
		CollectionInterval: defaultCollectionInterval,
		Timeout:            defaultTimeout,
		Retries:            defaultRetries,
		Pollers:            defaultPollers,
		Discovery: DiscoveryConfig{
			RediscoveryInterval: defaultRediscoveryInterval,
			Workers:             defaultDiscoveryWorkers,
			AllowedFailures:     defaultAllowedFailures,
		},
		Naming: NamingConfig{
			Scheme:            string(naming.SchemeSemconv),
			FallbackNamespace: "snmp",
		},
		Fetch: FetchConfig{
			OIDBatchSize:       snmp.DefaultOIDBatchSize,
			BulkMaxRepetitions: snmp.DefaultBulkMaxRepetitions,
			MaxRowsPerColumn:   snmp.DefaultMaxRowsPerColumn,
		},
	}
}

// Validate reports every configuration problem at once, so a user fixing a
// config sees the whole list rather than one error per run.
func (c *Config) Validate() error {
	var errs []error

	if len(c.Subnets) == 0 && len(c.Devices) == 0 {
		errs = append(errs, errors.New("configure at least one entry under subnets or devices"))
	}
	if c.CollectionInterval <= 0 {
		errs = append(errs, errors.New("collection_interval must be positive"))
	}
	if c.Pollers < 0 {
		errs = append(errs, errors.New("pollers cannot be negative"))
	}
	if c.Discovery.RediscoveryInterval < 0 {
		errs = append(errs, errors.New("discovery.rediscovery_interval cannot be negative"))
	}
	if c.Discovery.AllowedFailures < 0 {
		errs = append(errs, errors.New("discovery.allowed_failures cannot be negative"))
	}

	switch naming.Scheme(c.Naming.Scheme) {
	case naming.SchemeSemconv, naming.SchemeDatadogCompat, naming.SchemeBoth, "":
	default:
		errs = append(errs, fmt.Errorf(
			"naming.scheme %q is not one of semconv, datadog_compat, both", c.Naming.Scheme))
	}

	for i, subnet := range c.Subnets {
		if err := subnet.validate(); err != nil {
			errs = append(errs, fmt.Errorf("subnets[%d]: %w", i, err))
		}
	}
	for i, device := range c.Devices {
		if err := device.validate(); err != nil {
			errs = append(errs, fmt.Errorf("devices[%d]: %w", i, err))
		}
	}
	return errors.Join(errs...)
}

func (s SubnetConfig) validate() error {
	var errs []error
	if s.Network == "" {
		errs = append(errs, errors.New("network is required"))
	} else if _, err := discovery.ExpandCIDR(s.Network, s.MaxHosts); err != nil {
		errs = append(errs, err)
	}

	credentials := s.credentials()
	if len(credentials) == 0 {
		errs = append(errs, errors.New("set a community or user inline, or list authentications"))
	}
	for i, creds := range credentials {
		if err := creds.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("authentications[%d]: %w", i, err))
		}
	}
	for _, address := range s.IgnoredAddresses {
		if net.ParseIP(address) == nil {
			errs = append(errs, fmt.Errorf("ignored_addresses: %q is not an IP address", address))
		}
	}
	return errors.Join(errs...)
}

// credentials returns the credential sets to try, inline form first.
func (s SubnetConfig) credentials() []snmp.Credentials {
	var out []snmp.Credentials
	if s.Community != "" || s.User != "" {
		out = append(out, s.Credentials)
	}
	out = append(out, s.Authentications...)
	return out
}

func (d DeviceConfig) validate() error {
	var errs []error
	if d.Endpoint == "" {
		errs = append(errs, errors.New("endpoint is required"))
	} else if _, _, err := parseEndpoint(d.Endpoint); err != nil {
		errs = append(errs, err)
	}
	if err := d.Credentials.Validate(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// parseEndpoint accepts host, host:port and udp://host:port.
func parseEndpoint(endpoint string) (host string, port uint16, err error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return "", 0, errors.New("endpoint is empty")
	}

	if strings.Contains(endpoint, "://") {
		parsed, parseErr := url.Parse(endpoint)
		if parseErr != nil {
			return "", 0, fmt.Errorf("parse endpoint %q: %w", endpoint, parseErr)
		}
		if parsed.Scheme != "udp" {
			return "", 0, fmt.Errorf("endpoint %q: only the udp scheme is supported", endpoint)
		}
		endpoint = parsed.Host
	}

	if !strings.Contains(endpoint, ":") {
		return endpoint, defaultPort, nil
	}
	hostPart, portPart, splitErr := net.SplitHostPort(endpoint)
	if splitErr != nil {
		return "", 0, fmt.Errorf("parse endpoint %q: %w", endpoint, splitErr)
	}
	parsedPort, convErr := strconv.ParseUint(portPart, 10, 16)
	if convErr != nil {
		return "", 0, fmt.Errorf("endpoint %q: invalid port: %w", endpoint, convErr)
	}
	if hostPart == "" {
		return "", 0, fmt.Errorf("endpoint %q: host is empty", endpoint)
	}
	return hostPart, uint16(parsedPort), nil
}

// namingOptions converts config to registry options.
func (c *Config) namingOptions() naming.Options {
	opts := naming.Options{
		Scheme:                     naming.Scheme(c.Naming.Scheme),
		FallbackNamespace:          c.Naming.FallbackNamespace,
		SystemNamespaceForDeviceOS: c.Naming.SystemNamespaceForDeviceOS,
	}
	if opts.Scheme == "" {
		opts.Scheme = naming.SchemeSemconv
	}
	if opts.FallbackNamespace == "" {
		opts.FallbackNamespace = "snmp"
	}
	return opts
}

// fetchConfig converts config to engine options.
func (c *Config) fetchConfig() snmp.FetchConfig {
	return snmp.FetchConfig{
		OIDBatchSize:       c.Fetch.OIDBatchSize,
		BulkMaxRepetitions: c.Fetch.BulkMaxRepetitions,
		MaxRowsPerColumn:   c.Fetch.MaxRowsPerColumn,
	}
}

// subnetConfigs converts config subnets to discovery subnets.
func (c *Config) subnetConfigs() []discovery.SubnetConfig {
	out := make([]discovery.SubnetConfig, 0, len(c.Subnets))
	for _, subnet := range c.Subnets {
		port := subnet.Port
		if port == 0 {
			port = defaultPort
		}
		out = append(out, discovery.SubnetConfig{
			Network:          subnet.Network,
			Port:             port,
			Credentials:      subnet.credentials(),
			IgnoredAddresses: subnet.IgnoredAddresses,
			Timeout:          c.Timeout,
			Retries:          c.Retries,
			MaxHosts:         subnet.MaxHosts,
		})
	}
	return out
}
