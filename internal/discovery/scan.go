package discovery

import (
	"context"
	"fmt"
	"net/netip"
	"sync"
	"time"

	"github.com/tsuga-dev/networkdevicereceiver/internal/snmp"
)

// Probe OIDs. sysObjectID both proves reachability and selects the profile, so
// one GET does discovery and identification together.
const (
	OIDSysObjectID = "1.3.6.1.2.1.1.2.0"
	OIDSysName     = "1.3.6.1.2.1.1.5.0"
)

// DefaultMaxHosts bounds how many addresses one subnet may expand to. A /8
// expands to sixteen million probes, which is a configuration mistake rather
// than an intent, so it is refused rather than attempted.
const DefaultMaxHosts = 65536

// SubnetConfig describes one subnet to scan.
type SubnetConfig struct {
	// Network is a CIDR such as 10.0.0.0/24.
	Network string
	Port    uint16
	// Credentials are tried in order until one answers.
	Credentials []snmp.Credentials
	// IgnoredAddresses are skipped, for hosts that must not be probed.
	IgnoredAddresses []string

	Timeout time.Duration
	Retries int
	// MaxHosts overrides DefaultMaxHosts.
	MaxHosts int
}

// Dialer opens a session. Injected so scanning can be tested without a network.
type Dialer func(snmp.ConnectionConfig) (snmp.Session, error)

// Scanner probes subnets for reachable devices.
type Scanner struct {
	// Workers bounds concurrent probes. Probing is IO-bound, so this can exceed
	// the core count considerably.
	Workers int
	Dial    Dialer
	// Dedupe drops devices that answer on more than one address with the same
	// identity.
	Dedupe bool
}

// probeResult is one address's outcome.
type probeResult struct {
	device Device
	found  bool
}

