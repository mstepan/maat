# Custom PostgreSQL HA Controller

This document defines the architecture and safety requirements. The repository
now implements a fixed three-node PostgreSQL 18 Compose development controller
using HashiCorp Raft, durable BoltDB state, native PostgreSQL tools, and Docker
fencing. It is not production-ready. See [README](../README.md) for current
commands and scope, [OpenSpec capabilities](../openspec/specs) for concrete
requirements and implementation notes, and [the validation record](validation.md)
for tested scenarios and remaining gaps. Component/type examples below are
conceptual, not declarations of the current Go API; dynamic membership,
maintenance controls, production security, and metrics remain future work.

## 1. Objective

Build a custom PostgreSQL HA controller similar in concept to Patroni, implemented in Go, without etcd.

The system will run one Go agent alongside each PostgreSQL instance.

The agents collectively form the HA control plane using Raft.

PostgreSQL remains responsible for the data plane and physical replication.

Target initial topology:

```text
Node A
  ├── Go HA Agent
  └── PostgreSQL

Node B
  ├── Go HA Agent
  └── PostgreSQL

Node C
  ├── Go HA Agent
  └── PostgreSQL
```

The initial deployment assumes three nodes so that Raft can maintain a majority when one node fails.

---

# 2. Fundamental Architecture

The system consists of two independent distributed mechanisms.

## Control plane

Implemented by:

```text
Go Agent + Raft
```

Responsible for:

- leader election
- cluster membership
- cluster generation
- primary authorization
- failover coordination
- fencing decisions
- desired PostgreSQL topology
- reconciliation

## Data plane

Implemented by:

```text
PostgreSQL streaming replication
```

Responsible for:

- WAL generation
- WAL streaming
- WAL replay
- physical replication
- PostgreSQL timelines
- replication slots

The Go agent must NOT implement WAL replication.

Architecture:

```text
              Go Agents
           ┌───────────────┐
           │     Raft      │
           │               │
           │ cluster state │
           │ leader election
           └───────┬───────┘
                   │
              orchestration
                   │
        ┌──────────┼──────────┐
        ▼          ▼          ▼
   PostgreSQL PostgreSQL PostgreSQL
        A          B          C
        │          │          │
        └──── WAL streaming ──┘
```

---

# 3. No Separate Control-Plane Service

"Control plane" is a logical concept, not a separate deployment.

There is no requirement for:

```text
HA controller service
+
etcd
+
PostgreSQL
```

Instead:

```text
Node A = Agent + PostgreSQL
Node B = Agent + PostgreSQL
Node C = Agent + PostgreSQL
```

The three agents collectively form the control plane through Raft.

---

# 4. Raft

Use an existing Go Raft implementation.

The MVP uses HashiCorp Raft with durable BoltDB log/stable storage and file snapshots.

Do not implement Raft from scratch.

Raft provides:

- leader election
- majority/quorum
- ordered state changes
- replicated control-plane state
- protection against multiple independent control-plane leaders

Raft does NOT replicate PostgreSQL data.

---

# 5. Raft Leader vs PostgreSQL Primary

These are conceptually different.

For example, temporarily:

```text
Raft leader:
    B

PostgreSQL primary:
    A
```

is valid even outside a failover transition.

During a successful leader-local failover, they can converge to:

```text
Raft leader:
    B

PostgreSQL primary:
    B
```

The Raft leader is responsible for coordinating the transition, not automatically making its local PostgreSQL instance primary.
There is no requirement to move a healthy PostgreSQL primary merely to match a
new Raft leader.

---

# 6. Cluster State

Raft should contain a small replicated cluster state.

Example:

```go
type ClusterState struct {
    Generation uint64
    Primary    string
    Nodes      map[string]NodeState
}

type NodeState struct {
    ID          string
    RaftAddress string
}
```

Additional observed information can be stored if useful, but PostgreSQL itself should remain authoritative for local database state.

The important control-plane state is:

```text
generation
primary
membership
desired topology
```

---

# 7. Generation

Every primary transition increments a cluster generation.

Example:

```text
Generation 41:
    primary = A
```

After failover:

```text
Generation 42:
    primary = B
```

A node returning from failure sees:

```text
I was primary in generation 41.
Current generation = 42.
Current primary = B.
```

Therefore it must not become primary again.

Generation checks help reject stale work when combined with current quorum
authority, guarded PostgreSQL startup, and verified fencing. A generation number
alone cannot stop an old primary from accepting writes.

---

# 8. PostgreSQL Timeline

PostgreSQL has its own timeline mechanism.

Example:

```text
A:
    timeline 1
```

B is promoted:

