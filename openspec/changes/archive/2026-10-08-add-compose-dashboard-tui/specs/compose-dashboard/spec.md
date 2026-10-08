## ADDED Requirements

### Requirement: Deployment-independent core and UI
Dashboard business logic SHALL use normalized models and small infrastructure
contracts for discovery, status reading, and session execution. Freshness, state
interpretation, selection, and workflows SHALL not depend on Docker/Compose or
Kubernetes types/commands. The TUI SHALL render application state and invoke its
actions without containing infrastructure logic. Current Compose discovery and
session execution, and agent HTTP schema mapping, SHALL be isolated adapters wired
at the application entry point. No Kubernetes implementation SHALL be required
for this change.

#### Scenario: Test without deployment infrastructure
- **GIVEN** fake discovery, observation, and session adapters
- **WHEN** core freshness, view agreement, selection, and session workflows are tested
- **THEN** they run without Docker, Kubernetes, network access, or terminal rendering

#### Scenario: Prepare a future infrastructure adapter
- **WHEN** a maintainer inspects the application and rendering boundaries
- **THEN** Compose labels/commands/inspect payloads occur only in infrastructure wiring/adapters
- **AND** a future adapter can supply normalized targets and observations and implement session execution without replacing existing business rules or screens

### Requirement: Local Compose project selection
The dashboard SHALL run as `maat-dashboard` on the local Docker host, default to
Compose project `maat-dev`, and accept `--project NAME`. It SHALL require an
interactive terminal and a reachable local Docker context. It SHALL not start
containers or create a lab as a side effect of startup.

#### Scenario: Open the default lab
- **GIVEN** the existing local `maat-dev` lab
- **WHEN** the operator starts `maat-dashboard` without a project flag
- **THEN** only that project's Maat nodes are shown

#### Scenario: Open an isolated lab
- **GIVEN** a local Maat integration lab and a separate `maat-dev` lab
- **WHEN** the operator supplies the integration project's name
- **THEN** its nodes and published status ports are used, without reading the other lab

#### Scenario: Unavailable startup prerequisites
- **WHEN** Docker is inaccessible, the context is remote, no matching lab exists, or the terminal is noninteractive
- **THEN** startup reports the specific prerequisite failure and exits cleanly without modifying a lab

### Requirement: Identity-bound Docker discovery
The dashboard SHALL discover running and stopped project containers, match Compose
project/service and Maat cluster/node labels, require one cluster and unique node
bindings, and resolve loopback mappings of `8000/tcp`. It SHALL maintain rows for
the fixed three expected nodes. It SHALL validate status identities and membership
against full inspected container IDs before accepting a snapshot. Missing nodes,
non-loopback-only or ambiguous ports, duplicate bindings, mixed clusters, and
replacement identities SHALL be explicit errors, never silent substitutions.

#### Scenario: Dynamic published ports
- **GIVEN** each integration node has an automatically assigned loopback status port
- **WHEN** discovery inspects that project
- **THEN** status requests use those mappings without hard-coded development ports

#### Scenario: Stopped or missing member
- **GIVEN** an expected node is stopped or missing while the others run
- **WHEN** discovery refreshes
- **THEN** its node row remains with stopped/missing state and the other rows continue updating

#### Scenario: Identity mismatch or replacement
- **GIVEN** labels, returned status, membership, or the retained binding disagree about container/node/cluster identity
- **WHEN** discovery or polling observes that mismatch
- **THEN** the response is rejected and psql is blocked for that ambiguous target

### Requirement: Bounded independent status refresh
The dashboard SHALL poll existing read-only `GET /status` endpoints every two
seconds, independently for each node, with two-second request timeouts, at most
1 MiB per response, and no overlapping batches. Polling SHALL not block keyboard
handling. Invalid responses SHALL not count as fresh observations. WAL/lag values
SHALL retain integer precision. No redirects/proxy routing SHALL change the
discovered endpoint target.

#### Scenario: One slow endpoint
- **GIVEN** one node's endpoint hangs and two respond
- **WHEN** a refresh runs
- **THEN** the responding nodes update while navigation remains responsive
- **AND** the hanging request times out and no overlapping poll batch is created

#### Scenario: Malformed or oversized response
- **WHEN** an endpoint returns invalid required fields, non-200 status, or more than 1 MiB
- **THEN** it is marked unavailable without accepting that body or exposing it as an error dump

