# IPAM Guides

Task-oriented guides for people *using* the IPAM service — provisioning address
space, claiming prefixes, and automating it. These complement the reference and
design docs one level up:

- [API reference](../api.md) — the `ipam.miloapis.com/v1alpha1` kinds, fields, and errors
- [Integrating with the API directly](../integration-guide.md) — client-go / kubectl
- [Architecture](../architecture) — how the service is built
- [Operational runbooks](../runbooks) — on-call procedures

## Available guides

- [Managing IP address space with the CLI](managing-ip-address-space.md) — install the
  `datumctl ipam` plugin, understand pools/claims/allocations, and run the common
  workflows (create a pool, claim a prefix, inspect utilization, release space).

## Planned guides

Topics we expect to add as the audience grows. Open an issue (or a PR) if you
need one sooner:

- **Designing an address plan** — root vs. child pools, hierarchical delegation,
  `--min-length`/`--max-length` bounds, and choosing an allocation strategy
  (`FirstFit` / `BestFit` / `LeastUtilized`).
- **Automating IPAM in scripts and CI** — machine output (`-o json|yaml|name`),
  `--quiet`, idempotent claims (`--name`), `--dry-run`, and the stable exit-code
  contract.
- **Enabling IPAM for a project** — the service-entitlement request/approval flow
  end to end, for consumers and for provider-side approvers.
- **IPv6 addressing** — what changes for IPv6 pools and claims (single address
  family per resource, utilization vs. capacity reporting on very large spaces).
- **Sharing pools across projects** — pool `visibility` (`platform` / `consumer` /
  `shared`) and cross-project selection.
- **Troubleshooting** — reading IPAM errors and exit codes, and resolving the
  common ones (not entitled, pool exhausted, no matching pool).
