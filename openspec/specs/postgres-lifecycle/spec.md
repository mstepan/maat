# PostgreSQL Lifecycle Specification

## Purpose
Define native PostgreSQL initialization, replication, safe control operations, old-primary rejoin, operator-authorized data replacement, and credential handling.

## Requirements

### Requirement: Native initialization and observed replication health
PostgreSQL SHALL own physical asynchronous replication. The agent SHALL use
PostgreSQL-native tools for initialization and configure dedicated replication
credentials, `pg_hba.conf`, standby settings, upstream connection, and per-replica
physical slots with finite retention. It SHALL verify database identity, recovery
role, WAL receiver/upstream, and replay catch-up before declaring a replica healthy.
Go MUST NOT implement WAL streaming or copy database files itself.

#### Scenario: Fresh replica initialization
- **GIVEN** an authorized healthy primary and an empty managed replica directory
- **WHEN** initialization runs
- **THEN** a native base backup and standby configuration produce a replica following that primary
- **AND** readiness remains false until receiver and replay checks pass

#### Scenario: Idle primary
- **GIVEN** a replica receives WAL from the correct primary and has replayed its observed watermark
- **WHEN** the primary generates no new WAL
- **THEN** lack of LSN movement alone does not make the caught-up replica unhealthy

### Requirement: Safe idempotent PostgreSQL control
The agent SHALL bound and validate control operations, serialize local changes,
recheck generation around long-running work, and observe the result of every
operation. PostgreSQL and its native tools SHALL run as the database user.
Subprocess arguments MUST NOT be constructed through shell interpolation.
Unknown command results SHALL remain pending or blocked until observed.

#### Scenario: Promotion returns before role verification succeeds
- **GIVEN** committed current authorization permits local promotion
- **WHEN** the promotion call succeeds but PostgreSQL still reports recovery or cannot be observed
- **THEN** the agent does not mark the node primary or the transition complete

#### Scenario: Upstream reconciliation repeats
- **GIVEN** a replica already follows the authorized verified primary
- **WHEN** reconciliation runs repeatedly
- **THEN** it verifies health without repeatedly restarting or rewriting an unchanged configuration

#### Scenario: Source authority changes during a backup
- **GIVEN** a native backup started against generation N
- **WHEN** the authorized primary changes before the new data is admitted
- **THEN** the agent revalidates source/history and does not start from stale assumptions

### Requirement: Old-primary isolation and rewind-based rejoin
A returning old primary SHALL remain unable to accept application writes until
rejoined as a verified standby. The agent SHALL inspect system identity, timeline
history, stopped state, WAL availability, checksums or `wal_log_hints`, and
`full_page_writes` before rewind. It SHALL journal recovery progress, run native
`pg_rewind`, restore standby configuration, and verify replication. A compatible
replica changing upstream SHALL NOT be rewound merely because the primary has a
new timeline.

#### Scenario: Safe old-primary return
- **GIVEN** A previously accepted writes and B is now authorized primary
- **WHEN** A's container returns with intact data and valid rewind prerequisites
- **THEN** A keeps PostgreSQL isolated, rewinds, configures B as upstream, and starts only in recovery
- **AND** A becomes healthy only after receiver and replay verification

#### Scenario: Interrupted or unsafe rewind
- **GIVEN** prerequisites are absent, rewind fails, or an interrupted rewind cannot be proven complete
- **WHEN** reconciliation runs or the agent restarts
- **THEN** PostgreSQL stays stopped and status reports `reinitialization_required`
- **AND** the agent neither starts the uncertain directory nor automatically takes a replacement backup

### Requirement: Explicit operator-authorized data replacement
Fresh-base-backup recovery of existing data SHALL require an explicit operator
request naming node and expected generation with data-replacement acknowledgement.
The request SHALL be committed and revalidated. The target MUST be a stopped
non-primary within the configured managed data root with validated ownership,
identity, prerequisites, available space, and current source. Unsafe paths,
symlinks, and unsupported tablespaces SHALL be rejected. The old directory SHALL
be retained separately and never automatically deleted. A recovery journal SHALL
make directory selection, backup, and configuration restart-safe.

#### Scenario: No acknowledgement
- **GIVEN** a node reports `reinitialization_required`
- **WHEN** a request omits acknowledgement or references a stale generation
- **THEN** no data directory is replaced and the command reports why it was rejected

#### Scenario: Authorized rebuild
- **GIVEN** a valid committed request for an isolated replica
- **WHEN** the operator-authorized recovery runs
- **THEN** the old directory is retained, a native fresh backup is verified, and standby configuration is installed
- **AND** the node is admitted only after replication verification

#### Scenario: Unsafe target or interruption
- **GIVEN** a target resolves outside the managed root, names the primary, lacks space, or has a partially completed rebuild
- **WHEN** reinitialization is requested or resumed
- **THEN** unsafe work is rejected and interrupted work follows the recorded phase without starting uncertain data

### Requirement: Credentials and observable data-loss limits
Replication SHALL use dedicated credentials and avoid ordinary replication with
superuser credentials. Secrets SHALL be loaded from protected files and redacted
from logs and status. Status SHALL distinguish observed lag and its evidence age
from the unknown final WAL/transaction loss of a failed primary. The implementation
MUST NOT claim lossless asynchronous failover.

#### Scenario: Database or subprocess error contains sensitive context
- **GIVEN** an operation fails with connection or credential context
- **WHEN** its outcome is logged or returned through status
- **THEN** credentials are redacted and the operational error remains identifiable

#### Scenario: Eligible asynchronous candidate
- **GIVEN** a candidate satisfies the configured 16 MiB observed-lag limit
- **WHEN** it is selected or promoted
- **THEN** status exposes the sampled lag and age and states that actual final transaction loss may be unknown
