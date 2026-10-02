# Validation record

This is a development controller, not production-ready HA. Passing a unit test,
using real Raft, and exercising Docker/PostgreSQL end to end provide different
levels of evidence. Container failures on a single host do not validate physical
machine failures. Asynchronous replication can lose committed transactions.

## Recorded implementation checks

The implementation session on 2026-10-02 recorded passing unit/race checks,
real durable Raft restart/leadership-transfer/quorum tests, FSM snapshot restore
at every transition phase, and the explicitly enabled PostgreSQL 18 native
lifecycle test. Eight ordinary Compose scenarios, a real competing-proposal test,
29 deterministic transition cases, and an explicit operator rebuild passed.
At that stage, `make fmt`, `make test`, the then-existing `make vet`, `make build`,
and `go test -race ./...` were recorded as passing. Compose configuration
and image build, strict OpenSpec validation, local documentation links, and
checks excluding actual secret values from tracked content also passed. The
original six-scenario sequence passed together; the two additional replica
scenarios passed separately. Final normal/tagged Go race checks and the native
PostgreSQL lifecycle test were rerun after adding the deterministic fault hooks.

These are historical results, not fresh validation of later commits. The current
Makefile replaces `make vet` with `make lint`, includes tagged race and Python
checks in `make test`, and runs the isolated smoke runner via `make integration`.
For significant codebase changes, execute `make integration` as required by
[AGENTS.md](../AGENTS.md), plus the scenarios affected by the change. The smoke
target does not execute the full matrix. Documentation-only edits require link,
command, and spec/code consistency checks rather than fault injection.

| Check | Command or evidence | Scope and limitation |
| --- | --- | --- |
| Unit and script tests | `make test` | Normal Go tests, tagged race tests, and Python runner-isolation/cleanup tests. Native PostgreSQL lifecycle is skipped by default. |
| Race detector | `go test -race -tags maat_faults ./...` (included in `make test`); `go test -race ./...` for normal builds | Go concurrency paths exercised by the test suite; not a proof against every distributed race. |
| Current static checks/build | `make fmt`; `make lint`; `make build` | Formatting, pinned golangci-lint, and the local binary build. Historical vet results above do not establish a current lint pass. |
| Real Raft | `go test ./internal/cluster -run TestDurableRaftRestartTransferAndQuorum -count=1 -v` | Three TCP Raft nodes, BoltDB persistence, restart, leadership transfer and quorum loss. No PostgreSQL processes in this test. |
| Native PostgreSQL | `MAAT_PG_INTEGRATION=1 go test ./internal/postgres -run TestNativeLifecycle -count=1 -v` | PostgreSQL 18 tools, non-root user, ports 16541/16542. Real primary/replica, marker replay, promotion, crash recovery, rewind, retained rebuild, failed-rebuild retry and selected rename crash boundaries. No three-agent Docker coordination. |
| Docker adapter | `go test -race ./internal/fencing` | Real HTTP test servers and Unix socket transport; version negotiation, timeouts, API errors, exact identity, replacement, restart policy and separate stop verification. Synthetic daemon responses do not establish real container isolation. |
| Deployment assets | `sh -n deploy/entrypoint.sh deploy/prepare.sh`; `docker compose config --quiet` | Syntax/configuration passed. Credential-generation checks covered restrictive modes, no printed values, repeated preparation and symlink rejection. |
| Integration runners | `make integration` / `deploy/run_integration.py`; `deploy/transition_crashes.py`; `deploy/concurrent_transition.py` | The isolated wrapper runs smoke by default, or eight ordinary cases with `--scenario all`. Historical evidence below covers the underlying scenarios; deterministic cases and overlapping proposals use separate runners. |

The tests live in [cluster](../internal/cluster), [agent](../internal/agent),
[postgres](../internal/postgres), and [fencing](../internal/fencing).

## Compose suite results

The supported entry point, `make integration`, builds the Linux agent and invokes
[the isolated runner](../deploy/run_integration.py). Each invocation creates a
fresh `maat-integration-<unique-id>` project with separate identities, credentials,
volumes, network, and dynamic loopback status ports. It runs smoke by default and
stops its own containers afterward, retaining volumes and generated `bin/` assets.
After `make linux-build`, use `python3 deploy/run_integration.py --scenario all`
to run all eight ordinary cases. No existing `maat-dev` cluster is needed or
modified by this wrapper.

