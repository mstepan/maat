> Archived completion record for the initial development implementation. Checked
> tasks and their commands describe that session, not fresh validation of later
> commits or exhaustive fulfillment of every safety scenario. The former
> `make vet` target is now `make lint`; follow
> [AGENTS.md](../../../../AGENTS.md), including `make integration` for significant
> codebase changes, and [the validation record](../../../../docs/validation.md).

## 1. Runtime and configuration

- [x] 1.1 Verify the required Go toolchain, Docker Engine/Compose, and PostgreSQL 18 image/tool availability; record compatible dependency and image pins without lowering the Go requirement.
- [x] 1.2 Replace the startup-only entry point with strict configuration loading and command dispatch; test unknown fields, duplicate identities, invalid paths, thresholds, and three-member topology.
- [x] 1.3 Add cancellation, bounded operation deadlines, structured redacted logging, and protected secret-file handling used by the actual runtime.

## 2. Durable control-plane state

- [x] 2.1 Define and test the deterministic FSM for identity, membership, generation, primary, transition phases, and recovery requests; reject stale/conflicting commands and make retries idempotent.
- [x] 2.2 Integrate HashiCorp Raft, durable BoltDB stores, snapshots, and orderly shutdown; verify state survives restart and rejects unknown persisted formats.
- [x] 2.3 Implement single-seed fixed-membership bootstrap with fresh-storage validation and committed initial authority; test conflicting topology, repeated bootstrap, and missing Raft state with surviving database data.
- [x] 2.4 Implement current-quorum/state checks and leadership transfer; test leadership changes and majority loss without granting database authority.

## 3. PostgreSQL observation and native replication

- [x] 3.1 Implement bounded observations of role, system identity, timeline/history, WAL/replay positions, receiver, and upstream; test malformed LSNs and unknown/error results.
- [x] 3.2 Implement native initialization and process control as the database user, verifying outcomes and excluding shell interpolation and credential leakage.
- [x] 3.3 Implement base-backup initialization for empty replicas, dedicated credentials, standby configuration, and bounded-retention physical slots; verify real replication and idle-cluster catch-up.
- [x] 3.4 Implement idempotent upstream reconfiguration and slot setup on a promoted primary, with current-generation checks and receiver/replay verification.

## 4. Fencing and guarded node startup

- [x] 4.1 Define the infrastructure-independent fencing target/interface and a Docker HTTP adapter with API version negotiation, request deadlines, immutable identity checks, stop, and separate inspection.
- [x] 4.2 Test fencing failure, verification failure, timeout, 404, wrong labels/identity, paused/running/restarting containers, and unsafe restart policy; prove none can authorize promotion.
- [x] 4.3 Implement the container supervisor and guarded PostgreSQL startup; verify agent-only crashes preserve database role and container stop actually fences all node processes.
- [x] 4.4 Implement restart observation and mismatch handling; test that an old-primary container cannot start PostgreSQL writable from its existing data or stale authorization.

## 5. Failover reconciliation

- [x] 5.1 Implement fresh observation exchange and local sample aging; test restart cache invalidation, peer clock skew, missing samples, unhealthy agents, incompatible history, and the exact 16 MiB boundary.
- [x] 5.2 Implement configurable failure detection and leader-local candidate eligibility, including leadership transfer when the current leader cannot be promoted.
- [x] 5.3 Implement durable validation/fencing phases, revalidation, and compare-and-set generation/primary authorization; test competing attempts and expired evidence after fencing.
- [x] 5.4 Implement authorized promotion, role/timeline verification, and replica reconfiguration; test delayed/unknown promotion outcomes and fencing an authorized candidate before replacement.
- [x] 5.5 Implement restart/resume for every transition phase; cover crashes before/after fencing, authorization, promotion, and completion, plus leadership/quorum changes during each unsafe window.

## 6. Rejoin and explicit rebuild

- [x] 6.1 Implement old-primary isolation, history/prerequisite checks, recovery journaling, native rewind, standby configuration repair, and replication verification.
- [x] 6.2 Implement `reinitialization_required` for unsafe, failed, or interrupted rewind; verify repeated reconciliation cannot automatically replace data or start an uncertain directory.
- [x] 6.3 Implement the local operator reinitialization command with explicit target, expected generation, and acknowledgement; route through committed leader authority and reject stale/no-quorum requests.
- [x] 6.4 Implement validated retained-directory rename and native fresh backup with a durable recovery journal; test unsafe paths, primary targets, insufficient space, changed source generation, and interruption at each filesystem phase.

## 7. Compose deployment and operator visibility

- [x] 7.1 Add the pinned development image and three-node Compose topology with persistent storage, managed container identities, disabled automatic restart, loopback host ports, and uncommitted secret files.
- [x] 7.2 Implement read-only status and local operator access showing actual versus desired state, evidence age, recovery reasons, durable transition history, and unknown asynchronous loss.
- [x] 7.3 Add a repeatable fresh-cluster bootstrap/failover/rejoin demonstration that asserts actual database state and preserves volumes during ordinary cleanup.

## 8. Validation and documentation

- [x] 8.1 Run real Compose checks for primary/replica process and container failures, agent-only restart, old-primary return, simultaneous transitions, majority loss, leader changes, and partition with the old primary still writable before fencing.
- [x] 8.2 Exercise fencing uncertainty, stale/lagging candidates, rewind failure, explicit rebuild, and transition/recovery crash windows; map every specification section 34 scenario, including degraded links, hangs, disk/WAL failures, and physical-machine failures, to evidence or an explicit unvalidated limitation.
- [x] 8.3 Run `make fmt`, `make test`, `make vet`, and `make build`; run race checks for concurrent FSM/reconciler behavior and the explicit integration suite, recording unavailable checks and reasons.
- [x] 8.4 Update README and the current-implementation description in OpenSpec configuration with tested commands, implemented scope, fencing trust assumptions, recovery instructions, credential handling, retained-data cleanup, and unvalidated production requirements.
- [x] 8.5 Verify documentation links and Makefile commands, inspect the final diff for scope and secrets, and validate the OpenSpec change before reporting the implementation complete.

## Validation scope

All 32 implementation and development-validation tasks are complete. Real Compose
checks cover replica process/container failures and overlapping proposals across
a leader change, with one committed transition and authorization. The deterministic
matrix passed 17 exact agent-crash boundaries plus six quorum-loss and six
leadership-change windows. Pre-authorization restarts with missing fresh evidence
correctly block; committed authorization resumes after required verification.
See [the validation record](../../../../docs/validation.md) for commands, evidence,
and remaining production fault models. These checks do not claim exhaustive
physical-host, storage, degraded-network, or adversarial timing validation.
