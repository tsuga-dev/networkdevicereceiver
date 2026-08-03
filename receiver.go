package networkdevicereceiver

import (
	"context"
	"fmt"
	"hash/fnv"
	"sync"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/extension/xextension/storage"
	"go.opentelemetry.io/collector/receiver"
	"go.opentelemetry.io/otel/metric"
	"go.uber.org/zap"

	"github.com/tsuga-dev/networkdevicereceiver/internal/discovery"
	"github.com/tsuga-dev/networkdevicereceiver/internal/naming"
	"github.com/tsuga-dev/networkdevicereceiver/internal/profile"
	"github.com/tsuga-dev/networkdevicereceiver/internal/report"
	"github.com/tsuga-dev/networkdevicereceiver/internal/snmp"
)

// schedulerTick is how often the scheduler looks for due devices. It bounds the
// timing error of a poll, and is capped so a short collection interval still
// gets reasonable resolution.
const schedulerTick = time.Second

// snmpReceiver polls a fleet of SNMP devices.
type snmpReceiver struct {
	cfg      *Config
	logger   *zap.Logger
	consumer consumer.Metrics

	profiles *profile.Store
	naming   *naming.Registry
	devices  *discovery.Registry
	scanner  *discovery.Scanner
	fetchCfg snmp.FetchConfig

	// dial is injectable so tests can drive the receiver without a network.
	dial discovery.Dialer

	mu    sync.Mutex
	state map[string]*deviceState

	telemetry telemetry

	cancel      context.CancelFunc
	wg          sync.WaitGroup
	storageDone func(context.Context) error
}

// deviceState is the per-device state that survives between polls: the compiled
// profile and the metric builder holding cumulative start times.
type deviceState struct {
	builder     *report.Builder
	compiled    *profile.Compiled
	profileName string
}

// telemetry holds the receiver's own instruments, mirroring the Datadog agent's
// snmp.devices_monitored and check-duration metrics.
type telemetry struct {
	devicesMonitored  metric.Int64Gauge
	discoveredDevices metric.Int64Counter
	pollDuration      metric.Float64Histogram
	pollErrors        metric.Int64Counter
	pdusSent          metric.Int64Counter
}

func newReceiver(cfg *Config, set receiver.Settings, next consumer.Metrics) (*snmpReceiver, error) {
	meter := set.TelemetrySettings.MeterProvider.Meter(report.ScopeName)

	var tel telemetry
	var err error
	if tel.devicesMonitored, err = meter.Int64Gauge("otelcol_networkdevice_devices_monitored"); err != nil {
		return nil, err
	}
	if tel.discoveredDevices, err = meter.Int64Counter("otelcol_networkdevice_discovered_devices"); err != nil {
		return nil, err
	}
	if tel.pollDuration, err = meter.Float64Histogram("otelcol_networkdevice_poll_duration",
		metric.WithUnit("s")); err != nil {
		return nil, err
	}
	if tel.pollErrors, err = meter.Int64Counter("otelcol_networkdevice_poll_errors"); err != nil {
		return nil, err
	}
	if tel.pdusSent, err = meter.Int64Counter("otelcol_networkdevice_pdus_sent"); err != nil {
		return nil, err
	}

	return &snmpReceiver{
		cfg:       cfg,
		logger:    set.TelemetrySettings.Logger,
		consumer:  next,
		fetchCfg:  cfg.Fetch,
		dial:      snmp.NewSession,
		state:     map[string]*deviceState{},
		telemetry: tel,
	}, nil
}

// Start prepares the profile library and device registry, then begins scanning
// and polling.
func (r *snmpReceiver) Start(ctx context.Context, host component.Host) error {
	profiles, err := profile.NewStore(r.cfg.Profiles.UserDir)
	if err != nil {
		return fmt.Errorf("load profiles: %w", err)
	}
	r.profiles = profiles

	registry, err := naming.New(r.cfg.namingOptions())
	if err != nil {
		return fmt.Errorf("build naming registry: %w", err)
	}
	r.naming = registry

	persistence, err := r.storageClient(ctx, host)
	if err != nil {
		return err
	}
	r.devices = discovery.NewRegistry(r.cfg.Discovery.AllowedFailures, persistence)

	if restored, err := r.devices.Load(ctx); err != nil {
		// A corrupt or unreadable registry must not stop the receiver: the next
		// scan repopulates it.
		r.logger.Warn("could not restore the device registry; will rescan", zap.Error(err))
	} else if restored > 0 {
		r.logger.Info("restored devices from storage", zap.Int("devices", restored))
	}

	if err := r.seedStaticDevices(); err != nil {
		return err
	}

	r.scanner = &discovery.Scanner{
		Workers: r.cfg.Discovery.Workers,
		Dial:    r.dial,
	}

	// The run context is deliberately independent of the start context, which is
	// cancelled once startup finishes.
	runCtx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel

	if len(r.cfg.Subnets) > 0 {
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			r.discoveryLoop(runCtx)
		}()
	}

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.pollLoop(runCtx)
	}()

	r.logger.Info("network device receiver started",
		zap.Int("subnets", len(r.cfg.Subnets)),
		zap.Int("static_devices", len(r.cfg.Devices)),
		zap.Int("profiles", len(profiles.Names())),
		zap.Int("curated_symbols", registry.Curated()))
	return nil
}

