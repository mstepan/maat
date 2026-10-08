## Why

The repository currently treats the PostgreSQL HA agent as the entire project.
A monorepo with explicit artifact directories will keep that implementation
self-contained and reserve clear homes for the future monitoring TUI,
Kubernetes controller, and Helm deployment.

## What Changes

- Move the current Go implementation, module files, tests, Makefile, Dockerfile, `.dockerignore`,
  Compose file, and deployment/fault runners into `agent/`.
- Preserve root Makefile entry points by forwarding them to the agent Makefile.
- Add README-only `dashboard/`, `k8s-controller/`, and `helm-deployment/`
  directories. Use the corrected spelling `helm-deployment`.
- Use independent Go modules per implemented Go artifact. Only the agent has a
  module now; retain its existing `maat` module path and Go requirement.
- Keep shared governance, technical specification, validation record, license,
  and OpenSpec at the repository root. Provide a root artifact overview and an
  agent README with operating instructions.
- Update path-sensitive documentation, ignore rules, and OpenSpec context.
- **BREAKING**: direct Go, Python, Docker Compose, and binary paths move under
  `agent/`. Root `make` target names remain available. Agent outputs and generated
  assets move to `agent/bin/`; lab credentials move to `agent/.secrets/`.
- **BREAKING**: the reorganized deployment supports fresh labs. Migration of
  existing Compose containers, identities, and volumes is not required.

## Capabilities

### New Capabilities

- `artifact-monorepo`: artifact ownership, independent module boundaries,
  root/agent developer workflows, future-artifact placeholders, and fresh-lab
  compatibility expectations.

### Modified Capabilities

None. Existing HA control-plane, PostgreSQL lifecycle, and Compose operations
requirements retain their runtime behavior; only implementation locations and
documented source/build paths change.

## Impact

This serves maintainers developing and testing artifacts independently. It
affects root Go files, `internal/`, `deploy/`, build/container assets, README,
`AGENTS.md`, `.gitignore`, `docs/`, `openspec/config.yaml`, and CI module/cache
paths. CLI command names,
the `maat` executable name, strict JSON configuration, status API, dependencies,
container-internal paths, and HA algorithms remain unchanged.

Non-goals are implementing the TUI, controller, CRD manifests, Helm chart,
shared libraries, release automation, a Go workspace, or existing-lab migration.
The only future CRD commitment recorded here is its kind, `PostgresqlCluster`;
its API group, versions, schema, permissions, and agent integration belong to a
later proposal.

All eight HA safety invariants remain mandatory. No new authorization, fencing,
promotion, replication, or destructive recovery policy is introduced. A fresh
lab must use fresh database and control storage together, never replacement
containers against surviving identity-bound volumes. Default integration cleanup
continues to retain storage. No lab deletion is needed to prepare this proposal.
