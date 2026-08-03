# Network Device Receiver

| Status    |                       |
| --------- | --------------------- |
| Stability | alpha: metrics        |
| Type      | `networkdevice`       |

Polls SNMP network devices at fleet scale. Declare subnets and credentials; the
receiver discovers devices, selects a device profile from `sysObjectID`, and
emits metrics under OpenTelemetry semantic conventions.

## Why this exists

The existing `snmpreceiver` models one receiver instance per device, with every
metric hand-declared. Monitoring two thousand devices means two thousand
configuration blocks and no SNMP probing. This receiver instead takes the three
things that make the Datadog agent's SNMP integration usable at scale — subnet
autodiscovery, `sysObjectID`-based profiles, and near-zero per-device
configuration — and puts an OpenTelemetry-native output stage behind them.

Roughly 25 lines of configuration covers two subnets and thousands of devices.
See [`examples/two-subnets.yaml`](./examples/two-subnets.yaml).

## Configuration

```yaml
receivers:
  networkdevice:
    collection_interval: 60s
    subnets:
      - network: 10.10.0.0/24
        version: v2c
        community: ${env:SNMP_COMMUNITY}
```

| Field | Default | Description |
| --- | --- | --- |
| `collection_interval` | `60s` | How often each device is polled. Polls are spread across the interval rather than issued together. |
| `timeout` / `retries` | `5s` / `3` | Per-request SNMP timeout and retry count. |
| `pollers` | `50` | Maximum devices polled concurrently. |
| `discovery.rediscovery_interval` | `1h` | How often subnets are rescanned. Set to `0` to scan once at startup only. |
| `discovery.workers` | `10` | Concurrent probes during a scan. |
| `discovery.allowed_failures` | `3` | Consecutive failures tolerated before a device is forgotten. |
| `discovery.dedupe` | `false` | Drop devices reachable at more than one address. |
| `subnets[]` | — | CIDR plus credentials. See below. |
| `devices[]` | — | Explicitly configured devices, never aged out. |
| `profiles.user_dir` | — | Profiles that shadow the embedded ones by name. |
| `naming.scheme` | `semconv` | `semconv`, `datadog_compat` or `both`. |
| `naming.fallback_namespace` | `snmp` | Prefix for generated names of unmodelled symbols. |
| `naming.system_namespace_for_device_os` | `true` | Emit device cpu/memory as `system.*`. Set `false` for the fallback namespace. |
| `fetch.oid_batch_size` | `10` | OIDs per GET or GETBULK. Shrinks automatically if a device rejects a request. |
| `fetch.bulk_max_repetitions` | `10` | GETBULK max-repetitions. |
| `fetch.max_rows_per_column` | `10000` | Cap on one table walk; truncation is logged, never silent. |
| `storage` | — | A storage extension used to persist the discovered device set. |

### Credentials

A single set can be given inline on a subnet. Several go under
`authentications` and are tried in order per device; the set that answers is
remembered, so later polls do not retry the others.

SNMPv1, v2c and v3 are supported. For v3, the security level is derived from
which secrets are present — supplying an `auth_key` gives authNoPriv, adding a
`priv_key` gives authPriv — so it cannot be stated inconsistently. Auth
protocols: MD5, SHA, SHA224, SHA256, SHA384, SHA512. Privacy protocols: DES,
AES, AES192, AES256, and the Cisco `AES192C`/`AES256C` key-extension variants.

### Persistence

Without a `storage` extension the device set is in-memory, and a restart
rescans every subnet before monitoring resumes. With one, devices come back
immediately along with the credential index and profile that already worked.
Statically configured devices are deliberately not persisted, so removing one
from the configuration actually removes it.

## Profiles

Profiles are read in Datadog's SNMP profile format, which is parsed natively
rather than converted. About 240 upstream profiles are embedded (Cisco, Juniper,
Arista, Fortinet, Palo Alto, F5, APC, NetApp, Meraki and more), including the
abstract ones such as `_base.yaml` and `_generic-if.yaml` that vendor profiles
extend. An existing Datadog `snmp.d/profiles` directory can be pointed at
unchanged via `profiles.user_dir`; a user profile shadows an embedded one of the
same name entirely.

Profile selection reads `sysObjectID` on first contact and picks the profile
whose `sysobjectid` glob is the most specific match. `generic-device.yaml`
claims `1.3.6.1.4.*`, so an unrecognised enterprise device still gets basic
monitoring rather than nothing.

Validate profiles before deploying them:

