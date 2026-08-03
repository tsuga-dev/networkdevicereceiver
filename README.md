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
| `naming.system_namespace_for_device_os` | `false` | Emit device cpu/memory as `system.*`. |
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
2. **`system.*` — device-OS metrics, opt-in.** `cpu.usage` and `memory.*` are the
   two most-referenced metric families in the profile library and `hw.*` has no
   home for either. `system.*` is the natural fit, but its conventions say that
   namespace is for metrics collected from *within* the target system, and SNMP
   polling is external. Rather than decide that silently, these default to tier 3
   and `naming.system_namespace_for_device_os: true` opts in.
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
| `ifHCIn*Pkts` / `ifHCOut*Pkts` | `hw.network.packets` | Counter | `{packet}` | `network.packet.class` for unicast/multicast/broadcast is not semconv yet |
| `ifInErrors` / `ifOutErrors` | `hw.errors` | Counter | `{error}` | `error.type=error` |
| `ifInDiscards` / `ifOutDiscards` | `hw.errors` | Counter | `{error}` | `error.type=discard`; discards are not strictly errors, flagged for review |
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
  exist upstream. Until they do, the two most-referenced metric families in the
  profile library have no `hw.*` home.
- Device dedupe currently removes only exact address duplicates; multi-homed
  devices need serial or engine-ID based identity.
- SNMP traps and LLDP topology are out of scope.

## Attribution

The profile schema, fetch strategy and profile library are ported from or
vendored out of [DataDog/datadog-agent](https://github.com/DataDog/datadog-agent)
and [DataDog/integrations-core](https://github.com/DataDog/integrations-core),
both Apache-2.0. The embedded profiles' upstream commit is recorded in
`internal/profile/default_profiles/UPSTREAM-SHA`.
