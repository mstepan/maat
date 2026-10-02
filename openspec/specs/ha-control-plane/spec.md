# HA Control Plane Specification

## Purpose
Define durable Raft authority, bootstrap, candidate eligibility, serialized failover, and recovery of PostgreSQL primary authorization.

## Requirements

### Requirement: Durable control-plane authority
The system SHALL use HashiCorp Raft with durable logs, stable state, and snapshots
for cluster identity, fixed three-voter membership, HA generation, authorized
primary, desired topology, and recoverable transitions. It MUST NOT replicate
PostgreSQL data through Raft or equate Raft leadership with PostgreSQL role.

#### Scenario: Restart preserves authority
- **GIVEN** generation 7 authorizes node B and a transition is in progress
- **WHEN** agents restart with their existing storage
- **THEN** they recover generation 7, node B, membership, and transition state
- **AND** they do not bootstrap a new cluster or authorize a different primary

#### Scenario: Lost Raft state with surviving database
- **GIVEN** a node has an existing database but no valid Raft state
- **WHEN** the agent starts with bootstrap configuration
- **THEN** it refuses fresh bootstrap and reports operator recovery required

### Requirement: Explicit fresh-cluster bootstrap
Only the configured bootstrap seed SHALL initialize the three-member Raft set.
Initial primary authorization SHALL require verified fresh managed database
directories and a committed generation 1; other nodes MUST NOT bootstrap after
an election timeout. Conflicting identities and nonempty unknown data SHALL fail
closed.

#### Scenario: Fresh cluster starts once
- **GIVEN** three configured nodes with verified empty managed storage
- **WHEN** the seed bootstraps and quorum commits the initial primary
- **THEN** only the designated primary can initialize and start writable
- **AND** the remaining nodes initialize replicas from that primary

### Requirement: Fresh candidate eligibility
New authorization SHALL require current quorum and membership; healthy candidate
agent and PostgreSQL; recovery mode; valid system identity and compatible timeline
history; valid WAL/replay positions; no fence or recovery exclusion; and fresh
observations. Default maximum observed lag SHALL be 16,777,216 bytes and SHALL be
configurable, including zero. Missing, malformed, or stale evidence MUST NOT pass.
Observations SHALL expire using locally measured age rather than peer clock trust.
The default maximum age SHALL be 30 seconds and SHALL be configurable.

#### Scenario: Lag boundary
- **GIVEN** fresh compatible primary and candidate observations
- **WHEN** candidate replay trails the sampled primary flush position by exactly 16,777,216 bytes
- **THEN** the default lag check passes, subject to all other eligibility checks
- **AND** a lag of 16,777,217 bytes fails the check

#### Scenario: Evidence cannot establish eligibility
- **GIVEN** a candidate has a stale sample, unknown primary position, incompatible history, invalid LSN, or unhealthy agent
- **WHEN** the leader evaluates promotion
- **THEN** it records the specific blocking reason and neither authorizes nor promotes the candidate

#### Scenario: Clock skew and restart cannot refresh evidence
- **GIVEN** a peer reports a future timestamp or the receiving agent restarts
- **WHEN** eligibility is evaluated
- **THEN** peer timestamps and persisted health records cannot substitute for fresh observations

### Requirement: Leader-local failover with explicit quorum checks
Only an eligible current Raft leader SHALL initiate local promotion. An ineligible
leader SHALL attempt Raft leadership transfer to an eligible member, choosing
deterministically by node ID. The new leader SHALL independently validate the
transition. Primary failure detection SHALL default to three consecutive failed
observations at a one-second interval, with validated configurable values.
Election or agent loss alone MUST NOT change a database role.

#### Scenario: Primary PostgreSQL fails while its agent remains leader
- **GIVEN** node A leads Raft but its PostgreSQL has failed
- **WHEN** the detection threshold is reached and B is eligible
- **THEN** A attempts leadership transfer and B validates before fencing or promoting
- **AND** A does not promote its unhealthy local database

#### Scenario: Majority is unavailable
- **GIVEN** a node has no Raft majority, regardless of cached authorization
- **WHEN** it detects failure or resumes an interrupted transition
- **THEN** it neither creates authority nor promotes a replica unilaterally

### Requirement: Serialized fence-before-authorization transitions
The system SHALL persist explicit validating, fencing, authorized, promoting,
reconfiguring, and complete phases. Commands SHALL compare expected generation,
transition identity, and phase. Old-primary fencing MUST be independently verified
before committing one replacement authorization and one generation increment.
Candidate eligibility SHALL be revalidated before this commit. Promotion SHALL
follow committed authorization and current quorum checks.

#### Scenario: Two competing attempts
- **GIVEN** two requests reference the same current generation
- **WHEN** they attempt conflicting transitions
- **THEN** at most one transition advances and only one replacement is authorized
- **AND** exact retries do not increment the generation twice

#### Scenario: Fence success without verified isolation
- **GIVEN** a fencing request reports success but inspection fails or the target remains running
- **WHEN** the transition attempts authorization
- **THEN** generation and primary authorization remain unchanged and no promotion occurs

#### Scenario: Leadership changes while promotion may be in flight
- **GIVEN** B is authorized but its promotion result is unknown
- **WHEN** a new leader wants to authorize C
- **THEN** it treats B as a possible writer and fences B's recorded container before changing authority
- **AND** it does not treat the last observed recovery role as proof of isolation

### Requirement: Recoverable authorization and observed-role reconciliation
The system SHALL recover transitions from committed state plus fresh observations.
It SHALL independently track HA generation and PostgreSQL timeline and verify
every PostgreSQL control operation. Stale transition work MUST NOT resume under
a different authorization. A previously committed authorization MAY resume only
after current quorum, membership, fencing, local health, and its recorded replay
watermark/history have been verified; it SHALL NOT require new observations from
the already fenced old primary.

#### Scenario: Crash around fencing and authorization
- **GIVEN** an agent crashes before fencing, after fencing, or after authorization
- **WHEN** reconciliation resumes
- **THEN** it repeats required verification, retains sole authority, and advances only from the committed phase
- **AND** a new authorization is blocked if its eligibility evidence has expired

#### Scenario: Crash after promotion before completion
- **GIVEN** B was authorized and PostgreSQL promoted before the agent crashed
- **WHEN** the agent returns and current authorization still names B
- **THEN** it verifies the primary role and timeline and records completion without a second promotion

#### Scenario: Unauthorized writable database
- **GIVEN** observed local PostgreSQL is writable but current authorization names another node
- **WHEN** reconciliation establishes the mismatch
- **THEN** it stops and verifies isolation of the local database before recovery
- **AND** it cannot claim primary status from its old generation