```console
$ go run ./cmd/snmpprofilecheck -user-dir /etc/otelcol/snmp.d/profiles
$ go run ./cmd/snmpprofilecheck -profile cisco-catalyst -coverage
$ go run ./cmd/snmpprofilecheck -sysobjectid 1.3.6.1.4.1.9.1.1745
```

## Metric naming

Profiles say *what to collect*; a separate naming registry says *what to call
it*. This split is deliberate: it means the upstream profile library can be
resynced without touching naming, and naming can follow the semantic
conventions without forking 240 files.

Three tiers:

1. **`hw.*` — the hardware semantic conventions.** The right home for interface,
   sensor, fan, PSU, voltage, temperature and status metrics; `hw.network.*` is
   explicitly defined as covering a physical interface on a switch, router or
   firewall. Device identity lives on the **resource**, and component identity
   in the `hw.id` / `hw.name` / `hw.type` datapoint attributes, as the
   conventions require. One resource per device.
2. **`system.*` — device-OS metrics, on by default.** `cpu.usage` and `memory.*`
   are the two most-referenced metric families in the profile library, and `hw.*`
   defines nothing for either: `hw.cpu.*` is only `speed` and `speed.limit`,
   `hw.memory.*` only `size`. Meanwhile `system.cpu.utilization`,
   `system.memory.usage` (with `system.memory.state`) and
   `system.memory.utilization` are exactly the right shapes.

   The objection is that the system conventions say that namespace is for metrics
   collected from *within* the target system, and SNMP polling is external. But an
   SNMP agent **is** in-system instrumentation merely transported over the wire —
   the same reading by which a remotely scraped node_exporter yields `system.*` —
   and the alternative is inventing three `hw.*` metrics that do not exist. So
   `system.*` is the default; `naming.system_namespace_for_device_os: false` opts
   out.

   This costs little to adopt because Datadog already normalises every vendor's
   CPU and memory OIDs onto the same symbol names — Cisco's `cpmCPUTotal5minRev`
   and HOST-RESOURCES' `hrProcessorLoad` are both `cpu.usage` — so a handful of
   entries covers the whole library.
3. **`snmp.*` — generated fallback.** For the long tail of vendor symbols, a
   deterministic `snmp.<mib>.<symbol>` derived only from the MIB and symbol
   names, so it is stable across profile resyncs. Naming a namespace after the
   transport is against semconv guidance, but it honestly signals "raw
   MIB-derived, not yet modelled" and is configurable.

Current coverage, measurable with `snmpprofilecheck`: **94%** of `_generic-if`'s
symbols (the profile 136 of 240 others inherit) and about **21%** of symbol
references across the whole corpus. The remaining tail resolves to tier 3.

### Worked IF-MIB mapping

| Profile symbol | Metric | Instrument | Unit | Notes |
| --- | --- | --- | --- | --- |
| `ifHCInOctets` / `ifHCOutOctets` | `hw.network.io` | Counter | `By` | `network.io.direction` distinguishes them |
| `ifHCIn*Pkts` / `ifHCOut*Pkts` | `hw.network.packets` | Counter | `{packet}` | `network.io.cast` for unicast/multicast/broadcast is ours; no convention models it |
| `ifInErrors` / `ifOutErrors` | `hw.errors` | Counter | `{error}` | `error.type=error` |
| `ifInDiscards` / `ifOutDiscards` | `system.network.packet.dropped` | Counter | `{packet}` | A discard is not an error; `hw.*` has no dropped-packet metric |
| `ifOperStatus` | `hw.network.up` | UpDownCounter | `1` | `up(1)` maps to 1, everything else 0 |
| `ifAdminStatus` | `hw.status` | UpDownCounter | `1` | State-set: one datapoint per state |
| `ifHighSpeed` | `hw.network.bandwidth.limit` | UpDownCounter | `By/s` | Mbit/s × 125000 |
| `ifSpeed` | `hw.network.bandwidth.limit` | UpDownCounter | `By/s` | bit/s ÷ 8; lower priority than `ifHighSpeed` |
| derived | `hw.network.bandwidth.utilization` | Gauge | `1` | A **fraction**, not a percentage |

Three mechanics beyond a flat symbol-to-name table:

- **State-set fan-out.** `hw.status` follows the OpenMetrics StateSet pattern, so
  an SNMP enum becomes one datapoint per possible state (1 for the active state,
  0 for the others) rather than one enum-valued gauge.
