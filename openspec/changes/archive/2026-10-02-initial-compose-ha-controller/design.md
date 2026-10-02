## Context

The existing executable has no HA behavior. This change implements a development
cluster for the maintainers of Maat, following the
[technical specification](../../../../docs/Custom%20PostgreSQL%20HA%20Controller%20%E2%80%94%20Technical%20Specification.md).
The interview established Docker Compose, replaceable Docker fencing, an
operator gate for base-backup recovery, and a configurable 16 MiB lag limit.
Other defaults below are proposed implementation choices.

## Goals / Non-Goals

**Goals:** a runnable three-node asynchronous cluster; durable control state;
verified fencing and promotion; native replication; recoverable, idempotent
transitions; safe rewind and explicit rebuild; inspectable status; meaningful
failure tests.

**Non-goals:** production certification, lossless failover, arbitrary existing
cluster adoption, online membership changes, client routing, synchronous
replication, additional infrastructure backends, transport authentication/TLS,
remote administrative APIs, or a metrics system.

## Decisions

### 1. Three combined node containers with guarded PostgreSQL startup

Each node contains the Go agent and PostgreSQL 18 binaries, with separate durable
Raft and PostgreSQL directories. A minimal container supervisor starts/restarts
the agent; PostgreSQL starts only through the agent's checked control path. It
does not use an image entry point that automatically starts an existing database.
An agent-process crash can leave PostgreSQL running, permitting the separate
agent-failure behavior required by the specification. Stopping the container
fences both processes. Use Docker's init support for signal handling/reaping and
verify the supervisor's actual crash and shutdown behavior in integration tests.

An agent restart first observes any already-running PostgreSQL and obtains
current control-plane state. A new container start leaves PostgreSQL stopped
until authorization or a verified standby configuration permits startup.
If current state cannot be established, no database is newly started writable.
An existing authorized primary is not automatically demoted solely because its
agent or quorum is temporarily unavailable; any replacement still needs fencing.
A detected unauthorized writable instance is stopped and its stopped state
verified before any recovery work.

Use `restart: "no"` for the development node containers. Returning a fenced
container is an explicit operator action; startup does not restore a primary
role from the old data directory. The Docker socket belongs to the agent's
runtime, and PostgreSQL runs as its unprivileged database user. Never execute
PostgreSQL tools as root.

Separate agent/database containers were considered, but require a second
process-control channel and coordination of shared data directories. A separate
controller service conflicts with the intended deployment. The small supervisor
exists specifically to distinguish agent and PostgreSQL process failures.

### 2. Durable Raft state and fixed membership

Use HashiCorp Raft, `raft-boltdb/v2` for durable log/stable storage, and file
snapshots. Do not implement consensus or a custom durable log. Keep a single
deterministic FSM for cluster identity, three-member topology, generation,
authorized primary, active transition, recovery requests, and relevant history.
PostgreSQL state is observed, never inferred from desired state.

Each member records node ID, Raft and observation addresses, database endpoint,
and Docker container identity. The three voters are fixed for this change.
Use one explicit bootstrap seed to create the configured voter set; other nodes
never bootstrap themselves on a timeout. Existing durable state always takes
precedence over bootstrap configuration. Reject conflicting identity/topology,
unrecognized nonempty database directories, or lost Raft state with surviving
database data; report operator recovery instead of inventing a new generation.

Fresh bootstrap verifies all managed data directories are empty and commits the
initial designated primary at generation 1 before starting it. Generation-zero
state cannot promote a replica. There is no previous primary to fence only in
this explicitly verified fresh-cluster path.

FSM commands carry the expected generation, transition ID, and phase. Reject
conflicting or stale commands and make exact retries idempotent. Retain the
authorized identity during uncertain promotion; never forget a possible writer
merely because a completion report is missing.

### 3. Observation, eligibility, and simple leader-local promotion