The historical commands in the table below used the lower-level
[scenario runner](../deploy/integration.py), which targets the existing
`maat-dev` lab directly. Run them only when injecting failures into that lab is
intended; use `deploy/run_integration.py` with the same scenario arguments for
a fresh isolated run. The scenario runner requires all three nodes to converge
before each case, never removes volumes or recreates containers, and pins container
IDs before injecting faults. Database checks use actual SQL roles, system IDs,
upstream/receiver state, and replayed markers. Failover checks require a stopped
old container when observing a replacement writer and verify persisted transition
ordering and incarnation-bound fencing proof.

| Scenario | Command | Recorded result |
| --- | --- | --- |
| Primary process failure, fencing, promotion and old-primary rejoin | `python3 deploy/integration.py --scenario smoke --timeout 150` | Passed in final suite: generation 8, primary a, old-primary rewind/replay verified. |
| Agent-only crash while PostgreSQL remains writable | `python3 deploy/integration.py --scenario agent-restart --timeout 150` | Passed in final suite: generation 8 and primary a retained. |
| Majority loss by stopping both replicas, then restoration | `python3 deploy/integration.py --scenario majority --timeout 150` | Passed in final suite: generation 8 and primary a retained, replicas returned. |
| Primary network isolation with PostgreSQL initially alive | `python3 deploy/integration.py --scenario partition --timeout 150` | Passed in final suite: old database verified writable before fencing; generation 9, primary c, rejoin verified. |
| Primary container SIGKILL, failover and rewind rejoin | `python3 deploy/integration.py --scenario container --timeout 150` | Passed in final suite: generation 10, primary a, three healthy nodes after rejoin. |
| Candidate container crash after committed authorization but before promotion | `python3 deploy/integration.py --scenario authorized-crash --timeout 150` | Passed: old-primary agent restored quorum while its PostgreSQL stayed stopped; candidate restart/leadership transfer completed promotion, generation 11 primary b, then full rejoin in the final suite. |
| Replica PostgreSQL process failure with primary writes and catch-up | `python3 deploy/integration.py --scenario replica-process --timeout 150` | Passed: generation 12 and primary a unchanged; marker replay verified after recovery. |
| Replica container SIGKILL with primary writes and guarded return | `python3 deploy/integration.py --scenario replica-container --timeout 150` | Passed: generation 12 and primary a unchanged; stopped container stayed stopped until explicitly restarted, then caught up. |
| Sequential scenarios | `python3 deploy/integration.py --scenario all --timeout 150` | Original six passed together at generations 7→11. `all` now also includes both separately validated replica cases. |

The final six-case sequence is recorded in `/tmp/maat-final-integration.log`.
Earlier runs are in `/tmp/maat-integration.log`, `/tmp/maat-container.log` and
`/tmp/maat-crash.log`. Replica results are in `/tmp/maat-replica-process.log`
and `/tmp/maat-replica-container.log`. These implementation-session logs are not versioned
artifacts. An explicit operator rebuild of replica a also completed,
retaining its old directory and verifying replication before these scenarios.

The runner emits phase/history evidence and failure diagnostics. One-second
polling samples state; it does not prove that no unobserved transient occurred
between samples. The persisted FSM ordering and independent Docker inspections
provide additional checks. A completed transition can exclude a still-fenced old
node; full three-node health is separately checked after rejoin.

## Deterministic transition and concurrency validation

The test-only `maat_faults` build pauses reconciliation at an acknowledged file
handshake. The runner verifies the precise committed phase, generation and SQL
role before killing the actual agent with SIGKILL. The supervisor restarts it
against the same PostgreSQL and durable Raft volumes. Ordinary binaries compile
these hooks to a no-op; they do not read the fault files.

All **29 cases passed** in new, uniquely identified Compose projects:

| Cases | Exact injection points | Result |
| --- | --- | --- |
| 12 commit-boundary crashes | Before and after `begin`, `fencing`, `authorize`, `promoting`, `reconfiguring`, and `complete` | Passed: committed state retained; no duplicate authorization/promotion. |
| 5 external-operation crashes | Before/after Docker fence, after fencing verification, before/after native promotion | Passed: isolation and actual role checked independently. |
| 6 crashes with quorum absent | Before/after fence, after authorization, before/after promotion, before completion | Passed: restarted agent held role and authority unchanged for six seconds without peers, then reconciled after quorum restoration. |
| 6 crashes after leadership change | The same six unsafe windows | Passed: another leader elected while the candidate was paused; restarted candidate retained or safely resumed the existing transition. |

