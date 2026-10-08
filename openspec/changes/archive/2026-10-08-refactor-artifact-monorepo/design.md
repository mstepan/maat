## Context

The repository has one Go module (`maat`, Go 1.27.1), root CLI sources and
`internal/` packages, plus root build/Compose assets and `deploy/` fault runners.
The Python runners derive their working root from their own file location; Go
CLI tests read deployment fixtures relative to the module root. Moving these
files together preserves those relationships.

Maintainers need a self-contained agent artifact and named places for three
future artifacts. The interview agreed independent modules, root Makefile
forwarding, README-only placeholders, the spelling `helm-deployment`, and fresh
labs without existing-container or volume migration.

## Goals / Non-Goals

**Goals:**

- Establish four top-level artifact directories with explicit ownership.
- Make the agent independently buildable, testable, and runnable from `agent/`.
- Preserve existing root Makefile target names and exit/failure behavior.
- Keep operational documentation accurate after paths move.
- Preserve runtime behavior and verify deployment from fresh storage.

**Non-Goals:**

- Implementing future artifacts, adding their modules, or defining their APIs.
- Exporting agent internals, extracting shared code, or introducing dependencies.
- Renaming the agent executable/module or changing configuration/status contracts.
- Changing HA transitions, quorum rules, fencing, replication, or recovery.
- Release/versioning/CI redesign, a root `go.work`, or existing-lab migration.

## Decisions

### 1. Own runnable agent assets together

Target structure (not all existing files shown):

```text
maat/
  Makefile                 # forwards existing targets to agent
  README.md                # artifact overview and root commands
  AGENTS.md
  LICENSE
  .gitignore
  docs/                    # shared technical specification and validation
  openspec/                # shared proposals and capability specs
  agent/
    README.md              # current agent operations and limitations
    Makefile
    go.mod                 # module maat; existing Go version/dependencies
    go.sum
    main.go
    commands.go
    main_test.go
    internal/
    Dockerfile
    .dockerignore            # excludes secrets/retained assets from build context
    compose.yaml
    deploy/
    bin/                   # generated, ignored
    .secrets/              # generated, ignored
  dashboard/README.md
  k8s-controller/README.md
  helm-deployment/README.md
```

Move all tracked Go sources and tests, module files, deployment fixtures,
supervisor/credential scripts, Compose/Docker assets, and integration/fault
runners together. Retain relative paths inside that group where possible.
The alternative of moving only Go code leaves agent-specific tooling split
across directories and makes later artifact ownership less clear.

### 2. Use independent modules without a workspace yet

Keep `module maat` in `agent/go.mod`, preserving `maat/internal/...` imports,
Go 1.27.1, and dependency versions. Remove the root module by moving its files.
Future Go artifacts receive their own modules when implemented; the Helm
artifact does not require a Go module. Do not add `go.work` for a single module.

A shared root module would couple future dependencies and tests. Separate
modules plus a workspace remain an option later if an actual cross-module
development need appears. No shared package/API boundary is created now.

### 3. Forward root commands; let agent own their implementation

The root Makefile forwards the existing targets (`all`, `fmt`, `test`, `lint`,
`build`, `run`, `clean`, `linux-build`, `fault-build`, `docker-build`,
`compose-up`, `compose-down`, `compose-clean`, and `integration`) using recursive
make in `agent/`. Use normal Make propagation of command-line variable overrides
and child failures. Keep bare `make` selecting `all`, including its existing
clean and integration effects. Do not duplicate recipes at the root.

Builds and integration outputs now live in `agent/bin/`. `make clean` removes
that entire directory, including generated test assets. `make run` retains its
usage output and nonzero exit without CLI/config arguments. Docker Compose is
invoked from `agent/`, preserving its explicit `maat-dev` project name, internal
mount destinations, labels, service names, image reference, and port bindings.
Explicit fault-runner commands documented from the repository root use
`agent/deploy/...` and `agent/bin/...`; direct Go/Compose commands run in `agent/`.

Preserving old root Go/Python/Compose/binary paths would add duplicate compatibility
assets. Root Makefile forwarding is the compatibility surface selected here.

### 4. Document future artifacts without scaffolding

Each future directory contains one README stating that it is unimplemented:

- `dashboard`: terminal UI to monitor PostgreSQL HA and agents in real time.
- `k8s-controller`: Kubernetes controller and CRD kind `PostgresqlCluster`.
- `helm-deployment`: Helm deployment for the future Kubernetes/CRD artifact.