// Shutdown stops the loops and persists the device set so the next start does
// not have to rescan.
func (r *snmpReceiver) Shutdown(ctx context.Context) error {
	if r.cancel != nil {
		r.cancel()
	}
	r.wg.Wait()

	var err error
	if r.devices != nil {
		if saveErr := r.devices.Save(ctx); saveErr != nil {
			err = saveErr
		}
	}
	if r.storageDone != nil {
		if closeErr := r.storageDone(ctx); closeErr != nil && err == nil {
			err = closeErr
		}
	}
	return err
}

// storageClient resolves the configured storage extension, if any.
func (r *snmpReceiver) storageClient(ctx context.Context, host component.Host) (discovery.Persistence, error) {
	if r.cfg.Storage == nil {
		return nil, nil
	}
	ext, ok := host.GetExtensions()[*r.cfg.Storage]
	if !ok {
		return nil, fmt.Errorf("storage extension %q is not configured in the service", r.cfg.Storage)
	}
	provider, ok := ext.(storage.Extension)
	if !ok {
		return nil, fmt.Errorf("extension %q is not a storage extension", r.cfg.Storage)
	}
	client, err := provider.GetClient(ctx, component.KindReceiver, *r.cfg.Storage, "")
	if err != nil {
		return nil, fmt.Errorf("get storage client: %w", err)
	}
	r.storageDone = client.Close
	return client, nil
}

// seedStaticDevices registers the explicitly configured devices.
func (r *snmpReceiver) seedStaticDevices() error {
	for i, device := range r.cfg.Devices {
		host, port, err := parseEndpoint(device.Endpoint)
		if err != nil {
			return fmt.Errorf("devices[%d]: %w", i, err)
		}
		r.devices.Upsert(discovery.Device{
			ID:          discovery.DeviceID(host, ""),
			Address:     host,
			Port:        port,
			ProfileName: device.Profile,
			Static:      true,
			LastSeen:    time.Now(),
		})
	}
	return nil
}

// discoveryLoop scans every subnet immediately, then on the rediscovery
// interval, which is how new devices appear and dead ones age out.
func (r *snmpReceiver) discoveryLoop(ctx context.Context) {
	r.scanAll(ctx)

	interval := r.cfg.Discovery.RediscoveryInterval
	if interval <= 0 {
		// Discovery explicitly disabled after the initial scan.
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.scanAll(ctx)
		}
	}
}

func (r *snmpReceiver) scanAll(ctx context.Context) {
	for _, subnet := range r.cfg.subnetConfigs() {
		if ctx.Err() != nil {
			return
		}
		started := time.Now()
		found, err := r.scanner.Scan(ctx, subnet)
		if err != nil && ctx.Err() == nil {
			r.logger.Error("subnet scan failed",
				zap.String("network", subnet.Network), zap.Error(err))
		}

		for _, device := range found {
			r.devices.Upsert(device)

			// Resolve the profile now rather than on first poll. Matching is a
			// local lookup against the embedded library, so this costs nothing
			// and makes the persisted registry self-describing: a restart knows
			// each device's profile without re-deriving it.
			if device.SysObjectID == "" {
				continue
			}
			matched, err := r.profiles.Match(device.SysObjectID)
			if err != nil {
				r.logger.Warn("no profile matches device",
					zap.String("device", device.Address),
					zap.String("sys_object_id", device.SysObjectID),
					zap.Error(err))
				continue
			}
			r.devices.SetProfile(device.ID, device.SysObjectID, matched)
		}
		r.telemetry.discoveredDevices.Add(ctx, int64(len(found)))
		r.logger.Info("subnet scanned",
			zap.String("network", subnet.Network),
			zap.Int("found", len(found)),
			zap.Duration("took", time.Since(started)))
	}

	// Persist after each round so a crash does not lose a completed scan.
	if err := r.devices.Save(ctx); err != nil {
		r.logger.Warn("could not persist the device registry", zap.Error(err))
	}
}