```text
B:
    timeline 2
```

The controller must track both:

```text
HA generation
PostgreSQL timeline
```

They solve different problems.

Generation identifies the current HA control-plane epoch.

Timeline identifies PostgreSQL's database history.

Do not confuse the two.

---

# 9. PostgreSQL Replication

Use PostgreSQL native physical streaming replication.

Example:

```text
A = PRIMARY
B = REPLICA
C = REPLICA
```

PostgreSQL performs:

```text
A
│
├── WAL ──► B
│
└── WAL ──► C
```

The agent configures and monitors this.

Typical PostgreSQL settings include:

```conf
wal_level = replica
max_wal_senders = ...
max_replication_slots = ...
```

The agent manages appropriate replication authentication through `pg_hba.conf` and replication credentials.

Replica configuration uses PostgreSQL's native replication configuration, including `primary_conninfo`.

---

# 10. Replica Initialization

When creating a new replica:

```text
1. Determine current primary.
2. Verify primary is healthy.
3. Create/configure replication credentials.
4. Run pg_basebackup or equivalent PostgreSQL-native initialization.
5. Configure replica connection to primary.
6. Start PostgreSQL.
7. Verify WAL receiver.
8. Verify replay.
9. Mark replica healthy.
```

The Go controller does not copy PostgreSQL data itself.

---

# 11. PostgreSQL Observation

The agent needs a PostgreSQL observer/controller.

Important checks include:

```sql
SELECT pg_is_in_recovery();
```

On the primary:

```sql
SELECT
    application_name,
    client_addr,
    state,
    sync_state,
    sent_lsn,
    write_lsn,
    flush_lsn,
    replay_lsn
FROM pg_stat_replication;
```

On replicas, inspect WAL receiver and replay state.

The controller should collect:

```text
PostgreSQL availability
role
timeline
WAL position
replication connection
replication lag
replay position
upstream
```

---

# 12. Controller Components

The Go agent should be internally separated into components.

Suggested architecture:

```text
Agent
│
├── Raft Controller
│
├── Cluster State
│
├── PostgreSQL Controller
│
├── Replication Controller
│
├── Health Controller
│
├── Reconciler
│
├── Fencing Controller
│
└── Membership Controller
```

Possible Go structure:

```go
type Agent struct {
    raft        *raft.Raft
    postgres    *PostgresController
    replication *ReplicationController
    fencing     FencingController
    reconciler  *Reconciler
}
```

---

# 13. PostgreSQL Controller

The PostgreSQL controller encapsulates local PostgreSQL operations.

Conceptually:

```go
type PostgresController interface {
    Health() error

    IsInRecovery() (bool, error)
    IsPrimary() (bool, error)

    Promote() error

    Start() error
    Stop() error
    Restart() error

    Timeline() (uint64, error)

    ReplicationState() (ReplicationState, error)

    ConfigurePrimary(primary Node) error

    InitializeReplica(primary Node) error

    Rewind(primary Node) error
}
```

The exact interface should be adapted to the supported PostgreSQL versions.

---

# 14. Replication Controller

Responsible for:

- creating replication configuration
- creating replication slots where required
- initializing replicas
- changing upstream
- monitoring replication
- deciding when a replica needs reinitialization
- coordinating `pg_rewind`
- performing a new base backup after explicit operator approval when rewind is impossible

It should not implement WAL streaming.

---

# 15. Fencing

Fencing is required to prevent split-brain.

Example:

```text
A = PRIMARY

Network partition

A  XXXXXXXXX  B
             C
```

B may conclude A has failed.

But A may still be alive and accepting writes.

If B promotes without fencing:

```text
A = PRIMARY
B = PRIMARY
```

Therefore:

```text
fence A
   ↓
verify A cannot write
   ↓
commit B's authorization and increment HA generation
   ↓
promote B
```

Possible fencing implementations:

- IPMI/BMC
- cloud infrastructure API
- VMware/hypervisor API
- Kubernetes-specific mechanism
- watchdog/STONITH
- infrastructure-specific power isolation

Fencing must be abstracted:

```go
type Fencer interface {
    Fence(nodeID string) error
    IsFenced(nodeID string) (bool, error)
}
```

Do not consider "TCP connection failed" to be fencing.

---

# 16. Failover State Machine

A PostgreSQL replica should not transition directly:

```text
REPLICA -> PRIMARY
```

Use explicit states.

The implemented durable transition phases are:

```text
validating       # current leader/quorum and candidate checks
    ↓
fencing          # stop old writer, independently verify isolation, revalidate candidate
    ↓
authorized       # committed replacement and HA generation increment
    ↓
promoting        # promote under current authority, verify role/timeline
    ↓
reconfiguring    # verify remaining unfenced replicas
    ↓
complete
```

