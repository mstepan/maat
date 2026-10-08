## Context

The agent is implemented; `dashboard/` contains only a future-work README. The
three-node lab publishes each agent's `GET /status` on loopback. Normal deployment
uses fixed ports; integration projects publish dynamically assigned ports. Docker
labels identify Compose project, Maat cluster, and node, and Raft membership stores
immutable container IDs. The dashboard will run on the Docker host for local lab
developers with Docker access. No Compose file or database password is needed.

Sources of truth remain unchanged: Raft authorizes topology and HA generation;
PostgreSQL supplies observed database state. Status is a local agent view, not a
linearizable cluster read or a proof of current quorum. The current response lacks
a complete Raft role enum, a separate replication-readiness flag, and a standalone
last-successful-health timestamp. The dashboard must not manufacture these fields.

## Goals / Non-Goals

**Goals:** implement the interview decisions in [proposal.md](proposal.md): local
project selection, two-second refresh, byte lag and sample age, structured node
details, core keyboard controls, and native `psql` handoff. A developer can identify
an unavailable node or lagging replica, inspect its diagnostics, and open the
intended node's database session without leaving the dashboard workflow. Application
logic and UI remain deployment-independent, with infrastructure isolated behind
small contracts so a future Kubernetes adapter can reuse both.

**Non-goals:** remote Docker/SSH, Kubernetes, historical metrics, lag in seconds,
logs, filtering/sorting, project switching within the UI, embedded terminals,
custom SQL roles, lifecycle or HA controls, controller redesign, and production
security/readiness. The approved preview is a design reference with synthetic
data; the shipped terminal UI is not a browser application.

## Decisions

### 1. Independent Go application with one terminal library

Create module `maat/dashboard` under `dashboard/`, with the same Go minimum as
the agent, and executable `maat-dashboard`. Use `tview` and its `tcell` terminal
support for tables, text views, keyboard events, resizing, and terminal suspension.
Pin a compatible released version during implementation. Terminal rendering and
handoff justify a dependency; hand-written terminal control would require more
code. Bubble Tea is an alternative, but adds no required capability for these
simple table/detail screens. No Docker SDK or shared agent package is needed.