Before authorization, loss of the local primary-observation cache intentionally
blocks new authority while the original primary is unhealthy or fenced. These
cases prove safe refusal, not automatic availability. After authorization, the
candidate resumes from current quorum, live fencing checks, fresh local state,
and the recorded replay watermark. An already-promoted primary may remain
writable during quorum loss; no new promotion or authority is allowed.

The separate competing-proposal test paused an eligible leader before its
`begin` command, elected another eligible leader at the same generation, then
released both pending proposals. Exactly one `begin` and one `authorize`
committed, the stale leader's proposal was rejected, one replacement became
writable after fencing, and the old primary rejoined with the marker intact.

Reproduce the tested matrix from the repository root:

```sh
make fault-build
python3 deploy/concurrent_transition.py --cleanup --timeout 150
python3 deploy/transition_crashes.py --binary bin/maat-faults-linux --point all --mode crash --remove-test-volumes --report /tmp/maat-transition-crashes.json
for mode in quorum leadership; do
  for point in before-fence after-fence after-authorize before-promote after-promote before-complete; do
    python3 deploy/transition_crashes.py --binary bin/maat-faults-linux --point "$point" --mode "$mode" --remove-test-volumes --report "/tmp/maat-transition-${mode}-${point}.json"
  done
done
```

The cleanup options above explicitly remove only newly created disposable test
projects and volumes. Without those options, cleanup stops containers and keeps
storage. Generated configuration/credentials remain in ignored `bin/` folders.
The normal `maat-dev` cluster is never targeted by these two runners.

Session evidence is in `/tmp/maat-transition-matrix-summary.json`,
`/tmp/maat-transition-matrix-evidence.json`, and `/tmp/maat-concurrent-final.log`.
Reports include exact boundary observations, changed incarnations, SQL recovery
roles, quorum holds/leader changes, and ordered transition history. They are
session artifacts; the checked-in runners and this record provide reproducibility.

## Specification section 34 matrix

Every failure scenario from [section 34 of the technical specification](<Custom PostgreSQL HA Controller — Technical Specification.md>)
is mapped below. “Partial” means the named boundary is exercised, with additional
failure injection still needed. The passed Compose cases establish the bounded
scenario described, not exhaustive
coverage of all timings or production fault models.