Database roles, local recovery-journal phases, and reconciliation errors are
reported separately. A completed transition can leave the fenced old node
stopped; full three-node health requires its subsequent verified rejoin.

---

# 17. Failover Protocol

Initial:

```text
A = PRIMARY
B = REPLICA
C = REPLICA
```

A fails.

### Step 1 — Detect failure

B/C agents detect that A is unavailable.

Important: failure detection alone does not authorize promotion.

### Step 2 — Raft election

B or C participates in Raft election.

Suppose:

```text
Raft leader = B
```

### Step 3 — Candidate validation

B checks:

- PostgreSQL is healthy
- PostgreSQL is in recovery
- candidate has valid PostgreSQL state
- candidate's WAL position is acceptable
- candidate belongs to the current cluster
- candidate is not stale

### Step 4 — Fence old primary

B attempts to fence A.

### Step 5 — Verify fencing

Do not continue solely because the fencing API returned success.

Independently verify the fencing condition. If isolation is uncertain, block
authorization and promotion.

### Step 6 — Authorize new primary

Commit/update Raft state:

```text
generation = N + 1
primary = B
```

### Step 7 — Promote PostgreSQL

On B:

```sql
SELECT pg_promote();
```

Verify:

```sql
SELECT pg_is_in_recovery();
```

returns:

```text
false
```

### Step 8 — Reconfigure remaining replicas

C must stop following A and begin following B.

---

# 18. Reconfiguration After Failover

Before:

```text
A PRIMARY
B REPLICA -> A
C REPLICA -> A
```

After B promotion:

```text
B PRIMARY
C REPLICA -> A    # stale
```

Desired:

```text
B PRIMARY
C REPLICA -> B
```

The C agent sees:

```text
desired:
    upstream = B

actual:
    upstream = A
```

It reconciles:

```text
stop/reconfigure as needed
set B as upstream
start/reload PostgreSQL
verify WAL receiver
verify replay
```

---

# 19. Reconciliation

The controller should use a desired-state reconciliation model.

Raft state:

```text
generation = 42

primary = B

desired:
    B = primary
    A = replica -> B
    C = replica -> B
```

Each agent continuously observes local PostgreSQL state.

Example:

```text
desired:
    B = primary

actual:
    B = replica
```

Action, only within the committed resumable transition after current quorum,
fencing, membership, and local-state checks:

```text
promote B
```

Another example:

```text
desired:
    C = replica -> B

actual:
    C = replica -> A
```

Action:

```text
reconfigure C -> B
```

This should be idempotent.

Running reconciliation repeatedly should converge toward the desired state.

---

# 20. Old Primary Rejoin

Suppose:

```text
A = old primary
B = new primary
C = replica
```

A returns.

A must not become primary.

The agent detects:

```text
old generation
old PostgreSQL timeline
current primary = B
```

A enters:

```text
REJOINING
```

Potential sequence:

```text
1. Ensure A cannot accept application writes.
2. Inspect A and B timelines.
3. Determine whether pg_rewind is possible.
4. Run pg_rewind if applicable.
5. Configure A -> B.
6. Start PostgreSQL as replica.
7. Verify replication.
8. Mark A healthy.
```

If `pg_rewind` cannot be used safely or fails, keep PostgreSQL stopped and report
`reinitialization_required`. Replacing existing data requires a committed operator
request with the target node, current generation, and explicit acknowledgement.
Validate the stopped target, identity, paths, source, and free space; retain the
old directory separately, take a native fresh base backup, and verify replication.
Never discard data automatically because a health check or rewind failed.

Final state:

```text
          B
       PRIMARY
       /     \
      A       C
   REPLICA  REPLICA
```

---

# 21. `pg_rewind`

`pg_rewind` is particularly important when an old primary has diverged from a newly promoted primary.

Example:

```text
Timeline 1:

A:
    100 101 102 103 104
                   ^
                   old primary
```

B was behind:

```text
B:
    100 101 102
```

B gets promoted and creates timeline 2:

```text
B:
    100 101 102
             \
              105 106 107
```

A and B now have divergent histories.

The controller can potentially:

```text
pg_rewind A against B
```

then configure:

```text
A -> B
```

and restart A as a replica.

The controller must verify the prerequisites for `pg_rewind`; a necessary fresh
base backup of existing data follows the explicit rebuild policy in section 20.

---

# 22. Promotion Candidate Selection

Candidate selection should not be based solely on:

```text
"I can reach PostgreSQL"
```

