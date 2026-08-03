// Command snmpprobe polls real devices and prints what the receiver would emit.
//
// It runs the same discovery, profile and reporting code as the receiver, but
// without a collector: point it at a device or a subnet and it prints the
// resolved profile and the resulting metrics. Use it to check a device is
// reachable, see which profile matches it, and inspect the metrics before
// wiring the receiver into a pipeline.
//
//	snmpprobe -target 192.168.1.1 -community public
//	snmpprobe -target 192.168.1.0/24 -community public
//	snmpprobe -target 10.0.0.5 -version v3 -user monitor -auth-key secret -auth-protocol SHA256
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"go.opentelemetry.io/collector/pdata/pmetric"

	"github.com/tsuga-dev/networkdevicereceiver/internal/discovery"
	"github.com/tsuga-dev/networkdevicereceiver/internal/naming"
	"github.com/tsuga-dev/networkdevicereceiver/internal/profile"
	"github.com/tsuga-dev/networkdevicereceiver/internal/report"
	"github.com/tsuga-dev/networkdevicereceiver/internal/snmp"
)

type options struct {
	target  string
	port    uint
	timeout time.Duration
	retries int
	workers int

	version      string
	community    string
	user         string
	authProtocol string
	authKey      string
	privProtocol string
	privKey      string

	profileName string
	userDir     string

	polls    int
	interval time.Duration

	systemNamespace bool
	compat          bool
	showAttributes  bool
	jsonOut         bool
	maxHosts        int
}

func main() {
	var opts options
	flag.StringVar(&opts.target, "target", "", "device address, or a CIDR to scan (required)")
	flag.UintVar(&opts.port, "port", 161, "SNMP port")
	flag.DurationVar(&opts.timeout, "timeout", 3*time.Second, "per-request timeout")
	flag.IntVar(&opts.retries, "retries", 1, "per-request retries")
	flag.IntVar(&opts.workers, "workers", 20, "concurrent probes when scanning a CIDR")

	flag.StringVar(&opts.version, "version", "v2c", "SNMP version: v1, v2c or v3")
	flag.StringVar(&opts.community, "community", "public", "v1/v2c community")
	flag.StringVar(&opts.user, "user", "", "v3 user")
	flag.StringVar(&opts.authProtocol, "auth-protocol", "", "v3 auth protocol: MD5, SHA, SHA224, SHA256, SHA384, SHA512")
	flag.StringVar(&opts.authKey, "auth-key", "", "v3 auth passphrase")
	flag.StringVar(&opts.privProtocol, "priv-protocol", "", "v3 privacy protocol: DES, AES, AES192, AES256, AES192C, AES256C")
	flag.StringVar(&opts.privKey, "priv-key", "", "v3 privacy passphrase")

	flag.StringVar(&opts.profileName, "profile", "", "pin a profile instead of detecting one from sysObjectID")
	flag.StringVar(&opts.userDir, "user-dir", "", "directory of user profiles")

	flag.IntVar(&opts.polls, "polls", 1, "how many times to poll; 2 or more lets derived rates appear")
	flag.DurationVar(&opts.interval, "interval", 10*time.Second, "delay between polls")

	flag.BoolVar(&opts.systemNamespace, "system-namespace", false, "emit device cpu/memory as system.*")
	flag.BoolVar(&opts.compat, "datadog-compat", false, "emit Datadog-style snmp.<symbol> names")
	flag.BoolVar(&opts.showAttributes, "attributes", false, "print every datapoint with its attributes")
	flag.BoolVar(&opts.jsonOut, "json", false, "print the metrics as OTLP JSON")
	flag.IntVar(&opts.maxHosts, "max-hosts", 0, "override the limit on addresses per subnet")
	flag.Parse()

	if opts.target == "" {
		fmt.Fprintln(os.Stderr, "snmpprobe: -target is required")
		flag.Usage()
		os.Exit(2)
	}

	if err := run(opts); err != nil {
		fmt.Fprintf(os.Stderr, "snmpprobe: %v\n", err)
		os.Exit(1)
	}
}

