# Compose Operations Specification

## Purpose
Define the three-node development deployment, verified Docker fencing, local operator controls, status, and validation requirements.

## Requirements

### Requirement: Managed three-node development deployment
Compose SHALL run three nodes, each containing an agent and PostgreSQL 18 with
durable separate database and Raft storage. It SHALL use guarded PostgreSQL
startup, disabled automatic container restart, and a supervisor that permits an
agent-only crash without automatically stopping an already-running PostgreSQL.
Published development ports SHALL bind to loopback. The deployment SHALL document
its trusted Docker socket and private unauthenticated network assumptions.

#### Scenario: Agent-only failure
- **GIVEN** an authorized primary with running PostgreSQL
- **WHEN** only its agent crashes and the supervisor restarts it
- **THEN** PostgreSQL does not change role solely because of agent loss
- **AND** the restarted agent observes current PostgreSQL and Raft state before acting

#### Scenario: Fenced container is started again
- **GIVEN** the container holds data from an old primary generation
- **WHEN** an operator starts it through Compose
- **THEN** the entry point starts the agent without automatically starting PostgreSQL
- **AND** PostgreSQL remains stopped until current authority and the recovery path permit a safe start

### Requirement: Replaceable verified Docker fencing
The controller SHALL depend on a fencing interface with separate context-bounded
fence and verification methods; Docker SHALL be its initial adapter. Targets
SHALL bind cluster ID, node ID, and immutable container ID. The adapter SHALL
validate identity and restart policy, request stop through the Docker Engine API,
and separately inspect the exact target. Only verified non-running and
non-restarting state with automatic restart disabled SHALL establish Docker
isolation. The supported workflow SHALL prohibit unmanaged database starts.

#### Scenario: Successful Docker fence
- **GIVEN** the recorded target container matches expected cluster/node identity and restart policy
- **WHEN** stop completes and separate inspection proves the target stopped
- **THEN** the adapter reports verified fencing for that exact incarnation
- **AND** a later restart invalidates that stopped-container evidence

#### Scenario: Ambiguous or failed fencing
- **GIVEN** inspection times out, returns 404, reports running/restarting, finds a replacement identity, or cannot contact Docker
- **WHEN** fencing verification executes
- **THEN** it reports uncertainty or failure, never successful isolation
- **AND** the controller does not authorize a replacement

#### Scenario: Future backend substitution
- **GIVEN** a test fencing implementation satisfies the same interface
- **WHEN** the controller executes its failover state machine
- **THEN** the state machine uses fence and verification operations without depending on Docker-specific response types

### Requirement: Validated configuration and local operator control
The executable SHALL accept strict JSON configuration, reject unknown/invalid
fields, require unique three-member identities and valid addresses/managed paths,
and validate lag/timing thresholds. It SHALL offer bootstrap, read-only status,
and explicit reinitialization workflows. Administrative commands SHALL use local
access and be validated against current Raft authority; no unauthenticated
administrative HTTP endpoint SHALL be published.

#### Scenario: Configuration cannot identify safe targets
- **GIVEN** duplicate node IDs, invalid paths, conflicting peers, or invalid thresholds
- **WHEN** the executable loads configuration
- **THEN** it exits with an actionable error before starting PostgreSQL or fencing any node

#### Scenario: Operator reinitialization routes to authority
- **GIVEN** an operator addresses a follower using the local command
- **WHEN** reinitialization is requested
- **THEN** only a request committed by the current leader can cause target recovery
- **AND** absence of quorum cannot be bypassed with local CLI access

### Requirement: Structured status distinguishes actual and desired state
Status SHALL expose node ID, Raft state/term, quorum observation, generation,
authorized primary, observed PostgreSQL role/timeline/system identity, WAL/replay
positions, lag and observation age, upstream, reconciliation/recovery state,
health timing, and latest failover/fencing outcomes. Unknown and stale values
SHALL be explicit. Significant transition history SHALL survive restart.

#### Scenario: Control and database state disagree
- **GIVEN** Raft authorizes B but B remains in recovery
- **WHEN** status is requested
- **THEN** it separately reports desired primary B, observed replica role, and pending/blocked promotion
- **AND** it does not report completed failover

### Requirement: Repeatable validation and honest readiness claims
The change SHALL provide a fresh-cluster demonstration and focused regression
tests for applicable technical-specification section 34 scenarios. It SHALL run
the repository's formatting, tests, vet, and build checks for Go changes and
record any unavailable checks. README SHALL distinguish implemented behavior,
validated scenarios, and deferred production requirements. Destructive volume
deletion SHALL NOT be part of default demonstration cleanup.

#### Scenario: Development failover and rejoin demonstration
- **GIVEN** fresh Compose volumes and available Docker/PostgreSQL tooling
- **WHEN** the demonstration initializes replication and injects primary failure
- **THEN** it verifies fencing before one promotion, replica reconfiguration, and safe old-primary rejoin
- **AND** it checks actual database roles and replication, not merely API responses

#### Scenario: Failure coverage is incomplete
- **GIVEN** container tests pass but physical-machine, storage, or degraded-link scenarios remain unvalidated
- **WHEN** validation results are documented
- **THEN** those gaps are explicit and automatic failover is not called production-ready
