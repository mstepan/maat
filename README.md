# maat

A Go controller for a three-node PostgreSQL 18 development cluster. One agent runs
beside each database; HashiCorp Raft and BoltDB persist primary authorization,
HA generations, transitions, and recovery requests. PostgreSQL performs native
asynchronous physical replication. No etcd or separate controller service is used.

**Status: implemented development controller; not production-ready.** The
[validation record](docs/validation.md) reports unit/race, real Raft restart/quorum,
and native PostgreSQL lifecycle checks, eight ordinary Compose scenarios,
29 deterministic transition cases, a competing-proposal test, and an explicit
operator rebuild. The original six ordinary scenarios passed together; the two
replica scenarios passed separately. These are recorded implementation-session
results, not a guarantee that every checkout or environment passes the matrix.
Asynchronous failover can lose acknowledged transactions. The leader-local
candidate policy does not necessarily choose the replica with the most WAL.

The [technical specification](<docs/Custom PostgreSQL HA Controller — Technical Specification.md>)
is the source of truth for architecture and safety requirements.

## Why maat?

The name comes from [Maat (Ma'at)](https://en.wikipedia.org/wiki/Maat), the
ancient Egyptian concept of truth, balance, order, and justice, and the goddess
who personified it. For this project, the connection is a metaphor: the controller
aims to maintain an orderly PostgreSQL cluster through consensus, reconciliation,
and a single authorized primary.

[![Maat depicted with an ostrich feather on her head](https://thumb.wikimedia.org/wikipedia/commons/thumb/a/ab/Maat.svg/250px-Maat.svg.png)](https://commons.wikimedia.org/wiki/File:Maat.svg)

*Illustration from the Wikipedia article: Jeff Dahl, with revisions by A. Parrot,
via [Wikimedia Commons](https://commons.wikimedia.org/wiki/File:Maat.svg),
[CC BY-SA 4.0](https://creativecommons.org/licenses/by-sa/4.0/). Shown unchanged.*

## Implemented scope

- Fixed three-member Raft topology, fresh-storage bootstrap, durable log/snapshot
  state, current-quorum checks, and leadership transfer.
- PostgreSQL observation, initialization, physical replication with a dedicated
  `maat_repl` role, finite-retention replication slots, and upstream reconfiguration.
- Docker socket fencing with immutable container IDs, disabled restart policy,
  bounded API calls, and separate inspection of stopped state.
- Explicit `validating → fencing → authorized → promoting → reconfiguring → complete`
  transitions, guarded promotion, and restart reconciliation.
- Journaled rewind and explicit operator-approved rebuild that retains old data.
- Strict JSON configuration, structured logs, read-only status, and a local
  administrative Unix socket.

Raft leadership and PostgreSQL primary status are separate. HA generation and
PostgreSQL timeline are also separate. Failure detection initiates validation;
it does not grant permission to promote. The agent fences and verifies the old
writer before committing replacement authorization, then verifies promotion and
replica configuration. Without a majority, it does not grant new authority.
An authorized candidate is a possible writer even when a promotion result is
unknown, so replacing it requires fencing that candidate too.
The FSM enforces that rule, but the reconciler currently resumes an unfinished
transition only on its original candidate. A new leader attempts to transfer
leadership back to that candidate; if it is unavailable, the transition blocks
and retains authority. Automatic replacement of an unfinished candidate is not
implemented.

Defaults in [deploy/instance-a.json](deploy/instance-a.json) are one-second observations, three
failed observations, a 30-second evidence age limit, and 16 MiB maximum observed
promotion lag. Lag is measured against the last accepted primary sample, not the
unobservable final WAL position. Missing/stale evidence can leave the cluster
unavailable. Agents invalidate old evidence after a peer incarnation changes.
Candidate validation requires equal observed timeline IDs and matching database
system identity; it does not compare timeline-history contents. It can wait for
replica restartpoints after a promotion; the integration runner checkpoints and
verifies timeline alignment. PostgreSQL's native recovery tools handle history
and WAL compatibility during rejoin.

## Start a fresh development lab

Requires Go 1.27.1 or newer, Docker Engine with Compose, Python 3, and OpenSSL.
`make lint` runs golangci-lint v2.14.0 through Go, locally and in CI. The first run
downloads and builds the tool into Go's caches; no separate installation is needed.
Docker API versions 1.44–1.54 are supported through version negotiation. Docker
must expose its socket at `/var/run/docker.sock`. The image is pinned to
PostgreSQL 18 Bookworm by digest in [Dockerfile](Dockerfile).

Run from the repository root, with no existing `maat-dev` cluster or old volumes:

```sh
make fmt
make test
make lint
make build
make compose-up
```

`make compose-up` cross-compiles the agent for the Docker server architecture,
builds the pinned image, prepares ignored `.secrets/` credentials, and starts
three containers without recreating existing ones. A rebuilt image is not applied
to existing containers; upgrading them requires a separate recovery workflow.
`MAAT_DOCKER_ARCH=arm64` or `amd64` can override architecture
detection. Initial bootstrap is automatic: node `instance-a` seeds Raft, and a committed
fresh-storage decision authorizes the initial database primary `instance-a`.
Compose services, hostnames, and controller node IDs are `instance-a`,
`instance-b`, and `instance-c`. Replication slots use underscores, for example
`maat_instance_c`.

Each node has separate database and control volumes. PostgreSQL data lives at
`/var/lib/maat/postgres/data`, beneath its volume root so recovery can rename it
atomically. Raft and recovery journals live under `/var/lib/maat/control`.
The supervisor starts only the agent; the agent decides whether PostgreSQL can
start. An agent crash is restarted without stopping an already-running database.

**Container IDs are persisted membership identities. Never recreate containers
against surviving volumes.** `docker compose up --force-recreate` or
`docker compose down` followed by `up` can change those IDs and is not a
supported upgrade/recovery path. Membership
replacement and in-place upgrades need a separate workflow. Do not erase Raft
state to work around an identity mismatch.

For an existing lab, preserve the containers and volumes:

```sh
make compose-down
docker compose start
```

Ordinary cleanup is `make compose-down`; it does not delete volumes. Retain
both PostgreSQL and control storage when investigating failures or rolling back.
The previous startup-only binary cannot operate or recover the HA cluster.

To discard the entire development lab and start with fresh identities and data,
run `make compose-clean`, then `make compose-up`. This removes the Compose
containers, volumes, network, local images, and generated lab credentials. It
permanently deletes database and Raft state; use it only when a fresh lab is
intended.

## Observe and exercise the cluster

Host ports bind only to loopback:

| Node | PostgreSQL | Read-only status |
| --- | --- | --- |
| `instance-a` | `127.0.0.1:15432` | `http://127.0.0.1:18080/status` |
| `instance-b` | `127.0.0.1:15433` | `http://127.0.0.1:18081/status` |
| `instance-c` | `127.0.0.1:15434` | `http://127.0.0.1:18082/status` |

```sh
curl -fsS http://127.0.0.1:18080/status | python3 -m json.tool
docker compose exec -T --user postgres instance-a maat status --config /etc/maat/config.json
docker compose exec -T --user postgres instance-a psql \
  -h /var/lib/maat/control/postgres/socket -U postgres -d postgres \
  -AtX -v ON_ERROR_STOP=1 -c 'SELECT pg_is_in_recovery();'
```

Status separates desired `state.Primary`/`state.Generation` from the observed
`local.database` role, system identifier, timeline, WAL and replay positions,
receiver, and upstream. It also exposes Raft leader/term, evidence age,
reconciliation errors, recovery state, and durable `state.History`. A local
status response or elected leader alone does not prove current quorum.
`local.database.healthy` means database observation succeeded, not that replication
is caught up. Check receiver/replay state and `reconciliation_error` too.
`quorum_last_confirmed_at` and `last_successful_reconcile` are historical timestamps;
there is no separate last-successful-health-check field or full Raft role enum.

`make integration` builds the Linux agent and starts a fresh Compose project named
`maat-integration-<unique-id>`. Each run has its own cluster/fencing identity,
credentials, volumes, network, and dynamically assigned loopback status ports.
It publishes no PostgreSQL host ports, so it can run alongside the `maat-dev`
cluster started by `make compose-up`.

The runner waits for a healthy three-node topology before injecting failures and
stops only its own project afterward, including after test or startup failure.
Test or shutdown failures still make the command fail. Containers and volumes
are retained for inspection; the generated Compose file, configuration,
credentials, and binary copy are kept in the printed `bin/maat-integration-*`
directory. Each invocation uses fresh storage.

The explicit integration runner injects failures into this dedicated lab and
leaves marker rows as evidence. It checks SQL
roles, replay, container isolation, and transition history, then attempts normal
rejoin. On failure it restores its stopped replicas/disconnected network; an old
primary fenced during an incomplete transition remains stopped for inspection.

```sh
make integration                                      # primary process failure and rejoin
python3 deploy/run_integration.py --scenario agent-restart
python3 deploy/run_integration.py --scenario majority
python3 deploy/run_integration.py --scenario partition
python3 deploy/run_integration.py --scenario replica-process
python3 deploy/run_integration.py --scenario replica-container
python3 deploy/run_integration.py --scenario container
python3 deploy/run_integration.py --scenario authorized-crash
python3 deploy/run_integration.py --scenario all --timeout 120
```

These are lab fault-injection commands, not health checks. They never delete
volumes. See [validation results](docs/validation.md) before interpreting coverage.
The Python commands use the binary built by `make integration` or `make linux-build`.
The lower-level `deploy/integration.py` still targets an already-running `maat-dev`
cluster; use it directly only when you intend to inject faults into that local lab.

Deterministic crash and competing-proposal tests use a separate binary whose
filesystem pause hooks are excluded from normal builds. Each run creates fresh
Compose projects with unique identities and loopback ports; it does not change
`maat-dev`. The following cleanup flags explicitly remove only these newly
created test projects and their disposable volumes. Without them, containers are
stopped and storage is retained under the generated project names.

```sh
make fault-build
python3 deploy/concurrent_transition.py --cleanup --timeout 150
python3 deploy/transition_crashes.py --binary bin/maat-faults-linux --point all --mode crash --remove-test-volumes
```

Test configuration and credential files are generated under ignored `bin/`
directories. Do not deploy the fault-enabled binary as the normal controller.

## Recover an old primary or rebuild a replica

After a completed failover, restart the same stopped container, for example
`docker compose start instance-a`. The guarded startup path keeps PostgreSQL stopped
until current authority permits rejoin. Compatible replicas follow the new
upstream; an old primary uses native `pg_rewind`, standby repair, and verification.

Unsafe, failed, or interrupted recovery reports `reinitialization_required`.
There is no automatic data replacement. Inspect status and resolve its cause;
if rebuilding is intended, read the current `raft_leader`, `state.Generation`,
and `state.Primary`. Run the following on the current leader, using the current
generation and a target that is **not** the authorized primary. This example
assumes leader `instance-b`, target `instance-a`, and generation `2`. The target's
agent must be running, but its PostgreSQL must already be verified stopped;
reinitialization does not stop a running database for the operator:

```sh
docker compose exec -T --user postgres instance-b maat reinitialize \
  --config /etc/maat/config.json --node instance-a --generation 2 --ack-data-replacement
```

The local socket is protected by filesystem permissions. A follower rejects the
request with the leader identity; repeat on that leader after checking current
state. A successful response means the request was committed, not that recovery
finished. Watch the target's recovery state and verify a streaming replica with
the expected system identity, upstream, and replay position. Stale generations,
a primary target, an active failover, or unavailable quorum block the request.

Rebuild validates paths, identity, stopped state, source, and available space;
it retains old data as `/var/lib/maat/postgres/data.retained-<request-id>` and
prepares a native base backup in a sibling staging directory. Recovery journals
allow supported rename/selection steps to resume. Retained and partial directories
are never automatically deleted. Inspect and back them up before any deliberate
operator cleanup; do not remove directories referenced by an active recovery
journal. A failed committed rebuild may require another explicit request.

## Trust boundaries and limits

The Docker socket grants powerful host control. Use only a trusted local lab.
Fencing assumes a trusted Docker daemon, no external container restarts, and
exclusive use of the managed PostgreSQL startup path. Stop API success, a failed
TCP connection, pause, a missing container, or an unreachable daemon is not
verified isolation. Loss of Docker access blocks automatic failover. Docker
fencing on one host does not provide independent hardware fencing or host-failure
survival.

The Compose network and host-loopback status ports are unauthenticated. Raft and
agent HTTP transport have no TLS or node authentication. The local administrative
socket has OS permissions, not application-level administrative authorization.
The development controller uses PostgreSQL superuser credentials for control
and rewind; ordinary physical replication uses `maat_repl`. Secrets are generated
in ignored `.secrets/` files and copied to PostgreSQL-owned `0600` files under
`/run/maat`; do not commit or print them. The socket's existing group permissions
are used without making it world-accessible.

Dynamic membership, container replacement, multi-host fencing, synchronous
replication, maintenance/drain controls, ranking the most advanced candidate,
production authentication/authorization, and metrics are deferred. Complete
specification section 34 coverage is still required before a production claim.

## Development checks

The [GitHub Actions CI workflow](.github/workflows/maat-ci.yaml) runs on pushes, pull
requests, and manual dispatch. It checks Go formatting, runs tests plus race tests
with the test-only fault hooks, runs golangci-lint, and builds using the Go version in
`go.mod`. Native PostgreSQL lifecycle and Docker Compose fault scenarios remain
explicit local checks; CI does not exercise them.

`make test` runs Go unit tests, Go tests with the race detector and fault hooks,
and the Python deployment-script unit tests.
For significant codebase changes, also run `make integration` and any additional
scenarios affected by the change, as required by [AGENTS.md](AGENTS.md). That
target runs only the primary-failure/rejoin smoke scenario; `--scenario all`
selects all eight ordinary cases, and the deterministic matrix remains separate.

Bare `make` runs `all`: `clean`, `fmt`, `lint`, `build`, `test`, and `integration`.
It removes `bin/` (including retained test configuration and credentials) and
launches a fresh fault-injection lab. Use explicit targets to choose those effects.

```sh
make fmt
make test
make lint
make build
# Explicit native integration: PostgreSQL 18 tools on PATH, non-root OS user,
# and free local ports 16541 and 16542 are required.
MAAT_PG_INTEGRATION=1 go test ./internal/postgres -run TestNativeLifecycle -count=1 -v
```

The native lifecycle test is skipped unless explicitly enabled. `make run` invokes
`go run .` and shows CLI usage with a nonzero exit status without arguments; use
`go run . run --config PATH` only in a correctly configured node environment.
`make clean` removes the entire `bin/` directory, including generated test files; it leaves Docker volumes untouched.

| Path | Contents |
| --- | --- |
| [internal/agent](internal/agent) | Configuration, eligibility, reconciliation, status and local administration |
| [internal/cluster](internal/cluster) | Durable Raft state and transitions |
| [internal/postgres](internal/postgres) | Native lifecycle, replication, rewind and retained-directory rebuild |
| [internal/fencing](internal/fencing) | Replaceable fencing contract and Docker adapter |
| [deploy](deploy) | Node configurations, supervisor, credentials and integration scenarios |
| [docs/validation.md](docs/validation.md) | Recorded checks and specification section 34 gaps |

See [AGENTS.md](AGENTS.md) for repository safety constraints.