// Scan probes every address in the subnet and returns the devices that answered.
func (s *Scanner) Scan(ctx context.Context, subnet SubnetConfig) ([]Device, error) {
	addresses, err := ExpandCIDR(subnet.Network, subnet.MaxHosts)
	if err != nil {
		return nil, err
	}
	if len(subnet.Credentials) == 0 {
		return nil, fmt.Errorf("subnet %s has no credentials", subnet.Network)
	}

	ignored := make(map[string]struct{}, len(subnet.IgnoredAddresses))
	for _, addr := range subnet.IgnoredAddresses {
		ignored[addr] = struct{}{}
	}

	workers := s.Workers
	if workers <= 0 {
		workers = 10
	}

	jobs := make(chan string)
	results := make(chan probeResult)

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for address := range jobs {
				device, found := s.probe(ctx, address, subnet)
				select {
				case results <- probeResult{device: device, found: found}:
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	go func() {
		defer close(jobs)
		for _, address := range addresses {
			if _, skip := ignored[address]; skip {
				continue
			}
			select {
			case jobs <- address:
			case <-ctx.Done():
				return
			}
		}
	}()

	go func() {
		wg.Wait()
		close(results)
	}()

	var found []Device
	for result := range results {
		if result.found {
			found = append(found, result.device)
		}
	}
	if err := ctx.Err(); err != nil {
		// Return what was discovered before cancellation; a partial scan is
		// still useful and the next interval completes it.
		return found, err
	}

	if s.Dedupe {
		found = dedupe(found)
	}
	return found, nil
}

// probe tries each credential set in turn, returning on the first that answers.
func (s *Scanner) probe(ctx context.Context, address string, subnet SubnetConfig) (Device, bool) {
	for index, creds := range subnet.Credentials {
		if ctx.Err() != nil {
			return Device{}, false
		}

		cfg := snmp.ConnectionConfig{
			Host:        address,
			Port:        subnet.Port,
			Credentials: creds,
			Timeout:     subnet.Timeout,
			Retries:     subnet.Retries,
		}
		session, err := s.Dial(cfg)
		if err != nil {
			continue
		}

		sysObjectID, _, ok := probeIdentity(session)
		_ = session.Close()
		if !ok {
			continue
		}

		// sysName is deliberately not stored: it is read again on every poll, so
		// a device rename is picked up without waiting for a rediscovery.
		return Device{
			ID:          DeviceID(address, subnet.Network),
			Address:     address,
			Port:        subnet.Port,
			Subnet:      subnet.Network,
			AuthIndex:   index,
			SysObjectID: sysObjectID,
			LastSeen:    time.Now(),
		}, true
	}
	return Device{}, false
}

// probeIdentity reads the two OIDs that identify a device.
func probeIdentity(session snmp.Session) (sysObjectID, sysName string, ok bool) {
	if err := session.Connect(); err != nil {
		return "", "", false
	}
	store, _, err := snmp.Fetch(session, []string{OIDSysObjectID, OIDSysName}, nil, snmp.FetchConfig{})
	if err != nil {
		// A partial answer still proves the device is there and speaks SNMP.
		if store == nil {
			return "", "", false
		}
	}

	value, err := store.Scalar(OIDSysObjectID)
	if err != nil {
		// Without sysObjectID no profile can be selected, so the device is not
		// usable even if something answered.
		return "", "", false
	}
	sysObjectID = value.String()

	if name, err := store.Scalar(OIDSysName); err == nil {
		sysName = name.String()
	}
	return sysObjectID, sysName, sysObjectID != ""
}

// DeviceID builds a stable identifier. The subnet is included so the same
// address in two different configured subnets stays distinguishable.
func DeviceID(address, subnet string) string {
	if subnet == "" {
		return address
	}
	return address + "|" + subnet
}

// dedupe drops devices sharing an identity, keeping the lowest address.
//
// Identity here is the sysObjectID alone, which is a weak signal: many devices
// of one model share it. Deduping on it would collapse a whole fleet, so this
// only removes exact address duplicates and is a placeholder for the serial or
// engine-ID based dedupe real multi-homed devices need.
func dedupe(devices []Device) []Device {
	seen := make(map[string]struct{}, len(devices))
	out := make([]Device, 0, len(devices))
	for _, dev := range devices {
		if _, dup := seen[dev.ID]; dup {
			continue
		}
		seen[dev.ID] = struct{}{}
		out = append(out, dev)
	}
	return out
}

// ExpandCIDR lists the probeable addresses of a CIDR.
//
// The network and broadcast addresses are omitted for IPv4 prefixes wide enough
// to have them, since probing them is pointless and broadcast probing is
// actively antisocial.
func ExpandCIDR(cidr string, maxHosts int) ([]string, error) {
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil {
		return nil, fmt.Errorf("parse subnet %q: %w", cidr, err)
	}
	if maxHosts <= 0 {
		maxHosts = DefaultMaxHosts
	}

	prefix = prefix.Masked()
	total := hostCount(prefix)
	if total > uint64(maxHosts) {
		return nil, fmt.Errorf("subnet %s expands to %d addresses, above the limit of %d; "+
			"split it or raise max_hosts", cidr, total, maxHosts)
	}

	skipEdges := prefix.Addr().Is4() && prefix.Bits() < 31
	out := make([]string, 0, total)

	addr := prefix.Addr()
	for prefix.Contains(addr) {
		isNetwork := addr == prefix.Addr()
		isBroadcast := skipEdges && !prefix.Contains(addr.Next())
		if !skipEdges || (!isNetwork && !isBroadcast) {
			out = append(out, addr.String())
		}
		next := addr.Next()
		if !next.IsValid() {
			break
		}
		addr = next
	}
	return out, nil
}

// hostCount returns how many addresses a prefix covers, saturating rather than
// overflowing on very wide prefixes.
func hostCount(prefix netip.Prefix) uint64 {
	bits := prefix.Addr().BitLen() - prefix.Bits()
	if bits >= 64 {
		return ^uint64(0)
	}
	return uint64(1) << uint(bits)
}
