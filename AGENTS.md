# Repository guidance

## Scope and source of truth

This file applies to the entire repository.

Read [the technical specification](<docs/Custom PostgreSQL HA Controller — Technical Specification.md>)
before implementing or changing controller behavior. It defines the architecture,
MVP scope, safety invariants, and required failure scenarios. Keep `README.md`
aligned with it and clearly distinguish planned behavior from implemented features.
If a requirement is ambiguous, identify the unresolved policy rather than inventing
a safety guarantee.

The repository implements a three-node PostgreSQL 18 Compose development
controller in module `maat`. `main.go` provides `run`, `status`, and `reinitialize`
commands with strict JSON configuration. `internal/agent`, `internal/cluster`,
`internal/postgres`, and `internal/fencing` implement reconciliation, durable
HashiCorp Raft/BoltDB state, native PostgreSQL lifecycle, and verified Docker
fencing. Unit/race tests, native PostgreSQL lifecycle tests, and Compose fault
runners exist; see [the validation record](docs/validation.md) for coverage and
remaining gaps. This is not production-ready HA. Go 1.27.1 or newer is required
by `go.mod`.

## Architecture and implementation scope

- Run one Go agent beside each PostgreSQL instance. The initial topology has
  three nodes; their agents form the control plane without etcd or a separate
  controller service.
- Use an existing Raft implementation. The MVP specifies HashiCorp Raft; do not
  implement consensus from scratch. Persist authoritative Raft state durably.
- Raft owns membership, HA generation, primary authorization, and desired
  topology. PostgreSQL remains authoritative for observed local database state.
- PostgreSQL owns WAL and physical streaming replication. Configure and monitor
  native replication; never implement WAL streaming or data copying in Go.
- Keep Raft leadership separate from PostgreSQL primary status, and HA generation
  separate from PostgreSQL timeline.
- Separate responsibilities for PostgreSQL control, replication, health,
  reconciliation, fencing, and membership as implementation requires. The spec's
  component and interface examples are guidance, not a mandate to scaffold unused
  packages or abstractions.
- Keep the MVP to asynchronous replication, health monitoring, membership,
  election, fencing, promotion, replica reconfiguration, rewind-based rejoin,
  and reconciliation. Expose potential transaction loss. Synchronous replication
  and more elaborate candidate policies are future work.
- Prefer existing code and the standard library. Add dependencies only for a
  concrete need; preserve the required fencing abstraction.

## Mandatory safety invariants

1. Authorize at most one PostgreSQL primary for the current HA generation.
2. Do not authorize a replacement while the old primary may still accept writes.
   Fence it first and verify the fencing condition; neither a failed connection
   nor fencing API success alone proves isolation.
3. Never allow a stale node to self-promote. Validate membership, current Raft
   state, PostgreSQL health and recovery state, timeline, WAL/replay position,
   lag, fencing status, and maintenance/drain state when supported.
4. Do not use Raft to replicate PostgreSQL data.
5. Observe and verify PostgreSQL state after every control operation.
6. Make reconciliation idempotent so retries converge safely to desired state.
7. Track PostgreSQL timeline and HA generation independently.
8. Represent transitions as explicit, recoverable states so promotion survives
   an agent crash at any step.

The failover order is detection and Raft leadership, candidate validation,
fencing and verification, committed authorization with generation increment,
promotion and verification, then replica reconfiguration. Failure detection or
election alone must never promote PostgreSQL. Without Raft majority, do not invent
cluster state or promote unilaterally.

Handle mismatches between Raft authorization and observed PostgreSQL role on
restart, including a crash after fencing, after authorization, or after promotion.
Distinguish agent failure from PostgreSQL failure: losing an agent does not
automatically change the database role, and an unhealthy agent's node is not a
safe promotion candidate.

## Replication, rejoin, and credentials

- Initialize replicas using PostgreSQL-native tools such as `pg_basebackup`;
  configure replication credentials, `pg_hba.conf`, `primary_conninfo`, and slots
  as required. Verify WAL receiver and replay before declaring a replica healthy.
- A returning old primary must first be prevented from accepting application
  writes. Inspect timelines and `pg_rewind` prerequisites; rewind safely or require
  an explicit operator-approved rebuild before replacing existing data with a
  fresh base backup. Configure the current primary as upstream and verify rejoin.
- Treat replacement of a data directory as destructive recovery. Validate the
  target and prerequisites; do not discard data merely because a health check
  failed.
- Use dedicated replication credentials and avoid superuser credentials for
  ordinary replication where possible. Protect fencing credentials and keep
  secrets out of logs, source files, and documentation examples.
- Transport TLS, node authentication, and administrative authorization are
  specified future requirements; do not imply they already exist.

## Validation and documentation

Use the existing commands from the repository root:

```sh
make fmt          # format Go code
make test         # run Go tests, race tests with fault hooks, and Python unit tests
make lint         # run golangci-lint
make build        # build bin/maat
make integration  # run Compose primary-failure/rejoin smoke test in a fresh project
make run          # show CLI usage; exits nonzero without command/config arguments
make clean        # remove the entire bin/ directory, including generated test assets
```

For Go changes, run formatting and relevant tests, lint, and build before claiming
completion. Add focused regression checks for changed safety or transition logic.
For any significant codebase change, also execute `make integration` before
claiming completion. This includes changes to controller behavior, Raft state,
fencing, PostgreSQL lifecycle/recovery, configuration, dependencies, deployment,
or the integration harness. It requires Docker Engine/Compose and runs the smoke
scenario in an isolated project; it does not run the full failure matrix. Run
additional affected scenarios from [the validation record](docs/validation.md).
Its default cleanup stops its containers and retains volumes and generated
assets for inspection. Do not substitute `make test` for this integration check.
Report checks that could not run and their reasons; do not present an empty test
suite as validation of HA behavior. For documentation-only changes, verify local
links, commands against the Makefile, and consistency with the spec and code.

Bare `make` runs `all`, which includes `clean` and `integration`; use explicit
targets when you do not intend to remove `bin/` or launch a fault-injection lab.

Before calling automatic failover production-ready, validate all scenarios in
specification section 34: process and machine failures, agent restarts, network
partitions and degraded links, PostgreSQL hangs and storage/WAL failures,
simultaneous promotions, old-primary return, leader changes and majority loss,
fencing/verification failures, rewind/base-backup recovery, and crashes around
each transition step.

As observability is implemented, expose agent identity, Raft state/term, HA
generation, authorized primary, PostgreSQL role/timeline, WAL position and lag,
upstream, reconciliation state, and health/failover/fencing history. Never claim
lossless asynchronous failover or unverified production readiness.