The controller should consider:

- node membership
- Raft state
- PostgreSQL role
- PostgreSQL health
- recovery state
- WAL/replay position
- replication lag
- PostgreSQL timeline
- fencing status
- node maintenance/drain state

A future implementation can rank eligible candidates based on these properties, but the initial implementation should keep policy simple and explicit.
The current policy is leader-local, with leadership transfer to an eligible
replica when needed. It requires equal observed timeline IDs, matching system
identity, and fresh primary/candidate WAL evidence; it does not parse timeline
history to admit candidates on different timelines. Defaults are a 16 MiB
maximum observed lag and 30-second evidence age. Maintenance/drain controls
and ranking by most advanced WAL position are not implemented.

---

# 23. Asynchronous Replication

The MVP can use asynchronous streaming replication.

Advantages:

- simpler
- lower write latency
- easier initial deployment

Tradeoff:

If:

```text
A WAL = 100
B WAL = 95
```

and A fails, promoting B can lose the transactions represented by WAL 96-100.

The controller must expose this behavior rather than pretending failover is lossless.

---

# 24. Synchronous Replication

Synchronous replication can be added later using PostgreSQL's native mechanisms.

The controller can dynamically manage synchronous replication based on replica health.

This is a future feature, not required for the first MVP.

---

# 25. Safety Invariants

The controller must explicitly enforce these invariants.

### Invariant 1

At most one PostgreSQL node is authorized as primary for the current HA generation.

### Invariant 2

A new primary cannot be authorized while the old primary may still accept writes.

### Invariant 3

A stale node cannot self-promote.

### Invariant 4

Raft does not replicate PostgreSQL data.

### Invariant 5

PostgreSQL state must be observed and verified after control operations.

### Invariant 6

Reconciliation must be idempotent.

### Invariant 7

PostgreSQL timeline and HA generation are distinct pieces of state.

### Invariant 8

Promotion must be recoverable if the agent crashes during the transition.

---

# 26. Important Crash Windows

The implementation must explicitly handle agent crashes at each point.

Example:

```text
Fence A
    ↓
agent crashes
    ↓
before B promotion
```

On restart, the agent must determine:

```text
A is fenced
B is not primary
cluster state says transition is in progress
```

and repeat the required checks. Before new authorization, missing or expired
primary evidence blocks progress even after fencing. After committed
authorization, resumption uses fresh local state and the recorded replay watermark
with current authority and fencing checks; it does not require a new sample from
the fenced primary.

Another case:

```text
B promoted
    ↓
agent crashes
    ↓
before recording promotion verification/completion in Raft
```

On restart, PostgreSQL says:

```text
B = primary
```

The agent must reconcile that against the Raft state. Primary authorization was
already committed before promotion; only its verified outcome is pending.

Another:

```text
Raft says B = primary
    ↓
agent crashes
    ↓
before PostgreSQL promotion
```

The next reconciliation may complete promotion only after the required current
authority, fencing, and local-state checks. The current reconciler retains the
original candidate during an unfinished transition and attempts to transfer
leadership back to it. If it cannot return, progress blocks; the FSM's rule for
fencing an uncertain candidate before replacement is not an implemented automatic
replacement workflow.

Therefore transitions should be represented as explicit, recoverable states rather than a single function call.

---

# 27. Membership

Raft membership and PostgreSQL membership should be coordinated but are not identical.
Current membership is fixed at three voters with persisted immutable container
identities. The add/remove flow below describes future work; container replacement
against surviving volumes and online membership changes are unsupported.

Adding a node involves:

```text
1. Add node to Raft membership.
2. Initialize PostgreSQL.
3. Configure replication.
4. Verify replica health.
5. Mark node available for HA.
```

Removing a node should first prevent it from being considered for promotion.

Maintenance mode should be supported eventually:

```text
node A = DRAINING
```

meaning:

```text
do not promote A
do not select A as failover candidate
```

---

# 28. Agent Failure vs PostgreSQL Failure

These must be treated differently.

### PostgreSQL fails, agent survives

The agent can observe:

```text
PostgreSQL unhealthy
```

and report/coordinate failover.

### Agent fails, PostgreSQL survives

The PostgreSQL instance should not automatically change role merely because its agent disappeared.

The development supervisor restarts only the agent after an agent crash, leaving
an already-running database in its existing role. The MVP policy is:

- Agent failure does not immediately promote another PostgreSQL node unless the Raft cluster and fencing policy authorize it.
- A node with no healthy agent should not be considered a safe promotion candidate.
- On agent restart, it observes and reconciles current state.

---

# 29. Raft Majority Loss