Agents observe local PostgreSQL and exchange bounded HTTP status responses on
the private Compose network. Every agent collects peer observations so a new
leader can have its own recent evidence. Observations include agent incarnation,
database system identifier, role, timeline/history, flush/replay LSN, receiver
state, upstream, and recovery/maintenance eligibility. Unknown data is explicit.
Malformed LSNs, incompatible histories, or identity changes are not zero lag.

Use receiver-side monotonic request/response timing for freshness; a peer's wall
clock cannot make old evidence fresh. Each response must represent a new local
database observation, not a cached healthy result. Discard observation caches
on agent restart and invalidate affected samples on identity/generation changes.
Do not persist a wall-clock timestamp and treat it as a fresh promotion proof.

Proposed defaults: observe every second, consider failover after three
consecutive failed primary observations, and reject evidence older than 30
seconds. All are validated configuration values. Fencing and promotion have
bounded operation deadlines. A timeout never establishes successful fencing.

Require current membership/quorum, a responsive candidate agent, healthy
PostgreSQL in recovery, compatible system identifier and timeline history,
unpaused replay, acceptable replay position, no fence/recovery exclusion, and
fresh evidence. Default `max_promotion_lag_bytes` to 16,777,216, with zero allowed.
Compare replay against a recent primary flush position on compatible history;
if the replica has passed that sampled position, its observed lag is zero.
The last primary sample is only an estimate of loss, never its final WAL bound.
An unavailable old-primary WAL receiver alone does not disqualify an otherwise
healthy replica during failover; replay validity is checked separately.

Prefer promotion of an eligible Raft leader's local PostgreSQL. If the leader is
the failed primary or otherwise ineligible, transfer leadership through the
Raft library to an eligible replica, using node ID as a deterministic tie-break.
The new leader independently validates before acting. If transfer or validation
fails, report a blocked transition. No remote fire-and-forget promotion command
is introduced. Selecting the most advanced replica regardless of leader is
deferred; this simple policy can lose more data than an optimal candidate policy.

### 4. Fence first, then commit authority, then promote

Use durable transition phases `validating`, `fencing`, `authorized`, `promoting`,
`reconfiguring`, and `complete`, with a recorded blocked/error reason. Desired
roles and observed roles remain separate. PostgreSQL timeline is independent of
HA generation.

1. A leader with current quorum commits a transition targeting its eligible node
   and identifying the old primary, expected generation, and container identities.
2. Revalidate candidate and evidence, then commit `fencing` before external action.
3. Invoke `Fencer.Fence(ctx, target)` and separately
   `Fencer.IsFenced(ctx, target)`. The target binds node, cluster, and container
   incarnation; a node name alone is insufficient.
4. Reobserve candidate eligibility and verify the old primary is still fenced.
   Commit one generation increment and the replacement primary with a
   compare-and-set transition command. Keep the required replay watermark and
   candidate history used for that decision. Stale/missing evidence blocks this
   new authorization even if fencing has already completed.
5. Before local promotion, confirm current leadership/quorum, apply committed
   state, recheck authorization and local PostgreSQL, and verify fencing again.
   Run `pg_promote` with a deadline. Verify recovery is false and record the new
   observed timeline before advancing. Unknown outcome remains recoverable.
6. Reconfigure each remaining replica toward the authorized, verified primary;
   verify receiver and replay before marking it healthy.

Once a candidate is authorized, it must be treated as a possible writer even if
it was last observed in recovery. A subsequent replacement must fence that
candidate's exact container before changing authority again. This closes the
race with a delayed promotion by the previous leader. A leadership change does
not by itself revoke primary authorization.

Crash handling is state-based:

| Crash point | Recovery action |
| --- | --- |
| Before fencing | Revalidate and retry under the same transition, or block |
| After fence, before authorization | Inspect fencing again; require fresh eligibility evidence before authorizing |
| After authorization, before promotion | Retain the sole authorization; confirm quorum, fencing, and fresh local state before resuming |
| After promotion, before completion | Observe the database; if authorization still matches, record verified completion without promoting again |
| After authority changed elsewhere | Never resume old work; stop any unauthorized writable database and enter recovery |