- **Type-dispatched sensors.** `entPhySensorValue` is one column whose meaning
  depends on the sibling `entPhySensorType` column, so it routes per row to
  `hw.temperature`, `hw.voltage`, `hw.fan.speed` or `hw.power`.
- **Priority.** When two symbols map to the same metric *and* the same
  attributes — a 32-bit and a 64-bit form of one counter, or `ifSpeed` alongside
  `ifHighSpeed` — priority picks one instead of emitting conflicting duplicate
  series.
- **Component identity outside `hw.*`.** A component metric with no `hw.*` home
  still needs to be joinable to the ones that have it, so an entry can opt into
  emitting `hw.id`, `hw.name` and `network.interface.name`. Interface discards use
  this.

### Sensor scaling

ENTITY-SENSOR-MIB publishes each sensor's exponent in `entPhySensorScale` (an
SI-prefix enum where `units(9)` is 10⁰ and each step is three orders of
magnitude) and `entPhySensorPrecision` (decimal places already folded into the
integer). Both are applied, so a reading of 425 with precision 1 is 42.5 °C and
334 with precision 2 is 3.34 V — correct per device, with no divisor to configure.

Cisco's older CISCO-ENTITY-SENSOR-MIB is dispatched the same way, but **without**
the exponent: `entSensorScale` and `entSensorPrecision` exist in that MIB and no
shipped profile collects them, so a Cisco device reporting deci-celsius reads ten
times high until those two columns are added to a profile.

### Rates

Datadog computes rates agent-side. This receiver emits cumulative Sums and lets
the pipeline derive rates, so `monotonic_count_and_rate` collapses to a single
Sum. Dashboards ported from a Datadog agent need `rate()`-style queries, or a
`cumulativetodelta` + `deltatorate` processor pair.

The one exception is `hw.network.bandwidth.utilization`, which stays
receiver-side because it needs a join between the traffic counters and
`ifHighSpeed` that a backend cannot easily express. It requires two polls before
it reports, and is suppressed across a counter reset rather than reporting a
false spike.

## Self-telemetry

| Metric | Description |
| --- | --- |
| `otelcol_networkdevice_devices_monitored` | Devices currently in the registry |
| `otelcol_networkdevice_discovered_devices` | Devices found by scans |
| `otelcol_networkdevice_poll_duration` | Per-device poll duration, seconds |
| `otelcol_networkdevice_poll_errors` | Failed polls, including poller saturation |
| `otelcol_networkdevice_pdus_sent` | SNMP request packets sent |

## Scale notes

- Column walks request only the columns a profile needs, never the table root.
- GETBULK batches several columns into one PDU, falling back to GETNEXT for
  SNMPv1 and for devices whose GETBULK is broken.
- Batch sizes shrink when a device answers `tooBig` and recover afterwards.
- OIDs a device does not implement are pruned after the first poll, so an
  unsupported OID is asked for once rather than forever.
- Polls are spread deterministically across the collection interval, keeping PDU
  rate flat instead of bursting at the top of each cycle. A device keeps its slot
  across restarts.
- Subnets wider than 65536 addresses are refused rather than attempted; raise
  `max_hosts` per subnet to override.

## Known gaps

- The `hw.*` conventions are Development status and `network.io.direction` is
  Release Candidate. Expect at least one breaking rename before they stabilise.
- Registry coverage beyond IF-MIB, ENTITY-SENSOR-MIB and the common UPS symbols
  is incomplete; the remaining vendor symbols resolve to tier 3.
- `hw.cpu.utilization`, `hw.memory.usage` and `hw.memory.utilization` do not
  exist upstream, so the two most-referenced metric families in the profile
  library are reported under `system.*` instead. Proposing them for `hw.*` is
  still worthwhile; if accepted, this becomes a rename behind a feature gate.
- `network.io.cast` and the `firewall.*`, `wireless.*` namespaces are ours: no
  convention models them. They are candidates for upstream proposals.
- Cisco entity sensors get no exponent, as described under Sensor scaling.
- Device dedupe currently removes only exact address duplicates; multi-homed
  devices need serial or engine-ID based identity.
- SNMP traps and LLDP topology are out of scope.

## Attribution

The profile schema, fetch strategy and profile library are ported from or
vendored out of [DataDog/datadog-agent](https://github.com/DataDog/datadog-agent)
and [DataDog/integrations-core](https://github.com/DataDog/integrations-core),
both Apache-2.0. The embedded profiles' upstream commit is recorded in
`internal/profile/default_profiles/UPSTREAM-SHA`.