### Requirement: Honest instance and cluster presentation
The overview SHALL separately show node identity, agent reachability, PostgreSQL
observation health, observed role, replication receiver state, byte lag, and sample
age. Details SHALL distinguish authorization from observed role, Raft leader/term
from PostgreSQL role, and HA generation from timeline. Cluster header values SHALL
derive only from fresh validated agreeing responses and display conflicts/unknowns
and response count otherwise. Historical quorum confirmation SHALL be labeled
historical; leader status or agreeing snapshots SHALL not be presented as verified
current quorum. Database observation success SHALL not imply replication readiness.

#### Scenario: Agent failure while PostgreSQL might still run
- **GIVEN** a formerly healthy node's status endpoint becomes unreachable
- **WHEN** its row is rendered
- **THEN** the agent is unreachable and current database health is unknown
- **AND** the dashboard does not infer a stopped database, fencing, or a new primary

#### Scenario: PostgreSQL failure with a responding agent
- **GIVEN** a valid status reports `local.database.healthy=false`
- **WHEN** it is displayed
- **THEN** agent reachability and unhealthy PostgreSQL observation are shown separately
- **AND** zero/default role fields are not represented as a known writable primary

#### Scenario: Leadership change or authorization mismatch
- **GIVEN** fresh agent views disagree about leader, generation, or primary, or an observed role differs from authorization
- **WHEN** overview or details are displayed
- **THEN** the disagreement/mismatch is visible and no reconciled authority is invented

### Requirement: Sampled byte lag and explicit freshness
The dashboard SHALL consume the agent's observed lag in bytes and primary sample
age, display human-readable IEC units and exact bytes in details, and advance age
between observations. Null/unavailable lag SHALL differ from measured zero; primary
lag SHALL be not applicable. Numeric lag SHALL require a valid sample age, be
marked stale when its effective age exceeds 30 seconds, and be stale whenever its
containing snapshot is stale. This UI limit SHALL not be described as controller
promotion eligibility or evidence of actual transaction loss.

#### Scenario: Zero versus unavailable
- **WHEN** one replica reports zero lag with valid fresh age and another reports null lag
- **THEN** the first shows measured zero and the second shows unavailable, not zero

#### Scenario: Old primary sample with a reachable agent
- **GIVEN** current valid status reports numerical lag with primary sample age over 30 seconds
- **WHEN** the replica row is rendered
- **THEN** its agent can remain reachable while its lag is explicitly stale

#### Scenario: Missing freshness evidence
- **WHEN** numerical lag has missing, invalid, or negative sample age
- **THEN** freshness is unknown and the dashboard does not display it as a fresh measurement

### Requirement: Retained stale snapshots and recovery
A failed refresh SHALL immediately label the node unavailable and its last accepted
snapshot stale. Its age SHALL continue increasing. Retained PostgreSQL role and
measurements SHALL be labeled last known, not current. A later validated response
SHALL restore current display without changing the selected node. The dashboard
SHALL not persist stale status as authority across restarts.

#### Scenario: Failure and recovery
- **GIVEN** a selected replica has an accepted snapshot
- **WHEN** status fails and later recovers with a valid response
- **THEN** failure retains a labeled stale snapshot and recovery restores fresh values
- **AND** that node remains selected throughout

#### Scenario: Dashboard restart with an unavailable node
- **WHEN** the dashboard restarts and cannot obtain that node's status
- **THEN** its values are unknown until a valid response arrives, without invented cached authority

### Requirement: Focused keyboard-driven screens
The dashboard SHALL use the approved Focused screens layout: context header,
bordered node table or dedicated detail screen, selected-row highlight, and
contextual keyboard hints/message line. Arrows/`j`/`k` SHALL select, `Enter` SHALL
open details, `Esc` SHALL return, `p` SHALL open psql, `?` SHALL show help, and `q`
SHALL quit. Selection SHALL survive refreshes and navigation by node identity.
Meaning SHALL remain readable without color. Terminal resize and normal termination
SHALL preserve terminal usability; all diagnostics SHALL be accessible at 80×24,
with scrolling and secondary-column reduction as needed.