Resuming an already committed authorization validates the recorded replay
watermark/history and current local health; it does not demand a new health
sample from the permanently fenced old primary. This differs from making a new
authorization, where fresh pre-fencing primary evidence is required. Without
quorum, no promotion or new authority proceeds. Expired evidence can intentionally
leave the cluster unavailable rather than permit an unsupported safety claim.

### 5. Docker fencing is an adapter with an explicit trust boundary

Use a small interface, independent of Docker response types, with separate fence
and verification methods. Other environments can provide their own target
configuration and adapter later; do not scaffold those implementations now.

Use `net/http` over the mounted Unix socket for version discovery, bounded
stop, and inspect operations. Negotiate a supported Docker API version and test
error/timeout handling. Resolve the configured cluster/node labels to exactly
one container and pin its immutable Docker ID in membership/transition state.
Validate identity and disabled automatic restart before acting. A conflicting
replacement container, unexpected label, or changed incarnation blocks the
operation rather than fencing a similarly named container.

After stop, inspect that exact container and verify it is neither running nor
restarting, with automatic restart disabled. API success, TCP failure, pause,
HTTP 404, an unreachable daemon, and ambiguous identity are not proof. Once the
node starts again, old stopped-container evidence no longer authorizes anything.
The supported startup path always keeps PostgreSQL stopped until the agent has
reconciled current authority. Container recreation is not automatic membership
replacement; it requires an explicit future recovery workflow or a fresh lab.

The guarantee assumes a trusted Docker daemon and exclusive use of the managed
PostgreSQL startup/control path. An administrator manually executing PostgreSQL
or changing restart policy can defeat fencing. Docker is not independent
hardware fencing for a failed host, and loss of its API blocks automatic failover.

### 6. PostgreSQL owns replication and recovery

Use a PostgreSQL driver for bounded observations and SQL control, and
`exec.CommandContext` argument arrays for `initdb`, `pg_ctl`, `pg_basebackup`,
`pg_controldata`, and `pg_rewind`. Do not construct shell commands from node data.
Use matching PostgreSQL 18 server/client tools, checksums and `full_page_writes`
for rewind prerequisites, dedicated replication credentials, and separate
privileges for observation/control and rewind where required. Credentials come
from restrictive mounted/password files; logs redact connection strings/errors.

Fresh replicas use native base backup, standby configuration, and the current
primary as upstream. Use one physical replication slot per replica with finite
WAL retention. Verify receiver identity, streaming state, and replay catch-up to
an observed upstream watermark; an idle cluster need not produce new WAL solely
to be declared healthy. Revalidate generation around long-running work.

Reconfigure upstream idempotently and verify the effect. A timeline change alone
does not imply divergence: inspect history. Compatible replicas can follow the
new primary; an old writable primary is stopped and inspected before rewind.
Persist a local recovery journal before modifying the data directory. Correct
standby configuration after rewind before any PostgreSQL start. A crash in a
partially completed rewind does not authorize an ordinary database startup.

When rewind is unsafe, fails, or cannot be shown complete after interruption,
leave PostgreSQL stopped and publish `reinitialization_required`. Do not fall
back automatically. The operator's `maat reinitialize` request identifies the
node, expected generation, and explicit data-replacement acknowledgement. It
must reach the leader, be committed, and be revalidated by the target agent.
Keep administrative entry local (Unix socket/CLI), not a published HTTP API.

Validate the configured data root, ownership, canonical target, PostgreSQL
identity, free space, stopped state, and current source before replacement.
Reject symlinks, unsafe roots, unexpected tablespaces, and a primary target.
Retain the old directory by atomic rename within its managed parent; create and
verify a fresh backup before selecting it for startup. A durable phase journal
makes interrupted rename/backup/configuration steps recoverable. Never delete
the retained directory automatically; operator cleanup is separate. A changed
generation invalidates source assumptions and requires revalidation.

### 7. Small operational interface and visible limitations