| Required scenario | Existing evidence | Remaining validation |
| --- | --- | --- |
| Primary process crash | Native lifecycle and Compose `smoke` passed, including coordinated fence/promotion/rejoin. | Repeated crash timing under sustained write load. |
| Primary machine crash | No physical-host test. | Separate machines and independent fencing; containers sharing one Docker host are insufficient. |
| Replica process crash | Native lifecycle and Compose replica-process/replica-container cases passed with writes and catch-up. | Sustained load and additional storage faults during replica restart. |
| Replica machine crash | No physical-host test. | Separate-machine failure and recovery. |
| Agent crash | Observation-incarnation/cache tests and Compose `agent-restart` passed with PostgreSQL remaining writable. | Other roles and repeated crash timing under write load. |
| Agent restart | Durable Raft, journals, ordinary Compose restart and all 29 deterministic transition cases passed. | Repeated randomized crash schedules and power-loss effects. |
| Network partition | Compose `partition` and real Raft quorum tests passed. | Broader asymmetric partitions and independent host fencing remain unvalidated. |
| Network flapping | No fault-injection test. | Repeated partitions/heals with client writes and leadership changes. |
| Packet loss | No fault-injection test. | Controlled loss on Raft, observation, replication and Docker paths. |
| High latency | Docker timeout tests passed; observation age uses request start. | Sustained and asymmetric latency across the full cluster. |
| PostgreSQL hang | Bounded operations and stale/failed observation rejection are tested. | Real hung PostgreSQL while agents/containers remain alive. |
| Disk full | Free-space validation is implemented. | Real ENOSPC during WAL, Raft writes, rewind, backup and journal/rename phases. |
| WAL problems | Native replay, timeline checks, slots and finite retention exercised. | WAL corruption, missing segments, exhausted slot retention and replay failures. |
| Two simultaneous promotion attempts | Real agents held overlapping validated proposals across a leader change; exactly one transition/authorization committed and one replacement became writable. | Repeated contention combined with asymmetric network and storage faults. |
| Old primary returns | Native rewind and Compose smoke/partition/container/authorized-crash guarded rejoin passed. | Additional divergent histories and failure timings. |
| Old primary remains alive during partition | Compose `partition` passed, checking live writable SQL before Docker fencing. | Physical-host isolation remains unsupported. |
| Raft leader changes during failover | Six deterministic leadership windows plus the competing-proposal and authorized-candidate scenarios passed. | Randomized combinations, sustained write load and repeated elections. |
| Raft majority loss | Real Raft, ordinary Compose majority loss and six deterministic unsafe windows passed without new authority/promotion. | Asymmetric all-agent partitions and extended network degradation. |
| Fencing failure | Docker HTTP errors, timeout, unreachable/canceled request, wrong identity and unsafe restart policy tests passed. | Live controller with unavailable/denied Docker API and client writes continuing. |
| Fencing verification failure | Tests reject 404, malformed/missing state, replacements, running/restarting/paused containers and stop success without isolation. | End-to-end daemon uncertainty or misleading stop response under concurrent promotion attempts. |
| `pg_rewind` failure | Interrupted-rewind journal rejects ordinary startup; native successful rewind passed. | Real mid-rewind process kill, storage errors and generation/source changes. |
| Base backup required | Native retained rebuild/retry and full operator CLI-to-Raft-to-replica rebuild passed. | Interrupted/failed backup across all filesystem boundaries. |
| Agent crash before fencing | Real SIGKILL before/after validation and fencing-phase commits, and before Docker fencing, passed with fresh-evidence refusal after restart. | Randomized overlap with daemon/storage failures. |
| Agent crash after fencing | Real SIGKILL after Docker stop, after verification and before authorization passed; missing evidence blocked new authority. | Host power loss and ambiguous infrastructure fencing outcomes. |
| Agent crash before promotion | Real crashes around authorization/promoting commits and before native promotion passed; six-window leadership/quorum matrix included this boundary. | Randomized overlapping faults during native subprocess execution. |
| Agent crash after promotion | Real SIGKILL after native promotion and around reconfiguration/completion commits passed; native test also covers promotion ahead of its journal. | Power-loss/fsync behavior and arbitrary instructions inside PostgreSQL control calls. |
| Agent crash before Raft state update | Real SIGKILL immediately before each of the six failover commands passed, alongside snapshot/CAS regressions. | Arbitrary interruption inside Raft storage writes. |
| Agent crash after Raft state update | Real SIGKILL immediately after each of the six committed failover commands passed. | Power loss before storage-device flush completion. |

The [archived initial change](../openspec/changes/archive/2026-10-02-initial-compose-ha-controller/tasks.md)
records its development tasks as complete. This is not a claim that every target
requirement or later change is fully validated. The deterministic
cases cover the named transition boundaries and competing proposals; they do not
exhaust every instruction-level interleaving or production failure model. The
remaining validation column continues to identify those broader limitations.

## Operational limitations

- Three containers on one host cannot survive loss of that host. Docker access is
  a privileged trust boundary and must remain available to fence a possible writer.
- Manual PostgreSQL starts, external restarts or changed Docker restart policies
  violate the supported fencing assumptions. Preserve recorded container IDs.
- Fixed membership has no container-replacement or rolling-upgrade workflow.
  Retaining volumes while recreating containers triggers identity rejection.
- Native recovery tests cover selected interrupted states, not every possible
  crash window, storage failure or malicious filesystem race.
- Peer transport, Raft transport and status are unauthenticated and unencrypted.
  Local administration uses OS socket permissions; production authorization,
  credentials/secret rotation and independent fencing remain future work.
- The leader-local asynchronous candidate policy can lose committed transactions;
  observed lag cannot determine final transaction loss. Strict freshness can block
  failover indefinitely instead of inventing fresh evidence.
- Candidate eligibility compares observed timeline IDs, not parsed history;
  broader timeline-ancestry policies are not implemented. Native PostgreSQL tools
  handle history/WAL validation during recovery.
- An unfinished transition waits for its original candidate. FSM unit tests cover
  fencing an uncertain authorized writer before replacement, but the reconciler
  does not automatically initiate that replacement workflow.
- Status reports leader/term and last quorum/reconciliation timestamps, but lacks
  a full Raft role enum, separate last-successful-health timestamp, and durable
  history of failed health/fencing attempts. Database observation health alone
  does not prove streaming/replay readiness.
