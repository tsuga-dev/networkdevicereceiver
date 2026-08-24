// Package discovery finds SNMP devices on configured subnets and tracks their
// lifecycle: which credential answered, how many consecutive probes have failed,
// and which profile matched.
//
// The registry is persisted so an agent restart resumes monitoring immediately
// instead of rescanning cold.
package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Device is one discovered or configured device.
type Device struct {
	// ID is stable across restarts and is used as the resource identity.
	ID string `json:"id"`
	// Address is the device's IP or hostname.
	Address string `json:"address"`
	Port    uint16 `json:"port"`
	// Subnet is the CIDR this device was discovered in, empty when statically
	// configured.
	Subnet string `json:"subnet,omitempty"`
	// AuthIndex is which of the subnet's credential sets answered. Remembering
	// it avoids retrying every credential on every poll.
	AuthIndex int `json:"auth_index"`

	SysObjectID string `json:"sys_object_id,omitempty"`
	ProfileName string `json:"profile,omitempty"`

	// Failures counts consecutive failed polls or probes. A device is only
	// forgotten after AllowedFailures, so a brief outage does not drop it.
	Failures int `json:"failures"`
	// Static marks a device from the devices: list, which is never aged out.
	Static bool `json:"static,omitempty"`

	LastSeen time.Time `json:"last_seen"`
}

// Persistence is the subset of the collector's storage extension the registry
// needs. Declared here so the registry can be tested without an extension.
type Persistence interface {
	Get(ctx context.Context, key string) ([]byte, error)
	Set(ctx context.Context, key string, value []byte) error
}

// persistenceKey is where the device set is stored.
const persistenceKey = "devices"

// Registry holds the current device set.
type Registry struct {
	mu      sync.RWMutex
	devices map[string]*Device

	// allowedFailures is how many consecutive failures a device tolerates
	// before being forgotten.
	allowedFailures int

	store Persistence
}

// NewRegistry returns a registry. store may be nil, in which case the device set
// is in-memory only and a restart rescans.
func NewRegistry(allowedFailures int, store Persistence) *Registry {
	if allowedFailures < 0 {
		allowedFailures = 0
	}
	return &Registry{
		devices:         map[string]*Device{},
		allowedFailures: allowedFailures,
		store:           store,
	}
}

// Upsert adds or refreshes a device, clearing its failure count.
func (r *Registry) Upsert(dev Device) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if existing, ok := r.devices[dev.ID]; ok {
		existing.Address = dev.Address
		existing.Port = dev.Port
		existing.AuthIndex = dev.AuthIndex
		existing.LastSeen = dev.LastSeen
		existing.Failures = 0
		if dev.SysObjectID != "" {
			existing.SysObjectID = dev.SysObjectID
		}
		if dev.ProfileName != "" {
			existing.ProfileName = dev.ProfileName
		}
		if dev.Subnet != "" {
			existing.Subnet = dev.Subnet
		}
		existing.Static = existing.Static || dev.Static
		return
	}
	copied := dev
	copied.Failures = 0
	r.devices[dev.ID] = &copied
}

// Devices returns a snapshot, ordered for deterministic scheduling.
func (r *Registry) Devices() []Device {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]Device, 0, len(r.devices))
	for _, dev := range r.devices {
		out = append(out, *dev)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Len reports how many devices are being monitored.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.devices)
}

// Get returns one device.
func (r *Registry) Get(id string) (Device, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	dev, ok := r.devices[id]
	if !ok {
		return Device{}, false
	}
	return *dev, true
}

// Remove deletes a device, however it was registered. Used when a restored
// entry duplicates a statically configured device.
func (r *Registry) Remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.devices, id)
}

// SetProfile records the profile a device matched, so the next poll does not
// have to rediscover it.
func (r *Registry) SetProfile(id, sysObjectID, profileName string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if dev, ok := r.devices[id]; ok {
		dev.SysObjectID = sysObjectID
		dev.ProfileName = profileName
	}
}

// MarkSuccess clears a device's failure count.
func (r *Registry) MarkSuccess(id string, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if dev, ok := r.devices[id]; ok {
		dev.Failures = 0
		dev.LastSeen = at
	}
}

// MarkFailure records a failed poll, reporting whether the device was dropped.
//
// Static devices are never dropped: the operator asserted they exist, so a
// persistent failure is a condition to report rather than a reason to forget.
func (r *Registry) MarkFailure(id string) (dropped bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	dev, ok := r.devices[id]
	if !ok {
		return false
	}
	dev.Failures++
	if dev.Static || dev.Failures <= r.allowedFailures {
		return false
	}
	delete(r.devices, id)
	return true
}

// Save persists the device set. A nil store makes this a no-op.
func (r *Registry) Save(ctx context.Context) error {
	if r.store == nil {
		return nil
	}
	r.mu.RLock()
	devices := make([]Device, 0, len(r.devices))
	for _, dev := range r.devices {
		if dev.Static {
			// Static devices come from configuration on every start; persisting
			// them would resurrect entries the operator has removed.
			continue
		}
		devices = append(devices, *dev)
	}
	r.mu.RUnlock()

	sort.Slice(devices, func(i, j int) bool { return devices[i].ID < devices[j].ID })
	data, err := json.Marshal(devices)
	if err != nil {
		return fmt.Errorf("marshal device registry: %w", err)
	}
	if err := r.store.Set(ctx, persistenceKey, data); err != nil {
		return fmt.Errorf("persist device registry: %w", err)
	}
	return nil
}

// Load restores a persisted device set. A missing or unreadable record is not an
// error: the next scan repopulates the registry.
func (r *Registry) Load(ctx context.Context) (int, error) {
	if r.store == nil {
		return 0, nil
	}
	data, err := r.store.Get(ctx, persistenceKey)
	if err != nil {
		return 0, fmt.Errorf("read device registry: %w", err)
	}
	if len(data) == 0 {
		return 0, nil
	}

	var devices []Device
	if err := json.Unmarshal(data, &devices); err != nil {
		return 0, fmt.Errorf("parse device registry: %w", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for _, dev := range devices {
		copied := dev
		r.devices[dev.ID] = &copied
	}
	return len(devices), nil
}