The controller must distinguish:

```text
PostgreSQL is healthy
```

from:

```text
control plane has quorum
```

If the Raft majority is unavailable, the remaining agent must not invent a new cluster state.

For example:

```text
A isolated
B isolated
C isolated
```

No agent has Raft majority.

The current controller blocks new authority and promotion without quorum. An
already-running primary can remain writable; there is no unilateral force-promotion
or majority-loss override workflow.

---

# 30. Persistent State

Raft persistent state must survive agent restarts.

The implementation should use durable storage appropriate for the Raft implementation.

PostgreSQL itself has its own persistent state.

Do not store the authoritative Raft log only in memory.

---

# 31. Security

Agent-to-agent communication should eventually support:

- TLS
- node authentication
- authenticated Raft transport
- authorization for administrative operations

PostgreSQL replication should use dedicated replication credentials.

Do not use superuser credentials for normal replication if avoidable.

Fencing credentials must be protected carefully because fencing is a high-privilege operation.

---

# 32. Observability

The agent should expose structured operational state.

At minimum:

```text
agent ID
Raft state
Raft term
cluster generation
current authorized primary
local PostgreSQL role
PostgreSQL timeline
WAL position
replication lag
upstream
reconciliation state
last successful health check
last failover
last fencing operation
```

Current JSON status includes leader identity/boolean and term, last quorum
confirmation, desired cluster state, local PostgreSQL observations, sampled lag
and age, recovery state, last reconciliation error/success, and durable transition
history. It does not expose a full Raft role enum, separate last-successful-health
timestamp, or durable history of every failed fence/health probe.
`local.database.healthy` indicates successful observation; replication readiness
additionally requires receiver, upstream, and replay verification. These limits
must remain explicit until the broader observability requirements are implemented.

Metrics should eventually include:

```text
postgres_up
postgres_role
postgres_timeline
replication_lag_bytes
replication_lag_seconds
raft_is_leader
raft_term
cluster_generation
reconciliation_errors
failover_count
fencing_failures
```

---

# 33. Initial MVP

Implement only:

```text
3 nodes
3 Go agents
HashiCorp Raft
PostgreSQL physical streaming replication
asynchronous replication
health monitoring
cluster membership
leader election
fencing abstraction
promotion
replica reconfiguration
pg_rewind-based rejoin
reconciliation
```

Initial happy path:

```text
A PRIMARY
B REPLICA
C REPLICA

A fails

B wins Raft
    ↓
Validate B with current quorum and fresh evidence
    ↓
Fence A
    ↓
Verify A is isolated; revalidate B
    ↓
Authorize B
    ↓
Promote B and verify role/timeline
    ↓
C follows B
    ↓
A returns
    ↓
A is rewound/reinitialized
    ↓
A follows B
```

---

# 34. Failure Tests

The system should eventually test:

```text
Primary process crash
Primary machine crash
Replica process crash
Replica machine crash

Agent crash
Agent restart

Network partition
Network flapping
Packet loss
High latency

PostgreSQL hang
Disk full
WAL problems

Two simultaneous promotion attempts
Old primary returns
Old primary remains alive during partition

Raft leader changes during failover
Raft majority loss

Fencing failure
Fencing verification failure

pg_rewind failure
Base backup required

Agent crash before fencing
Agent crash after fencing
Agent crash before promotion
Agent crash after promotion
Agent crash before Raft state update
Agent crash after Raft state update
```

These scenarios should be considered mandatory before calling automatic failover production-ready.

---

# 35. Core Design Summary

The architecture is:

```text
                 ┌──────────────────────┐
                 │       Raft           │
                 │                      │
                 │ Consensus            │
                 │ Membership           │
                 │ Generation           │
                 │ Primary authorization│
                 └──────────┬───────────┘
                            │
                            ▼
                 ┌──────────────────────┐
                 │      Go Agent        │
                 │                      │
                 │ Reconciliation       │
                 │ PostgreSQL control   │
                 │ Replication control  │
                 │ Fencing              │
                 │ Health monitoring    │
                 └──────────┬───────────┘
                            │
                            ▼
                 ┌──────────────────────┐
                 │     PostgreSQL       │
                 │                      │
                 │ WAL                  │
                 │ Streaming replication│
                 │ Replay               │
                 │ Timeline             │
                 └──────────────────────┘
```

The central principle is:

**Raft decides the cluster's control-plane state. PostgreSQL handles data replication. The Go agent reconciles PostgreSQL with the Raft-authorized desired state. Fencing prevents two PostgreSQL instances from being writable primaries simultaneously.**

This is the foundation of the custom Patroni-like controller.