Do not add source code, module files, CRD YAML, RBAC, chart metadata, dependencies,
or empty build/test targets for these artifacts. Polling/streaming, CRD group and
versions, reconciliation ownership, and chart installation content are future
decisions. In particular, this change grants Kubernetes no new HA authority.

### 5. Keep shared documentation and governance at the root

The root README introduces artifacts and their implemented/planned status,
points to agent operations, and explains root commands and fresh-lab expectations.
Move the current detailed operations content to `agent/README.md`, correcting its
links to root docs. Update `AGENTS.md`, `openspec/config.yaml`, and current
documentation references to sources, fixtures, runner commands, and binary paths.
Update CI setup to read `agent/go.mod` and cache dependencies from `agent/go.sum`;
its validation commands continue through the root Makefile.
Keep historical validation dates and results distinguished from newly run checks.
Archived proposals remain historical rather than being rewritten as current paths.
Ensure ignore rules cover agent binaries, generated credentials, and Python cache;
retain protection for any legacy root generated files still present locally.

## Risks / Trade-offs

- Missed relative paths break builds, Docker COPY, fixture loading, or runner
  isolation → move related assets together, search active references, and validate
  both root forwarding and agent-local entry points.
- A wrapper conceals child failure or ignores build overrides → verify failing
  `run` propagation and dry-run a supported variable override through root make.
- Moving generated output could expose credentials → inspect ignore behavior for
  both new and legacy generated locations without printing secret values.
- Container recreation with surviving identity-bound storage is unsafe → target
  fresh labs with fresh database and control volumes together. Do not provide an
  identity-migration shortcut or erase state to bypass checks.
- Deployment path errors could change effective HA behavior → keep algorithms,
  configuration values, and container-internal paths unchanged; run the isolated
  smoke and affected fault-runner scenarios.
- README placeholders might imply delivered functionality → explicitly label all
  three as future work. Preserve the development-only and asynchronous data-loss
  limitations in root and agent documentation.

## Flows and Edge Cases

A maintainer runs `make build` at the root; recursive make enters `agent/` and
builds `agent/bin/maat`. Running `make build` inside `agent/` produces the same
artifact. Root `make test` runs the existing Go, tagged race, and Python suites
inside the actual module/fixture root. A future placeholder contributes no tests
or dependencies to these commands.

Root `make integration` builds the agent Linux binary and starts a fresh isolated
Compose project through `agent/deploy/run_integration.py`. Generated assets stay
under `agent/bin/`. Failure remains nonzero, and default cleanup stops only that
project while retaining its volumes/assets, as today. Moving a script must not
make it read old root configuration or affect an unrelated running lab.

Source moves do not change runtime administrative access, secret handling, or
operator-approved data replacement. Raft remains authoritative for HA state;
PostgreSQL remains authoritative for observed database state. Failover ordering,
idempotency, timeline/generation separation, and recoverable transitions remain
as defined by the technical specification. Root cleanup remains explicitly
destructive only for the existing `compose-clean` operation; the refactor itself
does not automatically delete containers, credentials, or storage.

## Migration Plan

1. Move agent-owned tracked assets together and establish root Make forwarding.
2. Add placeholder READMEs and update current documentation/ignore rules.
3. Validate formatting, tests, lint, build, command forwarding, Compose resolution,
   and fresh isolated integration/fault-runner behavior.
4. Use fresh labs for the reorganized checkout. Existing labs need no migration
   support; any intentional disposal remains a separate explicit cleanup action.

Rollback of source layout is a Git reversal of the refactor. It is not a database
rollback and must not reuse surviving control/database volumes with recreated
container identities. The new generated paths are documented breaking changes.

## Acceptance Criteria

- All current runtime sources/tests and deployment assets belong to `agent/`.
- Agent builds independently with its original module path and dependencies.
- Existing root make targets forward correctly, including failure and overrides.
- Future artifacts contain only accurate README placeholders.
- Documentation links and current commands match the new layout.
- Required checks have observed results; unavailable checks are reported explicitly.
- A fresh smoke run verifies fence/promotion, replica following, and old-primary
  rejoin. Affected fault runners resolve assets from the new location.

## Open Questions

None blocking this layout refactor. Future artifact API design, module names,
release/versioning policy, CRD schema and RBAC, Helm contents, and TUI transport
are intentionally deferred to their own proposals.
