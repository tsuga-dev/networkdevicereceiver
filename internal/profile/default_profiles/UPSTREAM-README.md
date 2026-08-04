# Vendored Datadog SNMP profiles

The 240 `*.yaml` files in this directory are **copied unmodified** from
[DataDog/integrations-core](https://github.com/DataDog/integrations-core), path
`snmp/datadog_checks/snmp/data/default_profiles/`, at the commit pinned in
`UPSTREAM-SHA`.

Do not hand-edit them. Local changes are lost on the next resync and make the
provenance claim above unverifiable. To adjust what a device reports, add a
profile to `profiles.user_dir` instead — a user profile shadows an embedded one
of the same name.

## License

BSD 3-Clause, Copyright (c) 2016, Datadog, Inc. The full text as published at
the pinned commit is in `LICENSE` in this directory. This differs from the rest
of the repository, which is Apache-2.0; see `NOTICE` at the repository root.

Datadog does not endorse this receiver.

## Resyncing

```sh
SHA=$(cat UPSTREAM-SHA)
BASE=https://api.github.com/repos/DataDog/integrations-core/contents
gh api "$BASE/snmp/datadog_checks/snmp/data/default_profiles?ref=$SHA" --jq '.[].name'
```

Bump `UPSTREAM-SHA`, re-copy the `*.yaml` files, refresh `LICENSE` from the same
commit, then re-run `go run ./cmd/snmpprofilecheck` — a resync can introduce
symbols the naming registry does not cover yet, which land in the fallback tier
rather than failing loudly.

This file replaces the upstream `README.md`, which was a one-line stub.