#### Scenario: Inspect and return
- **GIVEN** instance-b is selected
- **WHEN** Enter opens details, a refresh occurs, and Esc returns
- **THEN** instance-b remains selected and both screens expose the current node snapshot

#### Scenario: Small terminal and shutdown
- **WHEN** the terminal is resized to 80×24 and then below the minimum, or the dashboard exits normally
- **THEN** the supported size retains essential status and scrollable details, below minimum shows a resize prompt, and exit restores terminal state

### Requirement: Structured per-node diagnostics
Details SHALL show available node/container identity and Docker state, agent and
database observation status, observed/authorized roles, reported leader/local
leadership boolean and term, generation, primary, timeline, flush/replay LSNs,
receiver/upstream/replay-paused state, lag/age, reconciliation error/success,
recovery state, and current transition. Missing fields SHALL be explicit; full
Raft roles, health timestamps, and replication-readiness guarantees SHALL not be
fabricated. Logs and historical graphs SHALL not be required.

#### Scenario: Inspect a transition or failed recovery
- **GIVEN** valid status contains a transition phase, recovery state, or reconciliation error
- **WHEN** that node's detail screen is opened
- **THEN** those fields are visible alongside observed database state and authorization

### Requirement: Native selected-node psql handoff
Pressing `p` SHALL re-inspect the selected immutable target, require its previously
validated binding and running/unpaused state, suspend the dashboard, and invoke
interactive `psql` through `docker exec -it --user postgres`. The session SHALL use
database user/database `postgres`, `-X`, and the existing local PostgreSQL socket.
Arguments SHALL be passed without a shell. No SQL SHALL run automatically and no
credential file SHALL be read for this session. Foreground input and Ctrl-C SHALL
belong to psql; dashboard keys SHALL not process SQL session input.

#### Scenario: Session on the selected replica
- **GIVEN** instance-b is selected and its container binding is validated and running
- **WHEN** the operator presses p
- **THEN** native psql opens inside instance-b as postgres without redirecting to the primary

#### Scenario: Agent down with a known running target
- **GIVEN** the node was validated earlier but its agent is now unreachable
- **WHEN** re-inspection confirms the same running unpaused container and p is pressed
- **THEN** psql handoff remains available with stale status identified as such

#### Scenario: Stopped, unknown, or ambiguous target
- **WHEN** p is pressed for a stopped/missing/paused container, an unvalidated binding, or an identity mismatch
- **THEN** handoff is refused with a specific message
- **AND** no container is started, recreated, unpaused, or replaced

### Requirement: Session return and failure recovery
Normal psql exit, connection/launch failure, and nonzero child exit SHALL restore
the previous dashboard screen and selected node, show a concise error when needed,
and trigger discovery/status refresh. Retained values SHALL remain stale until a
new valid observation; pre-handoff poll results SHALL not overwrite post-session
state. The dashboard SHALL not capture/log SQL transcripts or silently move the
session to another node.

#### Scenario: Quit psql from details
- **GIVEN** psql was launched from instance-b's details
- **WHEN** the operator exits with `\q`
- **THEN** the same screen and node return and a new refresh begins

#### Scenario: Container or PostgreSQL disappears during handoff
- **WHEN** psql cannot launch/connect or its container exits during the session
- **THEN** the dashboard restores its terminal UI, shows the failed session, and refreshes that exact node
- **AND** it neither retries against another container nor starts PostgreSQL

### Requirement: Read-only monitoring and explicit limitations
Monitoring SHALL perform only Docker discovery/inspection and existing read-only
status requests. It SHALL not invoke fencing, promotion, recovery, Raft writes, or
container/data lifecycle operations. Help and documentation SHALL explain that
manual postgres sessions are privileged, asynchronous replication can lose
transactions, sampled lag cannot measure actual final loss, and production
authentication/security/readiness remain future work. External status text SHALL
be rendered without terminal-control or UI-markup injection.

#### Scenario: Majority loss or fenced old primary
- **GIVEN** the control plane loses majority or reports a stopped old primary during failover
- **WHEN** the dashboard observes this condition
- **THEN** it only displays reported state and freshness
- **AND** it neither grants authority nor starts the old writer

#### Scenario: External error contains control markup
- **WHEN** status/labels/errors contain terminal escapes or renderer markup
- **THEN** they appear as safe readable text and cannot issue terminal commands or alter the UI structure
