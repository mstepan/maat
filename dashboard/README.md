# Maat dashboard

A K9s-style terminal dashboard for existing three-node PostgreSQL 18 Maat labs.
Overview and dedicated node details show agent reachability, database observation
health, observed/authorized roles, receiver/upstream state, sampled byte lag and
age, Raft leader/term, HA generation, timeline/WAL positions, and structured
reconciliation/recovery/transition diagnostics. Monitoring reads Docker metadata
and existing HTTP `/status` endpoints; it performs no HA or lifecycle operations.

## Run

Requires Go 1.27.1+, Docker CLI/Engine with a local Unix-socket context (including
Docker Desktop or Colima), a running Maat Compose lab, and an interactive terminal
at least 80×24. Each agent's `8000/tcp` must have one explicit loopback binding;
dynamic ports are discovered. Remote Docker contexts are unsupported. Nothing is
started automatically; startup reports missing prerequisites. Use fresh labs as
described in the [monorepo README](../README.md).

From the repository root:

```sh
make dashboard-build
./dashboard/bin/maat-dashboard                     # maat-dev
./dashboard/bin/maat-dashboard --project NAME       # another existing lab
make dashboard-run ARGS='--project NAME'            # go run equivalent
```

| Key | Action |
| --- | --- |
| Arrows / `j` / `k` | Select a node; scroll in details/help |
| Enter | Open selected-node details |
| Esc | Return to overview |
| `p` | Hand the terminal to native psql on the selected node |
| `?` | Help and interpretation/SQL limits |
| `q` / Ctrl-C | Quit the dashboard (Ctrl-C belongs to psql during a session) |

Below 80×24 a resize prompt is shown. At 80×24 secondary overview columns are
omitted; all detail fields remain available by scrolling. Meaning does not depend
on color. Unknown and last-known values are explicitly labeled.

## Freshness and sessions

Discovery and independent bounded status requests refresh every two seconds.
HTTP reads time out after two seconds, limit bodies to 1 MiB, and use no proxies
or redirects. Full container identity, labels, status identity, and membership
are validated. Missing/stopped/paused/ambiguous nodes retain their rows; replacement
identities require dashboard restart after resolving the lab's identity policy.
Failure retains a stale snapshot in memory with increasing receipt/sample age;
recovery restores current values and preserves selection.

Lag uses IEC units, exact bytes in details, and primary sample age. Zero is a
measurement; null is unavailable. Primary lag is not applicable. `*`/`STALE`
means the snapshot failed or effective sample age exceeds 30 seconds; `?` means
freshness is unknown. The 30-second display limit is independent of controller
promotion eligibility. Database observation success does not prove replication
readiness. Header values require fresh agreeing views; response counts and
historical quorum confirmations do not establish current Raft quorum.

`p` re-inspects the exact previously validated container, requires it to be
running/unpaused, and executes without a shell:

```text
docker --host LOCAL_SOCKET exec -it --user postgres FULL_CONTAINER_ID \
  psql -X -h /var/lib/maat/control/postgres/socket -U postgres -d postgres
```

These are **unrestricted postgres sessions**, allowing administrative commands
and SQL writes. No SQL runs automatically; the dashboard does not read credential
files or capture SQL transcripts. A previously validated running node remains
available for psql during agent-only failure; identity conflicts block handoff.
There is no primary fallback or automatic restart. `\q` returns to the same node
and screen, including on connection/launch failure. Polling is suspended during
the session and new discovery/status starts on return; earlier poll results are
discarded. Ctrl-C cancels a psql query. Docker detach keys may leave a session
running; abrupt terminal/process loss cannot guarantee child cleanup. Normal
quit and SIGTERM restore terminal state.

## Development boundaries and checks

`maat/dashboard` is an independent Go module with no agent module/internal
imports. The entry point wires:

- `internal/core`: normalized targets/observations, polling, identity binding,
  freshness, view agreement, selection, and session rules.
- `internal/tui`: rendering/navigation and terminal suspension using core state.
- `internal/compose`: discovery/inspection and exact-container native sessions.
- `internal/agenthttp`: bounded HTTP transport and agent wire-schema validation.

A future Kubernetes adapter can implement discovery/session contracts and reuse
the HTTP status reader, core rules and screens. No Kubernetes SDK, registry or
backend flags are included. tview/tcell provide terminal widgets and restoration.

```sh
make dashboard-fmt dashboard-test dashboard-lint dashboard-build
make linux-build dashboard-build
python3 dashboard/deploy/smoke.py
make dashboard-clean
```

The test target runs unit and race tests, with fake adapters, HTTP fixtures and
simulated terminal checks. The smoke creates a fresh isolated Compose project
with dynamic ports, drives a real PTY through native psql/Ctrl-C/return, and
injects agent, PostgreSQL and container failures, stale lag and recovery. Only the
smoke runner injects faults. Cleanup stops its containers and retains volumes and
assets under `dashboard/bin/`; `dashboard-clean` removes that whole directory,
including retained lab assets. Dashboard CI checks its module separately. Existing
root agent commands, including `make integration`, retain their behavior.

This development lab is not production-ready HA. Asynchronous replication may
lose acknowledged transactions; sampled lag cannot measure actual final loss.
TLS, node authentication and administrative authorization remain future work.
See the [validation record](../docs/validation.md) and
[technical specification](<../docs/Custom PostgreSQL HA Controller — Technical Specification.md>).
