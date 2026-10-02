## Why

Maat currently prints a startup message. Developers need a runnable three-node
Docker Compose cluster that demonstrates the technical specification's complete
failover and rejoin flow, with explicit safety checks and repeatable failure tests.

## What Changes

- Run one Go agent alongside PostgreSQL on each of three Compose nodes, with
  durable HashiCorp Raft state and native asynchronous streaming replication.
- Implement observed health, guarded primary authorization, recoverable failover
  transitions, promotion verification, and replica reconciliation.
- Define a fencing interface with separate fence and verification operations.
  Implement it using the Docker Engine API; Kubernetes and VM backends remain
  future work.
- Default the configurable maximum observed promotion lag to 16 MiB
  (16,777,216 bytes). Reject stale or missing observations. Report that the failed
  primary's final WAL position and actual transaction loss may be unknown.
- Rejoin an old primary with `pg_rewind` when safe. If prerequisites are absent or
  rewind fails, keep PostgreSQL isolated, report `reinitialization_required`, and
  require an explicit operator action before a fresh base backup.
- Provide validated configuration, structured status, local operator commands,
  a fresh-cluster bootstrap workflow, and an integration demonstration.

### Scope and non-goals

This is a development implementation of the section 33 MVP, not a production
readiness claim. It covers the full bootstrap → failover → replica reconfiguration
→ old-primary rejoin lifecycle. Initial membership is a fixed three-voter set;
online expansion/removal, arbitrary existing-cluster adoption, automatic data
replacement, client connection routing, synchronous replication, TLS, node
authentication, administrative authorization, metrics export, and additional
fencing backends are outside this change.

All eight safety invariants apply. The implementation must preserve the order
candidate validation → fencing and verification → committed authorization with
generation increment → promotion and verification → replica reconfiguration.
Election, health-check failure, stale authorization, or Docker API success alone
must not authorize promotion.

### Confirmed decisions and proposed defaults

Confirmed in the requirements interview: Docker Compose is the primary target;
Docker Engine fencing sits behind a replaceable interface; rebuilding a data
directory requires an explicit operator action; the initial observed-lag limit
is configurable and defaults to 16 MiB.

Proposed implementation defaults are PostgreSQL 18, three fixed members, one
combined agent/PostgreSQL container per node, JSON configuration, a one-second
observation interval, three consecutive failed observations before attempting
failover, and a configurable 30-second maximum observation age. The design
explains these choices and their limits; they are not additional interview
decisions already approved by the user.

## Capabilities

### New Capabilities

- `ha-control-plane`: Durable membership and HA state, candidate validation,
  serialized failover authorization, fencing contracts, and crash recovery.
- `postgres-lifecycle`: PostgreSQL observation, native initialization and
  replication, guarded promotion, upstream reconciliation, rewind, and
  operator-authorized reinitialization.
- `compose-operations`: Three-node deployment, Docker fencing, startup isolation,
  configuration, status, operator commands, and failure demonstrations.

### Modified Capabilities

None. The repository contains no implemented OpenSpec capabilities.

## Impact

The change will replace the startup-only entry point and introduce only the Go
packages needed by these behaviors, focused tests, container/Compose assets, and
operator documentation. `README.md` and the current-implementation description
in `openspec/config.yaml` must reflect what actually passes validation.

Expected dependencies are HashiCorp Raft, its durable BoltDB backend, and a
PostgreSQL driver. Use the Go standard library for configuration, HTTP, logging,
subprocesses, and the small Docker stop/inspect client. Pin compatible dependency
versions during implementation and retain the repository's Go requirement.

Docker socket access is privileged and is explicitly confined to this trusted
development deployment. Secrets come from mounted files, not committed samples
or logs. An unavailable Docker daemon or unverifiable container identity blocks
failover. A single-host Compose cluster cannot validate physical machine or
multi-host infrastructure failures.

## Acceptance and open questions

Success means the three-node demonstration can initialize replication, fence a
failed or partitioned primary, promote one eligible replica, reconfigure the
other replica, and rejoin the old primary safely. Negative tests must show that
missing quorum, stale evidence, unacceptable lag, fencing uncertainty, and stale
generations prevent promotion. Recovery tests cover every transition crash window.

No further product-policy answer is needed to draft this proposal. The proposed
defaults and supported startup/recovery workflow require design review before
implementation. Exact dependency/image pins are resolved and recorded during
implementation. Production fencing, security, and multi-host failure validation
remain explicitly outside the guarantees of this change.
