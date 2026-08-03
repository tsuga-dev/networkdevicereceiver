[comment]: <> (Partly generated. The per-metric reference between the markers below comes from internal/naming/registry.yaml via `go generate ./...`; everything else is hand-written.)

# networkdevice

## About this document

Metric names are not fixed by the receiver. What a device emits depends on which
profile matched its `sysObjectID` and on which symbols that profile collects, so
`metadata.yaml` declares no metrics and mdatagen cannot generate this file.

What *is* fixed is the mapping: a given profile symbol always resolves to the
same metric name, unit, instrument and attributes. That mapping lives in
[`internal/naming/registry.yaml`](./internal/naming/registry.yaml), and the
[Metrics](#metrics) section below is generated from it — including each metric's
description and notes, which are kept in that file's `metric_docs` section.
Everything after it is hand-written, because the transformations and the fallback
naming rules are not expressible as registry data.

To see what a specific profile would produce:

```console
$ go run ./cmd/snmpprofilecheck -profile cisco-catalyst -coverage
```

All datapoint values are emitted as `Double`, including counters: SNMP gauges and
counters are converted through `float64` and scale factors may be fractional.

## Metrics

Curated mappings, one subsection per emitted metric, listing every profile symbol
that feeds it. Symbols not listed here are still emitted, under a generated name —
see [Unmapped symbols](#unmapped-symbols).

Some source names are Datadog's normalised symbol names rather than MIB object
names. The profile library already maps every vendor's CPU and memory OIDs onto
them, so Cisco's `cpmCPUTotal5minRev` and HOST-RESOURCES' `hrProcessorLoad` both
arrive as `cpu.usage`, and a handful of entries covers the whole library.

<!-- BEGIN GENERATED: metrics -->

### hw.battery.charge

UPS battery charge remaining, as a fraction of capacity.

| Unit | Metric Type | Value Type | Monotonic |
| ---- | ----------- | ---------- | --------- |
| 1 | Gauge | Double | n/a |

| Source symbol | Attributes | Scale |
| --- | --- | --- |
| `upsAdvBatteryCapacity` | `hw.type=battery` | × 0.01 |
| `upsEstimatedChargeRemaining` | `hw.type=battery` | × 0.01 |

Both symbols report a percentage, scaled here to the fraction the
convention's unit requires.

### hw.battery.time_left

Estimated UPS runtime remaining on battery.

| Unit | Metric Type | Value Type | Monotonic |
| ---- | ----------- | ---------- | --------- |
| s | Gauge | Double | n/a |

| Source symbol | Attributes | Scale |
| --- | --- | --- |
| `upsEstimatedMinutesRemaining` | `hw.type=battery` | × 60 |

`upsEstimatedMinutesRemaining` reports minutes.

### hw.errors

Interface errors.

| Unit | Metric Type | Value Type | Monotonic |
| ---- | ----------- | ---------- | --------- |
| {error} | Sum | Double | true |

| Source symbol | Attributes |
| --- | --- |
| `ifInErrors` | `error.type=error`, `hw.type=network`, `network.io.direction=receive` |
| `ifOutErrors` | `error.type=error`, `hw.type=network`, `network.io.direction=transmit` |

A discard is a deliberate drop rather than an error, and is reported
separately as `system.network.packet.dropped`.

### hw.fan.speed

Fan speed, from an entity sensor.

| Unit | Metric Type | Value Type | Monotonic |
| ---- | ----------- | ---------- | --------- |
| rpm | Gauge | Double | n/a |

| Source symbol | Condition | Attributes | Scale |
| --- | --- | --- | --- |
| `entPhySensorValue` | `entPhySensorType` = `10` | `hw.type=fan` | per-row, from `entPhySensorScale` and `entPhySensorPrecision` |

### hw.network.bandwidth.limit

Interface capacity.

| Unit | Metric Type | Value Type | Monotonic |
| ---- | ----------- | ---------- | --------- |
| By/s | Sum | Double | false |

| Source symbol | Attributes | Scale | Priority |
| --- | --- | --- | --- |
| `ifHighSpeed` | `hw.type=network` | × 125000 | 10 |
| `ifSpeed` | `hw.type=network` | × 0.125 | 1 |

`ifHighSpeed` reports Mbit/s and `ifSpeed` bit/s. Where a profile collects
both they map to the same metric and the same attributes, so priority
decides: `ifSpeed` saturates at 4.29 Gbit/s.

The declared instrument overrides the profile here. Profiles call these
gauges, but the conventions fix this metric as an UpDownCounter.

### hw.network.io

Bytes transferred on a physical interface.

| Unit | Metric Type | Value Type | Monotonic |
| ---- | ----------- | ---------- | --------- |
| By | Sum | Double | true |

| Source symbol | Attributes |
| --- | --- |
| `ifHCInOctets` | `hw.type=network`, `network.io.direction=receive` |
| `ifHCOutOctets` | `hw.type=network`, `network.io.direction=transmit` |

### hw.network.packets

Packets transferred on a physical interface, split by packet class.

| Unit | Metric Type | Value Type | Monotonic |
| ---- | ----------- | ---------- | --------- |
| {packet} | Sum | Double | true |

| Source symbol | Attributes |
| --- | --- |
| `ifHCInBroadcastPkts` | `hw.type=network`, `network.io.cast=broadcast`, `network.io.direction=receive` |
| `ifHCInMulticastPkts` | `hw.type=network`, `network.io.cast=multicast`, `network.io.direction=receive` |
| `ifHCInUcastPkts` | `hw.type=network`, `network.io.cast=unicast`, `network.io.direction=receive` |
| `ifHCOutBroadcastPkts` | `hw.type=network`, `network.io.cast=broadcast`, `network.io.direction=transmit` |
| `ifHCOutMulticastPkts` | `hw.type=network`, `network.io.cast=multicast`, `network.io.direction=transmit` |
| `ifHCOutUcastPkts` | `hw.type=network`, `network.io.cast=unicast`, `network.io.direction=transmit` |

`network.io.cast` is not a semantic convention; no convention models the
unicast/multicast/broadcast split.

### hw.network.up

Interface operational state as a boolean.

| Unit | Metric Type | Value Type | Monotonic |
| ---- | ----------- | ---------- | --------- |
| 1 | Sum | Double | false |

| Source symbol | Attributes | Value mapping |
| --- | --- | --- |
| `ifOperStatus` | `hw.type=network` | `1` → 1, anything else → 0 |

### hw.power

Power drawn, from an entity sensor.

| Unit | Metric Type | Value Type | Monotonic |
| ---- | ----------- | ---------- | --------- |
| W | Gauge | Double | n/a |

| Source symbol | Condition | Scale |
| --- | --- | --- |
| `entPhySensorValue` | `entPhySensorType` = `6` | per-row, from `entPhySensorScale` and `entPhySensorPrecision` |

### hw.status

Interface administrative state, as an OpenMetrics StateSet: one datapoint
per possible state, 1 for the active one and 0 for the rest.

| Unit | Metric Type | Value Type | Monotonic |
| ---- | ----------- | ---------- | --------- |
| 1 | Sum | Double | false |

| Source symbol | Attributes | State mapping |
| --- | --- | --- |
| `ifAdminStatus` | `hw.type=network`, `hw.state` ∈ {`ok`, `degraded`, `failed`} | `1` → `ok`, `2` → `failed`, `3` → `degraded` |

A value outside the mapping yields all-zero datapoints rather than none, so
a device in an unexpected state reads as "no known state active" instead of
dropping out of the series.

### hw.temperature

Temperature, from an entity sensor or a UPS battery.

| Unit | Metric Type | Value Type | Monotonic |
| ---- | ----------- | ---------- | --------- |
| Cel | Gauge | Double | n/a |

| Source symbol | Condition | Attributes | Scale |
| --- | --- | --- | --- |
| `entPhySensorValue` | `entPhySensorType` = `8` | `hw.type=temperature` | per-row, from `entPhySensorScale` and `entPhySensorPrecision` |
| `upsBatteryTemperature` |  | `hw.type=temperature` |  |

### hw.voltage

Voltage, from an entity sensor, a UPS battery or a UPS input line.

| Unit | Metric Type | Value Type | Monotonic |
| ---- | ----------- | ---------- | --------- |
| V | Gauge | Double | n/a |

| Source symbol | Condition | Attributes | Scale |
| --- | --- | --- | --- |
| `entPhySensorValue` | `entPhySensorType` = `3` | `hw.type=voltage` | per-row, from `entPhySensorScale` and `entPhySensorPrecision` |
| `entPhySensorValue` | `entPhySensorType` = `4` | `hw.type=voltage` | per-row, from `entPhySensorScale` and `entPhySensorPrecision` |
| `upsBatteryVoltage` |  | `hw.type=voltage` |  |
| `upsHighPrecInputLineVoltage` |  | `hw.type=voltage` | × 0.1 |

`upsHighPrecInputLineVoltage` reports tenths of a volt.

### system.cpu.utilization

Device CPU utilisation, as a fraction.

| Unit | Metric Type | Value Type | Monotonic |
| ---- | ----------- | ---------- | --------- |
| 1 | Gauge | Double | n/a |

| Source symbol | Scale |
| --- | --- |
| `cpu.usage` | × 0.01 |

Emitted only when `naming.system_namespace_for_device_os` is `true`, its
default. Set it to `false` and these symbols resolve to generated `snmp.*`
names instead.

The symbol reports a percentage, scaled here to a fraction.

### system.memory.limit

Total device memory.

| Unit | Metric Type | Value Type | Monotonic |
| ---- | ----------- | ---------- | --------- |
| By | Sum | Double | false |

| Source symbol |
| --- |
| `memory.total` |

Emitted only when `naming.system_namespace_for_device_os` is `true`, its
default. Set it to `false` and these symbols resolve to generated `snmp.*`
names instead.

### system.memory.usage

Device memory, by state.

| Unit | Metric Type | Value Type | Monotonic |
| ---- | ----------- | ---------- | --------- |
| By | Sum | Double | false |

| Source symbol | Attributes |
| --- | --- |
| `memory.free` | `system.memory.state=free` |
| `memory.used` | `system.memory.state=used` |

Emitted only when `naming.system_namespace_for_device_os` is `true`, its
default. Set it to `false` and these symbols resolve to generated `snmp.*`
names instead.

### system.memory.utilization

Device memory utilisation, as a fraction.

| Unit | Metric Type | Value Type | Monotonic |
| ---- | ----------- | ---------- | --------- |
| 1 | Gauge | Double | n/a |

| Source symbol | Scale |
| --- | --- |
| `memory.usage` | × 0.01 |

Emitted only when `naming.system_namespace_for_device_os` is `true`, its
default. Set it to `false` and these symbols resolve to generated `snmp.*`
names instead.

The symbol reports a percentage, scaled here to a fraction.

### system.network.connection.count

Established TCP connections.

| Unit | Metric Type | Value Type | Monotonic |
| ---- | ----------- | ---------- | --------- |
| {connection} | Sum | Double | false |

| Source symbol | Attributes |
| --- | --- |
| `tcpCurrEstab` | `network.connection.state=established`, `network.transport=tcp` |

### system.network.packet.dropped

Interface discards.

| Unit | Metric Type | Value Type | Monotonic |
| ---- | ----------- | ---------- | --------- |
| {packet} | Sum | Double | true |

| Source symbol | Attributes |
| --- | --- |
| `ifInDiscards` | `network.io.direction=receive` |
| `ifOutDiscards` | `network.io.direction=transmit` |

`hw.*` models no dropped-packet metric, so these live under `system.*` but
opt into the component-identity attributes (`hw.id`, `hw.name` and
`network.interface.name`), which keeps them joinable to the `hw.network.*`
metrics for the same interface.

### system.uptime

Time since the device's host resources were last initialised.

| Unit | Metric Type | Value Type | Monotonic |
| ---- | ----------- | ---------- | --------- |
| s | Gauge | Double | n/a |

| Source symbol | Scale |
| --- | --- |
| `hrSystemUptime` | × 0.01 |

`hrSystemUptime` reports TimeTicks, hundredths of a second.

<!-- END GENERATED: metrics -->

## Derived metrics

Computed by the receiver rather than mapped from a symbol, so they have no
registry entry.

### hw.network.bandwidth.utilization

The octet-counter delta since the previous poll divided by the interface's
capacity. A **fraction**, not a percentage.

| Unit | Metric Type | Value Type | Monotonic |
| ---- | ----------- | ---------- | --------- |
| 1 | Gauge | Double | n/a |

| Attributes |
| --- |
| `hw.type=network`, `network.io.direction` ∈ {`receive`, `transmit`}, `hw.id`, `hw.name` (when the profile names interfaces) |

Inputs, by OID, preferring the 64-bit counter where the device answers it:

| Purpose | OIDs, in preference order |
| --- | --- |
| receive bytes | `1.3.6.1.2.1.31.1.1.1.6` (`ifHCInOctets`), `1.3.6.1.2.1.2.2.1.10` (`ifInOctets`) |
| transmit bytes | `1.3.6.1.2.1.31.1.1.1.10` (`ifHCOutOctets`), `1.3.6.1.2.1.2.2.1.16` (`ifOutOctets`) |
| capacity | `1.3.6.1.2.1.31.1.1.1.15` (`ifHighSpeed`, × 125000), else `1.3.6.1.2.1.2.2.1.5` (`ifSpeed`, ÷ 8) |

Nothing is emitted for an interface until it has been polled twice, and nothing
is emitted across a counter reset — a reading below the previous one is dropped
rather than reported as a spike.

This is the one rate computed receiver-side, because it needs a join between two
tables that a metrics backend cannot easily express. See
[Rates](#rates-and-counters).

## Unmapped symbols

A symbol with no registry entry is still emitted, under a name derived from its
MIB and symbol name:

```
<naming.fallback_namespace>.<mib>.<symbol>
```

Both parts are snake_cased, and the conventional `-MIB` suffix is dropped:

| MIB | Symbol | Generated name |
| --- | --- | --- |
| `IF-MIB` | `ifInUnknownProtos` | `snmp.if.if_in_unknown_protos` |
| `CISCO-MEMORY-POOL-MIB` | `ciscoMemoryPoolUsed` | `snmp.cisco_memory_pool.cisco_memory_pool_used` |

Acronym runs stay together (`ifHCInOctets` → `if_hc_in_octets`, not
`if_h_c_in_octets`), characters invalid in an OTel metric name become `_`, and a
symbol that is already a dotted path keeps its dots as separators (`cpu.usage` →
`cpu.usage`).

Generated names depend only on the MIB and symbol names, so they are stable
across upstream profile syncs. Renaming one is a breaking change and goes behind
a feature gate.

These metrics carry the profile's tags as attributes, but no convention-defined
attributes and no `hw.*` component identity — the receiver does not claim to know
what they mean. The unit is empty and the instrument is inferred; see
[Instrument selection](#instrument-selection).

Each build reports the distinct generated names it produced, which is how
`snmpprofilecheck -coverage` measures the curation gap for a profile.

## Resource Attributes

One resource per device. Device identity lives here and not on datapoints, as the
hardware conventions require.

| Name | Description | Source |
| ---- | ----------- | ------ |
| `host.id` | Stable device identifier across polls and restarts | discovery |
| `host.name` | Device name; falls back to the IP address when no profile field yields one | profile metadata `name` or `sys_name` |
| `snmp.device.ip` | Device address | discovery |
| `snmp.device.sys_object_id` | `sysObjectID` read on first contact | discovery |
| `snmp.profile` | Name of the matched profile | profile selection |
| `snmp.subnet` | Configured CIDR the device was discovered in; absent for a statically configured device | configuration |
| `device.manufacturer` | | profile metadata `vendor` |
| `device.model.identifier` | | profile metadata `model` |
| `os.name` | | profile metadata `os_name` |
| `os.version` | | profile metadata `os_version` or `version` |
| `snmp.device.serial_number` | | profile metadata `serial_number` |
| `snmp.device.<field>` | Any other profile device-inventory field, verbatim | profile metadata |

Profile-level `metric_tags` also land here rather than on datapoints, because
they identify the device (`snmp_host` from `sysName`, for example). They do not
overwrite an attribute already set from the table above.

Within one inventory field, the profile's candidate symbols are tried in order
and the first that yields a non-empty value wins — which is how a profile tries a
vendor-specific OID before falling back to `sysDescr`.

## Datapoint attributes

Three sources, applied in this order — later ones win on collision:

1. Registry-declared attributes for the resolved entry (`hw.type`,
   `network.io.direction`, …), listed per metric above.
2. Profile `static_tags`, profile-level then per-metric. Written `key:value`; a
   bare tag with no colon becomes `key=true`.
3. Resolved profile `metric_tags` for the row.

Then component identity is added, on every `hw.*` metric and on any entry that
sets `component_identity`:

| Name | Value |
| ---- | ----- |
| `hw.id` | `<component>_<row index>` for a table symbol, e.g. `if_1`, `ent_phy_sensor_1013`; the snake_cased symbol name for a scalar |
| `hw.name` | The row's `interface`, `name`, `sensor` or `port` tag, first one present |
| `network.interface.name` | Same value as `hw.name`, added only outside `hw.*` |

`<component>` is the table name with a trailing `Table` dropped and snake_cased
(`entPhySensorTable` → `ent_phy_sensor`), or the table OID with `.` → `_` if the
profile leaves the table unnamed. `ifTable` (`1.3.6.1.2.1.2.2`) and `ifXTable`
(`1.3.6.1.2.1.31.1.1`) are both mapped to `if`, since they describe the same
interfaces and metrics from either must agree on `hw.id`.

A tag that cannot be resolved is skipped rather than failing the row, and an
empty tag value is dropped rather than emitted — devices routinely leave optional
columns such as `ifAlias` blank.

## Transformations

### Numeric values

Applied in order:

1. **`constant_value_one`** — the symbol reports `1` regardless of the value,
   which is how profiles count entities such as fans. No further steps apply.
2. **`flag_stream`** — the value is a string of `0`/`1` characters; the 1-based
   `placement` selects one bit, yielding `0` or `1`.
3. **Registry `value_map`** — an enum becomes a number by lookup, with a
   `default` key covering unlisted values (`ifOperStatus`).
4. **Parse** — the raw value to `float64`. Numbers arriving as octet strings are
   parsed, since devices not infrequently return one that way.
5. **Profile `scale_factor`** — multiplied, when non-zero.
6. **Registry `scale`** — multiplied, when neither zero nor one. Listed per
   metric above.

For sensor rows, step 6's scale also carries the per-row exponent; see
[Sensor scaling](#sensor-scaling).

### String values

Tag and metadata values pass through, in the order the Datadog agent applies
them:

1. **`format`** — `mac_address` renders packed octets as colon-separated
   lowercase hex; `ip_address` renders 4 or 16 octets as an address, IPv6 in its
   canonical compressed form. A value that is not raw octets is passed through,
   since some agents return it already formatted.
2. **`extract_value`** — a regex whose first capture group replaces the value.
3. **`match_pattern` / `match_value`** — a regex plus a template over its capture
   groups. Profiles write `$1`; it is rewritten to `${1}` so adjacent literal
   text is not absorbed into the group name.
4. **`mapping`** — a translation table for enums. A numeric key is retried after
   normalising, since a value read as a float renders as `4` while the table may
   be keyed `"4"`.
5. **UTF-8 sanitisation** — a value that is not valid UTF-8 is hex-encoded
   (`f47f3593af80`). SNMP OCTET STRING is arbitrary bytes and plenty of columns
   hold binary; OTLP string fields must be valid UTF-8, and a backend that
   validates rejects the entire request, so one binary column would otherwise
   destroy every metric for that device. A profile wanting colon-separated octets
   should declare `format: mac_address`, which runs first.

A `match` plus `tags` pair splits one value into several tags by capture group,
in place of steps 3–4.

### Row indexes

- **`index_transform`** re-slices a row index using inclusive arc ranges, which
  is how a tag from one table joins onto rows of another whose index is a subset.
  Cisco's WLC profiles need this: a radio table's index is an access point's
  six-arc MAC address plus a radio slot, so arcs 0–5 give the key into the access
  point table.
- **`index`** on a tag reads a 1-based arc out of the row index itself, with no
  column involved.
- Rows are emitted in OID order, not string order, so index `10` follows index
  `9`.

### State sets

An entry with `state_set` emits one datapoint per declared state instead of one
enum-valued gauge: `1` for the state the SNMP value maps to, `0` for every other.
This is the OpenMetrics StateSet representation the `hw.status` convention uses.

### Type-dispatched sensors

`entPhySensorValue` is one column whose meaning depends on the sibling
`entPhySensorType` column in the same row, so the target metric is resolved per
row. The dispatch values that appear in the tables above are ENTITY-SENSOR-MIB's
`EntitySensorDataType` enum:

| `entPhySensorType` | Meaning | Metric |
| --- | --- | --- |
| `3` | voltsAC | `hw.voltage` |
| `4` | voltsDC | `hw.voltage` |
| `6` | watts | `hw.power` |
| `8` | celsius | `hw.temperature` |
| `10` | rpm | `hw.fan.speed` |

A row whose type has no case — `percentRH`, say — is skipped and counted, not
filed under another metric.

### Sensor scaling

ENTITY-SENSOR-MIB publishes each sensor's magnitude alongside its value, and both
sibling columns are read per row:

- `entPhySensorScale` is an SI-prefix enum where `units(9)` is 10⁰ and each step
  is three orders of magnitude, contributing `3 × (scale − 9)` to the exponent.
  Values outside 1–17 are ignored.
- `entPhySensorPrecision` is the number of decimal places already folded into the
  integer, contributing `−precision`.

So a reading of 425 with precision 1 is 42.5 °C, and 334 with precision 2 is
3.34 V — correct per device, with no divisor to configure. A device that reports
neither column scales by 1.

Cisco's older CISCO-ENTITY-SENSOR-MIB is deliberately **not** mapped. Profiles
collect its `entSensorValue` and `entSensorType` but not `entSensorScale` and
`entSensorPrecision`, so the magnitude cannot be derived and a device reporting
deci-celsius would read ten times high. Those values resolve to generated `snmp.*`
names instead, where a reader does not assume they are conventional.

### Instrument selection

A curated entry's `instrument` wins, because the conventions fix the instrument
for `hw.*` metrics regardless of what a profile declares.

For a generated name there is no entry to trust, so the instrument is inferred
from the profile's `metric_type` — a symbol-level one overriding its metric's:

| `metric_type` | Instrument |
| --- | --- |
| `monotonic_count`, `monotonic_count_and_rate`, `rate` | Sum (cumulative, monotonic) |
| `gauge`, `flag_stream` | Gauge |
| anything else, or unset | Sum if the SNMP type is Counter32/Counter64, else Gauge |

A metric name must keep one instrument kind within a scope. Two symbols
disagreeing is a registry error: `go generate ./...` refuses to write this file,
and at runtime it fails the build for that device rather than silently mixing
them.

### Duplicate streams and priority

Two symbols can resolve to the same metric name *and* the same attribute set —
a 32-bit and a 64-bit form of one counter, or `ifSpeed` alongside `ifHighSpeed`.
The higher `priority` wins; a lower or equal one is dropped. Emitting both would
produce conflicting datapoints for one stream.

Datapoints that merely share a metric name are appended to the same metric, so
`hw.network.io` is one metric with a receive and a transmit datapoint per
interface rather than duplicate metric names in one scope.

### Rates and counters

Datadog computes rates agent-side. This receiver emits cumulative Sums and lets
the pipeline derive rates, so `monotonic_count_and_rate` collapses to a single
Sum rather than two streams. Dashboards ported from a Datadog agent need
`rate()`-style queries, or a `cumulativetodelta` + `deltatorate` processor pair.

Every cumulative stream carries a start timestamp, recorded the first time that
stream is seen, so a backend that did not observe the earlier points can still
derive a rate.

The one exception is
[`hw.network.bandwidth.utilization`](#hwnetworkbandwidthutilization).

### Partial polls

A poll that answers some OIDs and not others reports the metrics it can. An
absent scalar, an unresolvable tag and a symbol whose value will not parse are
each counted and skipped; errors are aggregated and logged once per poll rather
than per datapoint. A device answering ninety per cent of its OIDs reports ninety
per cent of its metrics.

## Naming schemes

`naming.scheme` selects how a resolved entry is rendered:

| Scheme | Emitted names |
| --- | --- |
| `semconv` (default) | The curated name, or the generated `snmp.<mib>.<symbol>` |
| `datadog_compat` | `snmp.<symbolName>` verbatim, keeping the curated entry's unit and attributes |
| `both` | Both of the above, for a migration window |

`datadog_compat` eases migration from a Datadog deployment whose dashboards use
those names. It changes only the metric name: units, attributes, scaling and
instrument still come from the registry entry.

## Internal Telemetry

| Name | Description | Unit | Metric Type |
| ---- | ----------- | ---- | ----------- |
| `otelcol_networkdevice_devices_monitored` | Devices currently in the registry | | Gauge |
| `otelcol_networkdevice_discovered_devices` | Devices found by scans | | Sum |
| `otelcol_networkdevice_poll_duration` | Per-device poll duration | s | Histogram |
| `otelcol_networkdevice_poll_errors` | Failed polls, including poller saturation | | Sum |
| `otelcol_networkdevice_pdus_sent` | SNMP request packets sent | | Sum |