// pollLoop schedules device polls, spreading them across the collection interval
// rather than issuing them all at once.
//
// Datadog polls every device of a subnet when the check runs, which produces a
// burst; spreading the load keeps PDU rate and CPU flat, which matters at
// thousands of devices.
func (r *snmpReceiver) pollLoop(ctx context.Context) {
	pollers := r.cfg.Pollers
	if pollers <= 0 {
		pollers = defaultPollers
	}
	interval := r.cfg.CollectionInterval

	jobs := make(chan discovery.Device)
	var workers sync.WaitGroup
	for range pollers {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for device := range jobs {
				r.pollDevice(ctx, device)
			}
		}()
	}

	tick := schedulerTick
	if interval < tick {
		tick = interval
	}
	ticker := time.NewTicker(tick)

	defer func() {
		ticker.Stop()
		close(jobs)
		workers.Wait()
	}()

	nextDue := map[string]time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			devices := r.devices.Devices()
			r.telemetry.devicesMonitored.Record(ctx, int64(len(devices)))

			live := make(map[string]struct{}, len(devices))
			for _, device := range devices {
				live[device.ID] = struct{}{}

				due, known := nextDue[device.ID]
				if !known {
					// Offset the first poll deterministically so devices spread
					// across the interval and stay spread across restarts.
					nextDue[device.ID] = now.Add(offsetFor(device.ID, interval))
					continue
				}
				if now.Before(due) {
					continue
				}

				// Advance in whole intervals to keep the device's phase, even if
				// a poll was delayed past its slot.
				for !due.After(now) {
					due = due.Add(interval)
				}
				nextDue[device.ID] = due

				select {
				case jobs <- device:
				case <-ctx.Done():
					return
				default:
					// Every poller is busy. Skipping is better than queueing
					// without bound; the next slot will try again.
					r.logger.Debug("pollers saturated, skipping this slot",
						zap.String("device", device.Address))
					r.telemetry.pollErrors.Add(ctx, 1)
				}
			}

			for id := range nextDue {
				if _, still := live[id]; !still {
					delete(nextDue, id)
				}
			}
		}
	}
}

// offsetFor spreads a device's first poll deterministically within the interval,
// so the same device keeps its slot across restarts.
func offsetFor(id string, interval time.Duration) time.Duration {
	if interval <= 0 {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(id))
	return time.Duration(h.Sum64() % uint64(interval))
}

// pollDevice performs one device's collection cycle.
func (r *snmpReceiver) pollDevice(ctx context.Context, device discovery.Device) {
	if ctx.Err() != nil {
		return
	}
	started := time.Now()

	credentials, err := r.credentialsFor(device)
	if err != nil {
		r.logger.Error("no credentials for device", zap.String("device", device.Address), zap.Error(err))
		return
	}

	session, err := r.dial(snmp.ConnectionConfig{
		Host:        device.Address,
		Port:        device.Port,
		Credentials: credentials,
		Timeout:     r.cfg.Timeout,
		Retries:     r.cfg.Retries,
	})
	if err != nil {
		r.failPoll(ctx, device, fmt.Errorf("open session: %w", err))
		return
	}
	if err := session.Connect(); err != nil {
		r.failPoll(ctx, device, fmt.Errorf("connect: %w", err))
		return
	}
	defer func() { _ = session.Close() }()

	state, err := r.ensureProfile(session, device)
	if err != nil {
		r.failPoll(ctx, device, err)
		return
	}

	scalars, columns := state.compiled.FetchOIDs()
	values, fetchReport, fetchErr := snmp.Fetch(session, scalars, columns, r.fetchCfg)
	if fetchReport != nil {
		r.telemetry.pdusSent.Add(ctx, int64(fetchReport.PDUs))
		// Stop asking for OIDs this device does not implement.
		state.compiled.MarkMissing(fetchReport.MissingOIDs...)
		for _, column := range fetchReport.TruncatedColumns {
			r.logger.Warn("table walk truncated by max_rows_per_column",
				zap.String("device", device.Address), zap.String("column", column))
		}
	}
	if fetchErr != nil {
		// A partial fetch still yields metrics, so this is logged rather than
		// treated as a failed poll.
		r.logger.Debug("partial fetch", zap.String("device", device.Address), zap.Error(fetchErr))
	}
	// A device that answered nothing is not healthy, whatever the transport did.
	// Absent OIDs are individually tolerated, so this total is the only signal
	// distinguishing an unreachable device from a sparsely populated one.
	if values == nil || values.Count() == 0 {
		if fetchErr == nil {
			fetchErr = fmt.Errorf("device answered none of the %d scalar and %d column OIDs",
				len(scalars), len(columns))
		}
		r.failPoll(ctx, device, fetchErr)
		return
	}

	info := report.DeviceInfo{
		ID:          device.ID,
		Address:     device.Address,
		Port:        device.Port,
		Subnet:      device.Subnet,
		SysObjectID: device.SysObjectID,
		ProfileName: state.profileName,
	}
	metrics, buildReport, buildErr := state.builder.Build(info, state.compiled, values, time.Now())
	if buildErr != nil {
		r.logger.Debug("build reported problems",
			zap.String("device", device.Address), zap.Error(buildErr))
	}

	if metrics.DataPointCount() > 0 {
		if err := r.consumer.ConsumeMetrics(ctx, metrics); err != nil {
			r.telemetry.pollErrors.Add(ctx, 1)
			r.logger.Error("pipeline rejected metrics",
				zap.String("device", device.Address), zap.Error(err))
			return
		}
	}

	r.devices.MarkSuccess(device.ID, time.Now())
	r.telemetry.pollDuration.Record(ctx, time.Since(started).Seconds())
	r.logger.Debug("polled device",
		zap.String("device", device.Address),
		zap.String("profile", state.profileName),
		zap.Int("datapoints", buildReport.DataPoints),
		zap.Int("unmapped_metrics", len(buildReport.GeneratedMetrics)))
}