The [tview application API](https://github.com/rivo/tview/blob/master/application.go)
supports suspending the terminal UI around a foreground process and queueing
screen updates. Use four small responsibilities, with packages only where they
enforce these requested boundaries:

- Application core: normalized node/status models, freshness, view agreement,
  selection, polling coordination, and session workflow. It owns small discovery,
  status-reader, and session-runner contracts and imports neither terminal nor
  deployment libraries.
- TUI: renders core state and calls application actions; owns terminal suspension
  and input routing but no Docker commands, Compose labels, or Kubernetes logic.
- Compose infrastructure adapter: project/context validation, container discovery,
  endpoint resolution, target revalidation, and Docker psql execution.
- Agent HTTP adapter: validates/decodes the existing status transport and maps its
  wire fields to normalized application observations.

Wire concrete adapters in the executable entry point. A discovered target exposes
node/cluster identity, opaque immutable instance identity, endpoint, generic runtime
availability, and display diagnostics, rather than Docker inspect objects. The
HTTP adapter maps current `container_id` and membership IDs to that generic instance
identity. The core compares identities without interpreting container IDs. Backend
target validation remains in the infrastructure adapter; the TUI requests a session
for the selected normalized target.

A future Kubernetes implementation can supply pod discovery/endpoint/session
adapters while keeping business rules and screens. Do not add a Kubernetes adapter,
SDK, backend selection flag, plugin registry, or assumptions about future pod
authorization/replacement now. Kubernetes-specific policies require their own spec.
Core tests use small in-memory fakes and run without Docker or a real terminal.

### 2. Docker CLI discovery, existing HTTP status

`maat-dashboard` defaults to `maat-dev`; `--project NAME` selects one other lab.
Resolve the local Docker context, list containers including stopped ones by
`com.docker.compose.project`, and inspect their JSON. Match `maat.cluster`,
`maat.node`, and Compose service labels. Require one cluster and one container per
expected node (`instance-a`, `instance-b`, `instance-c`). Keep missing-node rows.
Use current bindings of container port `8000/tcp`, not hard-coded host ports.
Support explicit loopback IPv4/IPv6 bindings; reject non-loopback-only bindings,
missing/ambiguous mappings, mixed clusters, and remote Docker contexts. Docker
Desktop contexts are local even when the engine runs in its managed VM.

Use the [Docker CLI JSON inspection](https://docs.docker.com/reference/cli/docker/inspect/)
through standard-library subprocesses. Discovery needs no working directory or
Compose manifest, which also supports retained integration labs. Rediscover every
refresh so node/container status changes recover without restarting the UI.
Docker failure is displayed separately from endpoint failure; do not retain an
apparently current Docker state after an inspection error.

Poll nodes independently with two-second request timeouts, a 1 MiB response limit,
no redirects or proxy routing, and no overlapping refresh batches. Dispatch
bounded background work so a stalled endpoint cannot block navigation. Cancel
polling on exit and pause new batches during a foreground session. Accept additive
JSON fields but validate required identities, types, and values; decode WAL/lag
numbers without floating-point precision loss. An invalid response is not fresh
status. Sanitize terminal control characters and escape library markup in all
externally supplied text, including error messages.

Validate returned `node_id`, `local.node_id`, `local.cluster_id`,
`local.container_id`, `state.ClusterID`, and membership against inspected labels
and full container ID before accepting a node response. Record the first accepted
identity binding for each node for this dashboard process. A replacement ID or
conflicting membership is an identity error, not a supported replacement workflow.
Do not print raw inspect payloads, container environments, or arbitrary HTTP bodies.

Alternatives considered: repeated `docker exec maat status` would add subprocesses
on every poll; an aggregate agent endpoint/new service would expand agent scope.
The user selected direct status HTTP plus Docker discovery and session launch.

### 3. Focused screens with k9s-style presentation

Use a full-terminal layout: compact context header, bordered content area,
highlighted selected row, and bottom keyboard hints/message line. Follow terminal
colors, with semantic text labels as well as color. The overview shows node ID,
agent reachability, PostgreSQL observation health, observed PostgreSQL role,
receiver state, observed byte lag, and primary sample age. Use IEC byte units;
node details also show the exact byte count.

`Enter` replaces the overview with a dedicated detail screen. Group information
into instance, replication, control plane, and reconciliation/recovery. Show
desired primary versus observed role, leader identity/local leadership boolean,
term, generation, timeline, flush/replay LSNs, receiver/upstream, replay-paused
state, reconciliation error/success, recovery state, and current transition.
Historical quorum confirmation is labeled historical. Unknown/missing fields are
explicit. Do not label a follower versus candidate based on `raft_is_leader=false`.

Arrows/`j`/`k` select nodes, `Enter` inspects, `Esc` returns, `p` opens `psql`, `?`
shows help, and `q` quits the dashboard. Restore the same node when navigating
back; preserve selection by identity across refreshes. Detail content scrolls
without inventing additional modes. Support at least an 80-column by 24-row
terminal, reducing secondary overview columns as needed while retaining node,
agent/PG state, lag, and age. All fields remain available in details. Below that
size, show a resize prompt; exiting still works. Handle ordinary resize and signal
termination without leaving the terminal in raw mode.

Header values summarize only fresh validated responses: show reported generation,
authorized primary, and leader when views agree; otherwise show conflicting or
unknown values and retain per-node details. State how many nodes responded.
Agreement among responding agents is not labeled verified quorum, even if all
three agree. An observed writable/authorized-role mismatch is visible, not repaired.

### 4. Explicit freshness and lag semantics

Store the last accepted response and monotonic receipt time per node. A failed
request, invalid identity/body, stopped/missing container, or missing binding marks
the node unavailable immediately. Retain its accepted snapshot as last known,
label it stale, and advance its age; current PostgreSQL health is unknown. A valid
response resumes current display without changing selection.

Consume `observed_replication_lag_bytes` and
`primary_observation_age_seconds`; do not calculate a new lag across node views.
Null means unavailable, zero is a real measured zero, and the primary shows not
applicable. Effective primary sample age includes elapsed time since receipt.
Numerical lag requires a valid nonnegative sample age and is marked stale when
effective age exceeds 30 seconds or its containing snapshot is stale. This fixed
30-second dashboard display limit matches the current agent default, but is
independent of configurable candidate eligibility and is not a promotion decision.
Malformed/missing age makes freshness unknown; do not display fresh zero lag.

No broad healthy-cluster badge is derived from database observation success alone.
The details distinguish successful SQL observation from receiver/replay evidence.
Display the asynchronous-data-loss limitation in help and diagnostics: a byte
sample and its age cannot establish final primary WAL or actual transaction loss.

### 5. Native `psql`, explicit target, no lifecycle changes

Before handoff, re-inspect the selected full container ID. Require its retained,
validated node/cluster/project identity binding and a running, unpaused container.
Current HTTP unreachability alone does not prevent a session on a previously
validated, still-running node, since agent and PostgreSQL failures differ. A node
never successfully validated, an identity mismatch, or a stopped/missing/paused
container is refused with a clear message. Do not start/recreate/unpause it, or
redirect the session to the primary. If PostgreSQL is stopped inside a running
container, `psql` reports its normal connection failure; the dashboard restores.

Suspend the UI, attach stdin/stdout/stderr to the terminal, and run an argument
array equivalent to:

```text
docker exec -it --user postgres CONTAINER_ID psql -X -h /var/lib/maat/control/postgres/socket -U postgres -d postgres
```

The [Docker exec command](https://docs.docker.com/reference/cli/docker/container/exec/)
provides interactive input, a TTY, and user selection. Use `exec.Command`, never
`sh -c`, and do not interpolate node/project names into shell commands. Do not read
or expose lab passwords; the local PostgreSQL socket uses the lab's existing
local authentication. Do not send SQL automatically. `-X` skips startup files;
the session otherwise remains normal unrestricted `psql`. `Ctrl-C` reaches the
foreground session instead of activating dashboard keys; no session time limit
is imposed. Document Docker detach-key behavior without promising session cleanup
after abrupt terminal/host loss.

On `\q`, normal exit, or launch/connection failure, restore the previous screen
and selected node, re-inspect and refresh before showing any retained measurements
as current, and display a concise nonzero-exit error. Discard pre-handoff poll
results arriving afterward. Dashboard navigation keys do not process session input.
No SQL transcript is captured or logged. Selection removal returns to its missing
row; it does not silently target another container.

### 6. Artifact-specific commands, unchanged agent contracts

Add dashboard Makefile targets `fmt`, `test` (including race), `lint`, `build`,
`run`, and `clean`; output only under `dashboard/bin/`. Add root
`dashboard-fmt`, `dashboard-test`, `dashboard-lint`, `dashboard-build`,
`dashboard-run`, and `dashboard-clean` forwarding targets. Preserve all existing
root targets, including agent-only `clean` and bare `make` behavior. Add a separate
dashboard CI check with its own module/cache paths. Update ignore rules and
implemented/planned documentation when code exists, not merely for this proposal.

## Flows and Acceptance

1. Start the binary on a local Docker host; resolve the requested project and show
   three stable node rows. No matching lab, inaccessible Docker, remote context,
   or noninteractive terminal yields an actionable startup error and clean exit.
2. Refresh status every two seconds. Show current measurements and disagreements;
   retain stale snapshots without freezing healthy-node updates or keyboard input.
3. Select a node and inspect all available diagnostics on its dedicated screen.
4. Press `p`; validate that exact target, hand the terminal to `psql`, and on exit
   return to the same screen/node with a new observation cycle.
5. Quit and release terminal state and outstanding polling work.

The acceptance criteria and failure cases are normative in
[compose-dashboard/spec.md](specs/compose-dashboard/spec.md). Implementation must
exercise current/zero/null/stale lag, dynamic ports, mixed identities, conflicting
views, agent-only failure, PostgreSQL-only failure, stopped/fenced nodes, restored
polling, `psql` errors/exit, and terminal resize/cleanup. Required checks include
agent root checks, dashboard checks, root `make integration`, and a dashboard
smoke run against a fresh isolated lab. The controller's full production failure
matrix remains outside this feature's validation claim.

## Risks / Trade-offs

- Privileged SQL can modify data or invoke PostgreSQL administration → clearly
  identify node/role and document unrestricted lab access; expose no automated HA
  or recovery action, and never claim SQL sessions enforce controller invariants.
- Status is unauthenticated local HTTP → require loopback deployment, validate
  identities and bound payloads; transport authentication remains future work.
- Docker access is powerful → use only inspect/list for monitoring and explicit
  selected-container exec for `psql`; never use fencing or lifecycle operations.
- Stale/asynchronously collected views can disagree → show source/freshness and
  disagreement, not a synthesized authoritative topology or lossless guarantee.
- The response does not expose the agent's configured maximum evidence age →
  document the fixed display threshold as a UI policy, not candidate eligibility.
- Existing data/identity replacement is unsupported → reject replacement bindings;
  no data-directory writes, rebuild, or container migration is introduced.

## Migration Plan

Ship a separately built binary alongside the unchanged lab. No volume, container,
agent schema, or credential migration is required. Roll back by stopping/removing
the dashboard binary. Sessions can have SQL effects; rolling back the application
does not undo operator SQL. Keep the dashboard marked planned until implemented
and validated; leave Kubernetes/Helm as future work.

## Open Questions

No unresolved product decision blocks the MVP. Routine design defaults to review
in this written spec are the `maat-dashboard` name, `tview`, the 30-second display
freshness limit, minimum 80×24 size, and dashboard-prefixed root commands.

Future policy questions deliberately remain unresolved: configurable freshness
matching agent policy, remote/authenticated access, restricted SQL roles, dynamic
membership/container replacement, and cleanup guarantees after detached or
abruptly abandoned SQL sessions. They do not add requirements to this change.
