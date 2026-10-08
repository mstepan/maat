## 1. Independent artifact and deployment boundaries

- [x] 1.1 Create the `maat/dashboard` module and `maat-dashboard` entry point under `dashboard/`, using the agent's Go minimum and a pinned compatible tview/tcell release; do not add a root module/workspace or agent dependency.
- [x] 1.2 Define normalized application models and only the discovery, status-reader, and session-runner contracts needed by this feature; keep core and UI free of deployment DTOs/commands.
- [x] 1.3 Wire the core, TUI, Compose adapter, and agent HTTP adapter in the entry point; add fake-adapter checks that exercise core behavior without Docker or a terminal.
- [x] 1.4 Add artifact-local fmt/test/race/lint/build/run/clean recipes, dashboard-prefixed root forwarding, ignored `dashboard/bin/` outputs, and separate module CI checks while preserving existing agent targets and bare make behavior.

## 2. Compose discovery and HTTP observation adapters

- [x] 2.1 Implement default `maat-dev`/`--project NAME`, local Docker-context validation, interactive-terminal validation, and actionable startup errors without starting labs.
- [x] 2.2 Discover stopped/running containers by project, validate Maat/service labels and unique cluster/node bindings, and resolve explicit loopback `8000/tcp` bindings including dynamically assigned ports.
- [x] 2.3 Normalize missing/stopped/paused/ambiguous nodes and retain the fixed three expected rows; cover mixed clusters, duplicate bindings, remote contexts, and non-loopback-only mappings with focused adapter tests.
- [x] 2.4 Implement bounded GET /status reads (two-second timeout, 1 MiB body, no redirects/proxies), precise integer decoding, required-field validation, and mapping from agent wire fields to core observations.
- [x] 2.5 Validate response identity/membership against normalized immutable instance identity; bind accepted nodes and reject replacement/conflicting identities. Cover malformed/oversized/non-200 status and unsafe external text.

## 3. Application status and freshness rules

- [x] 3.1 Coordinate independent two-second background refreshes without overlapping batches or UI blocking; rediscover each cycle and cancel work on exit/session suspension.
- [x] 3.2 Implement last-known stale snapshots, increasing receipt/sample age, recovery after a valid response, and node selection retained by identity; test failure/recovery and delayed responses.
- [x] 3.3 Interpret zero/null/not-applicable lag and invalid/missing age; mark lag stale above the fixed 30-second display limit and when its snapshot is stale. Test boundaries and values beyond floating-point integer precision.
- [x] 3.4 Separate agent reachability, database observation health, receiver/replay evidence, role/authorization, leader/term, and generation/timeline; summarize only agreeing fresh views and expose conflicts without inferring quorum.

## 4. Focused screens terminal UI

- [x] 4.1 Render the approved header, bordered overview table, selection highlight, contextual key hints, and message line using normalized application state only.
- [x] 4.2 Render dedicated structured node details, including generic identity/runtime diagnostics, role/authorization, Raft and HA state, WAL/receiver/upstream-host/lag fields, and reconciliation/recovery/transition errors; label unknown/historical values explicitly.
- [x] 4.3 Implement arrows/j/k, Enter, Esc, p, ?, and q, preserving selected node and view through refreshes; add focused navigation/state checks using a simulated terminal screen.
- [x] 4.4 Handle 80×24 terminals with essential overview columns and scrollable details, resize prompts below minimum, readable non-color labels, safe terminal text, and clean terminal restoration on normal/signal exit.

## 5. Selected-node psql session

- [x] 5.1 Implement Compose-side revalidation of the selected accepted immutable target and running/unpaused state; refuse stopped/missing/unknown/mismatched targets without lifecycle actions, and permit previously validated running nodes during agent-only failure.
- [x] 5.2 Connect generic core session actions to TUI suspension and Compose `docker exec -it --user postgres` with argument-array invocation of native `psql -X`, the existing local socket, and postgres user/database; attach foreground stdio without logging SQL or reading secrets.
- [x] 5.3 Restore the previous screen/selection after normal/nonzero/failed session exit, rediscover/refresh, and discard pre-handoff results. Test exact-target execution, no primary fallback, stopped PostgreSQL, vanished containers, and failed launch.
- [x] 5.4 Verify with a real terminal that Ctrl-C reaches psql, dashboard keys are suspended, \q restores navigation, and terminal state is usable afterward; document detach/abrupt-loss limits.

## 6. Documentation and validation

- [x] 6.1 Update root/dashboard READMEs, AGENTS.md, implemented-status text in the technical specification, and OpenSpec context only after implementation; document local-only infrastructure, reusable core/UI boundaries, all commands, unrestricted SQL, UI freshness limits, and asynchronous-loss/security limits.
- [x] 6.2 Run root `make fmt`, `make test`, `make lint`, and `make build`; run root `make dashboard-fmt`, `make dashboard-test`, `make dashboard-lint`, and `make dashboard-build`, checking dashboard race coverage and agent target preservation.
- [x] 6.3 Run root `make integration` in its isolated project; do not substitute unit tests. Record any unavailable check and its reason, and preserve default retained volumes/assets.
- [x] 6.4 Perform a dashboard-specific smoke check in a fresh isolated Compose lab: dynamic ports, correct project/identity isolation, overview/details, exact-node psql return, agent-only loss, PostgreSQL-only loss, stopped/fenced-node refusal, stale lag, and recovery. Keep dashboard observation separate from fault injection; use existing lab tooling where practical.
- [x] 6.5 Record verification evidence and remaining limits in `docs/validation.md`; verify local links, existing/future command documentation, module/CI paths, and OpenSpec consistency. Do not claim production HA, current-quorum proof, or lossless failover.