// failPoll records a failed poll and drops the device once its budget is spent.
func (r *snmpReceiver) failPoll(ctx context.Context, device discovery.Device, cause error) {
	r.telemetry.pollErrors.Add(ctx, 1)
	if dropped := r.devices.MarkFailure(device.ID); dropped {
		r.logger.Info("device dropped after repeated failures",
			zap.String("device", device.Address), zap.Error(cause))
		r.forgetState(device.ID)
		return
	}
	r.logger.Debug("poll failed", zap.String("device", device.Address), zap.Error(cause))
}

// credentialsFor returns the credential set that answered for this device.
func (r *snmpReceiver) credentialsFor(device discovery.Device) (snmp.Credentials, error) {
	if device.Static {
		for _, configured := range r.cfg.Devices {
			host, _, err := parseEndpoint(configured.Endpoint)
			if err == nil && host == device.Address {
				return configured.Credentials, nil
			}
		}
		return snmp.Credentials{}, fmt.Errorf("device %s is no longer configured", device.Address)
	}

	for _, subnet := range r.cfg.Subnets {
		if subnet.Network != device.Subnet {
			continue
		}
		credentials := subnet.credentials()
		if device.AuthIndex < len(credentials) {
			return credentials[device.AuthIndex], nil
		}
		// The credential list shrank since discovery; fall back to the first.
		if len(credentials) > 0 {
			return credentials[0], nil
		}
	}
	return snmp.Credentials{}, fmt.Errorf("subnet %s is no longer configured", device.Subnet)
}

// ensureProfile resolves and compiles the device's profile, caching the result.
// The profile is re-detected if the device's sysObjectID changes, which happens
// when hardware is replaced at the same address.
func (r *snmpReceiver) ensureProfile(session snmp.Session, device discovery.Device) (*deviceState, error) {
	r.mu.Lock()
	state, ok := r.state[device.ID]
	if !ok {
		state = &deviceState{builder: report.NewBuilder(r.naming)}
		r.state[device.ID] = state
	}
	r.mu.Unlock()

	wanted := device.ProfileName
	if wanted == "" {
		sysObjectID := device.SysObjectID
		if sysObjectID == "" {
			// A static device has not been probed, so identify it now.
			detected, ok := discovery.ProbeSysObjectID(session)
			if !ok {
				return nil, fmt.Errorf("could not read sysObjectID to select a profile")
			}
			sysObjectID = detected
		}
		matched, err := r.profiles.Match(sysObjectID)
		if err != nil {
			return nil, fmt.Errorf("select profile: %w", err)
		}
		wanted = matched
		r.devices.SetProfile(device.ID, sysObjectID, wanted)
	}

	if state.compiled != nil && state.profileName == wanted {
		return state, nil
	}

	def, err := r.profiles.Resolve(wanted)
	if err != nil {
		return nil, fmt.Errorf("resolve profile %q: %w", wanted, err)
	}
	compiled, err := profile.Compile(def)
	if err != nil {
		return nil, fmt.Errorf("compile profile %q: %w", wanted, err)
	}
	state.compiled = compiled
	state.profileName = wanted
	r.logger.Info("profile selected",
		zap.String("device", device.Address),
		zap.String("profile", wanted),
		zap.Int("scalar_oids", len(compiled.ScalarOIDs)),
		zap.Int("column_oids", len(compiled.ColumnOIDs)))
	return state, nil
}

func (r *snmpReceiver) forgetState(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.state, id)
}