func run(opts options) error {
	credentials := snmp.Credentials{
		Version:      opts.version,
		Community:    opts.community,
		User:         opts.user,
		AuthProtocol: opts.authProtocol,
		AuthKey:      opts.authKey,
		PrivProtocol: opts.privProtocol,
		PrivKey:      opts.privKey,
	}
	// v3 has no community; leaving the default set would fail validation.
	if strings.Contains(opts.version, "3") {
		credentials.Community = ""
	}
	if err := credentials.Validate(); err != nil {
		return err
	}

	profiles, err := profile.NewStore(opts.userDir)
	if err != nil {
		return err
	}
	namingOpts := naming.DefaultOptions()
	namingOpts.SystemNamespaceForDeviceOS = opts.systemNamespace
	if opts.compat {
		namingOpts.Scheme = naming.SchemeDatadogCompat
	}
	registry, err := naming.New(namingOpts)
	if err != nil {
		return err
	}

	devices, err := findDevices(opts, credentials)
	if err != nil {
		return err
	}
	if len(devices) == 0 {
		return fmt.Errorf("no device answered; check the address, credentials, and that SNMP is enabled")
	}

	fmt.Printf("%d device(s) answered\n\n", len(devices))
	for _, device := range devices {
		if err := pollDevice(opts, credentials, profiles, registry, device); err != nil {
			fmt.Printf("  %s: %v\n\n", device.Address, err)
		}
	}
	return nil
}