Use strict JSON configuration with unknown-field rejection and validated unique
IDs, addresses, paths, three peers, thresholds, and secret-file references.
Run with a configuration path; avoid a config framework. Keep packages limited
to code actually needed for Raft state, PostgreSQL control, fencing, and the
agent loop. Use standard `log/slog`, HTTP, contexts, and Go tests.

Expose a private read-only JSON status endpoint and a local `maat status` command.
Report node identity, Raft state/term/quorum observation, generation, authorized
primary, observed PostgreSQL role/system identifier/timeline, LSNs, lag and sample
age, upstream, transition/recovery state, last successful health observation,
and last failover/fence outcome. Preserve significant transition history in
durable state. Unknown/stale state must remain distinguishable from healthy.
Report estimated lag alongside unknown final transaction loss.

Compose binds any host database/status ports to loopback. Secrets are supplied
in ignored files through Compose mounts. No tool may claim the private network
provides node authentication. Bootstrap/status/reinitialize workflows are
documented with exact tested commands. Existing Makefile commands retain their
meaning; add a separate explicit integration-test entry point if needed.

## Risks / Trade-offs

- Asynchronous and leader-local promotion can lose committed transactions →
  expose observed lag, evidence age, candidate policy, and unknown final loss.
- Strict freshness can block failover after a long outage or restart → surface
  the reason; no implicit override or force-promotion command.
- Docker access can control the development host → document the privileged
  trust boundary and keep the deployment isolated and local.
- Three containers on one host do not survive host failure → distinguish
  simulated node/process/network tests from physical infrastructure tests.
- Fencing removes a Raft voter → tolerate one loss; after another loss, stop
  making changes until quorum returns.
- Interrupted native recovery can corrupt the target copy → keep it stopped,
  journal progress, and require explicit reinitialization when uncertain.
- Retained old directories and replication slots consume disk → finite slot
  retention, visible disk failures, and explicit operator cleanup.
- Security and full production failure coverage are deferred → README lists
  actual test evidence and unimplemented guarantees explicitly.

## Validation and Migration Plan

Implement and validate in dependency order: durable FSM and config; native
PostgreSQL lifecycle; Docker fencing; failover/recovery reconciliation; Compose
and end-to-end failure tests. Unit tests focus on safety branches and malformed
inputs. Integration tests must use real durable Raft and real PostgreSQL/Docker,
not substitute in-memory stores as evidence of restart safety.

The required development demonstration covers bootstrap, one-primary writes,
replication, primary process failure, container failure, agent-only restart,
network partition with the old database alive, majority loss, fencing and
verification failures, leadership change, simultaneous transition attempts,
lag/freshness rejection, replica upstream change, old-primary return, rewind
failure/operator rebuild, and crashes around every transition step. Map every
section 34 scenario to an implemented test or an explicit unvalidated limitation;
do not equate container tests with physical-machine validation.

Run `make fmt`, `make test`, `make vet`, and `make build` for code. Document
prerequisites and blocked checks. Preserve the current Go toolchain requirement;
missing tooling is a reported blocker, not a reason to silently lower it.

Deploy initially only with fresh managed volumes. Rollback stops the development
cluster while retaining database and Raft volumes; reverting the executable to
the startup-only version does not restore HA. Never use volume deletion as an
implicit rollback. Changes to persisted schemas will require explicit version
handling; the first version rejects unknown formats.

## Open Questions

The product-policy decisions requested in the interview are resolved. Review
the proposed PostgreSQL major version, fixed-membership boundary, combined-node
layout, freshness defaults, and local operator workflow before implementation.
Exact compatible dependency/image pins and available Docker/Go tooling are
implementation prerequisites to verify, not claims that those environments
already exist. Container recreation, multi-host fencing, and production security
need separate future designs.

## References

- [HashiCorp durable Raft backend](https://github.com/hashicorp/raft-boltdb)
- [Docker Engine API](https://docs.docker.com/reference/api/engine/)
- [Docker restart policies](https://docs.docker.com/engine/containers/start-containers-automatically/)
- [PostgreSQL rewind prerequisites and failure behavior](https://www.postgresql.org/docs/18/app-pgrewind.html)
