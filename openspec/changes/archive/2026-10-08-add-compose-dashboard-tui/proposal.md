## Why

Developers currently inspect each Compose node through separate JSON status
requests and Docker commands. A keyboard-driven terminal dashboard will make
instance health, replication lag, node diagnostics, and access to an individual
node's `psql` session available in one place.

## What Changes

- Implement an independent Go artifact in `dashboard/`, producing
  `dashboard/bin/maat-dashboard` without importing the agent's internal packages.
- Separate deployment-independent application logic and UI from infrastructure
  adapters. Compose is the first adapter; later Kubernetes support can reuse
  status interpretation, freshness, navigation, and screen rendering.
- Support the local Docker Compose development lab: default to project
  `maat-dev`, with `--project NAME` selecting another Maat lab.
- Discover project containers and published status ports using Docker; poll the
  existing read-only agent HTTP status endpoints every two seconds.
- Use the approved k9s-inspired **Focused screens** design: context header,
  bordered node table, selected-row highlight, contextual key hints, and a
  dedicated node-detail screen. Adopt the visual style, not k9s functionality.
- Separate agent reachability, PostgreSQL observation health and role,
  authorization, Raft leadership, HA generation, and PostgreSQL timeline.
- Display sampled replication lag in bytes with primary sample age, explicitly
  marking unavailable and stale measurements and retaining stale node snapshots.
- Provide arrows/`j`/`k`, `Enter`, `Esc`, `p`, `?`, and `q` navigation.
- Hand the terminal to native interactive `psql` inside the selected container,
  as OS/database user `postgres` in database `postgres`. Restore the same view
  and selection and refresh status on exit.
- Add dashboard-specific build/check commands and document its implemented scope
  when implementation is complete. Existing root commands keep their agent behavior.

## Capabilities

### New Capabilities

- `compose-dashboard`: deployment-independent dashboard core/UI with a local
  Compose adapter for discovery, identity-bound status polling, honest health/lag
  presentation, keyboard navigation, diagnostics, and native `psql` handoff.

### Modified Capabilities

- `artifact-monorepo`: advance `dashboard/` from a README-only placeholder to an
  independent implemented artifact, while keeping Kubernetes/Helm placeholders
  and preserving existing agent build and runtime contracts.

## Impact

The users are developers operating the existing three-node PostgreSQL 18 lab on
its Docker host, including isolated integration projects. The change affects
`dashboard/`, the root Makefile, `.gitignore`, CI, root/dashboard documentation,
repository guidance, and the implemented-status portions of the technical
specification and OpenSpec context. This proposal itself adds documentation only;
the dashboard is not implemented by creating these artifacts.

Use an established Go terminal UI library for rendering and terminal suspension;
use the standard library for HTTP, JSON, and Docker subprocesses. No new agent
API, Docker SDK, shared library, daemon, PostgreSQL observer, or credential store
is required. No existing command or status schema is intentionally broken.

Non-goals: Kubernetes, remote Docker/SSH, a project picker, logs, filtering,
sorting, command mode, historical graphs, lag in seconds, notifications,
configurable SQL roles, embedded terminal emulation, HA control actions,
container lifecycle operations, and production security or readiness claims.

All eight controller safety invariants continue to apply. The dashboard grants
no primary authority, infers no fencing proof from stopped/unreachable nodes,
and changes no reconciliation, replication, failover, or recovery policy.
Interactive `psql` is intentionally privileged and can change data or bypass
controller workflows; it is an explicit operator session, not an HA operation.
Sampled asynchronous lag cannot prove final WAL position or transaction loss.

The user approved local Compose, unrestricted lab `postgres` sessions, byte lag
plus sample age, structured details without logs, explicit project selection,
native terminal handoff, core navigation, two-second refresh, existing HTTP
status, and the Focused screens preview. They also required deployment-independent
business logic and UI with isolated infrastructure for future Kubernetes support.
Remaining routine design defaults and future policies are identified in `design.md`.