// findDevices probes one address, or scans a CIDR.
func findDevices(opts options, credentials snmp.Credentials) ([]discovery.Device, error) {
	if !strings.Contains(opts.target, "/") {
		return []discovery.Device{{
			ID:      opts.target,
			Address: opts.target,
			Port:    uint16(opts.port),
		}}, nil
	}

	addresses, err := discovery.ExpandCIDR(opts.target, opts.maxHosts)
	if err != nil {
		return nil, err
	}
	fmt.Printf("scanning %s (%d addresses, %d workers)...\n", opts.target, len(addresses), opts.workers)

	scanner := &discovery.Scanner{Workers: opts.workers, Dial: snmp.NewSession}
	found, err := scanner.Scan(context.Background(), discovery.SubnetConfig{
		Network:     opts.target,
		Port:        uint16(opts.port),
		Credentials: []snmp.Credentials{credentials},
		Timeout:     opts.timeout,
		Retries:     opts.retries,
		MaxHosts:    opts.maxHosts,
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(found, func(i, j int) bool { return found[i].Address < found[j].Address })
	return found, nil
}

func pollDevice(opts options, credentials snmp.Credentials, profiles *profile.Store,
	registry *naming.Registry, device discovery.Device) error {

	session, err := snmp.NewSession(snmp.ConnectionConfig{
		Host:        device.Address,
		Port:        device.Port,
		Credentials: credentials,
		Timeout:     opts.timeout,
		Retries:     opts.retries,
	})
	if err != nil {
		return err
	}
	if err := session.Connect(); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = session.Close() }()

	// Identify the device. sysName is read purely so the operator can confirm
	// they are looking at the box they meant.
	sysObjectID, sysName := device.SysObjectID, ""
	store, _, _ := snmp.Fetch(session,
		[]string{discovery.OIDSysObjectID, discovery.OIDSysName}, nil, snmp.FetchConfig{})
	if store != nil {
		if v, err := store.Scalar(discovery.OIDSysObjectID); err == nil {
			sysObjectID = v.String()
		}
		if v, err := store.Scalar(discovery.OIDSysName); err == nil {
			sysName = v.String()
		}
	}

	profileName := opts.profileName
	switch {
	case profileName != "":
	case sysObjectID == "":
		return fmt.Errorf("no sysObjectID returned, so no profile can be selected; pin one with -profile")
	default:
		matched, err := profiles.Match(sysObjectID)
		if err != nil {
			return fmt.Errorf("%w (pin one with -profile)", err)
		}
		profileName = matched
	}

	fmt.Printf("=== %s\n", device.Address)
	if sysName != "" {
		fmt.Printf("    sysName     %s\n", sysName)
	}
	fmt.Printf("    sysObjectID %s\n", sysObjectID)
	fmt.Printf("    profile     %s\n", profileName)

	def, err := profiles.Resolve(profileName)
	if err != nil {
		return err
	}
	compiled, err := profile.Compile(def)
	if err != nil {
		return err
	}
	fmt.Printf("    collecting  %d scalar OIDs, %d column OIDs\n",
		len(compiled.ScalarOIDs), len(compiled.ColumnOIDs))

	builder := report.NewBuilder(registry)
	info := report.DeviceInfo{
		ID:          device.Address,
		Address:     device.Address,
		Port:        device.Port,
		Subnet:      device.Subnet,
		SysObjectID: sysObjectID,
		ProfileName: profileName,
	}

	var metrics pmetric.Metrics
	for poll := 1; poll <= opts.polls; poll++ {
		if poll > 1 {
			fmt.Printf("    waiting %s before poll %d...\n", opts.interval, poll)
			time.Sleep(opts.interval)
		}

		started := time.Now()
		scalars, columns := compiled.FetchOIDs()
		// fetchErr is only fatal when nothing came back: a partial poll is the
		// normal case and still yields metrics.
		values, fetchReport, fetchErr := snmp.Fetch(session, scalars, columns, snmp.FetchConfig{})
		if values == nil {
			return fmt.Errorf("fetch: %w", fetchErr)
		}
		if fetchReport != nil {
			compiled.MarkMissing(fetchReport.MissingOIDs...)
		}

		md, buildReport, buildErr := builder.Build(info, compiled, values, time.Now())
		metrics = md

		fmt.Printf("    poll %d: %d values in %d PDUs (%s), %d datapoints, %d OIDs pruned\n",
			poll, values.Count(), fetchReport.PDUs, time.Since(started).Round(time.Millisecond),
			buildReport.DataPoints, compiled.MissingCount())

		if values.Count() == 0 {
			return fmt.Errorf("the device answered none of its profile's OIDs; " +
				"it may restrict the community to a subset of the MIB tree")
		}
		if buildErr != nil && poll == opts.polls {
			// Usually just absent optional columns; shown because it explains
			// missing metrics.
			fmt.Printf("    notes: %s\n", firstLines(buildErr.Error(), 3))
		}
	}

	fmt.Println()
	if opts.jsonOut {
		data, err := (&pmetric.JSONMarshaler{}).MarshalMetrics(metrics)
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		return nil
	}
	printMetrics(metrics, opts.showAttributes)
	fmt.Println()
	return nil
}

func printMetrics(md pmetric.Metrics, showAttributes bool) {
	for i := 0; i < md.ResourceMetrics().Len(); i++ {
		rm := md.ResourceMetrics().At(i)

		fmt.Println("    resource:")
		keys := make([]string, 0, rm.Resource().Attributes().Len())
		for k := range rm.Resource().Attributes().All() {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v, _ := rm.Resource().Attributes().Get(k)
			fmt.Printf("      %-32s %s\n", k, v.AsString())
		}

		fmt.Println("    metrics:")
		sms := rm.ScopeMetrics()
		for j := 0; j < sms.Len(); j++ {
			ms := sms.At(j).Metrics()
			type row struct {
				name, unit, kind string
				points           pmetric.NumberDataPointSlice
			}
			rows := make([]row, 0, ms.Len())
			for k := 0; k < ms.Len(); k++ {
				m := ms.At(k)
				rows = append(rows, row{m.Name(), m.Unit(), instrumentLabel(m), pointsOf(m)})
			}
			sort.Slice(rows, func(a, b int) bool { return rows[a].name < rows[b].name })

			for _, r := range rows {
				fmt.Printf("      %-42s %-14s %-16s %3d points\n",
					r.name, r.kind, unitLabel(r.unit), r.points.Len())
				if !showAttributes {
					continue
				}
				for p := 0; p < r.points.Len(); p++ {
					dp := r.points.At(p)
					fmt.Printf("          %-14g %s\n", dp.DoubleValue(), formatAttributes(dp))
				}
			}
		}
	}
}

func pointsOf(m pmetric.Metric) pmetric.NumberDataPointSlice {
	switch m.Type() {
	case pmetric.MetricTypeGauge:
		return m.Gauge().DataPoints()
	case pmetric.MetricTypeSum:
		return m.Sum().DataPoints()
	default:
		return pmetric.NewNumberDataPointSlice()
	}
}

func instrumentLabel(m pmetric.Metric) string {
	switch m.Type() {
	case pmetric.MetricTypeGauge:
		return "gauge"
	case pmetric.MetricTypeSum:
		if m.Sum().IsMonotonic() {
			return "sum"
		}
		return "updowncounter"
	default:
		return m.Type().String()
	}
}

func unitLabel(unit string) string {
	if unit == "" {
		return "-"
	}
	return unit
}

func formatAttributes(dp pmetric.NumberDataPoint) string {
	pairs := make([]string, 0, dp.Attributes().Len())
	for k, v := range dp.Attributes().All() {
		pairs = append(pairs, k+"="+v.AsString())
	}
	sort.Strings(pairs)
	return strings.Join(pairs, " ")
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = append(lines[:n], fmt.Sprintf("... and %d more", len(lines)-n))
	}
	return strings.Join(lines, "; ")
}
