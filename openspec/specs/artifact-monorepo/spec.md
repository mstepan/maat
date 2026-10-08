# artifact-monorepo Specification

## Purpose
Define artifact ownership, independent module boundaries, root and agent developer
workflows, future-artifact placeholders, and fresh-lab compatibility expectations.

## Requirements

### Requirement: Explicit artifact ownership
The repository SHALL contain top-level `agent`, `dashboard`, `k8s-controller`,
and `helm-deployment` artifact directories. The agent runtime implementation,
Go tests and module files, Makefile recipes, Dockerfile, Docker build-context
ignore rules, Compose file, deployment fixtures/scripts, and integration/fault
runners SHALL reside in `agent/`. Dashboard source, tests, module, and artifact
Makefile SHALL reside in `dashboard/`. Shared docs, governance, license, and
OpenSpec SHALL remain at the root.

#### Scenario: Locate agent implementation
- **GIVEN** the reorganized checkout
- **WHEN** a maintainer inspects the agent artifact
- **THEN** its source, tests, build and deployment assets are available under `agent/`
- **AND** root governance and the technical specification still apply to the agent

#### Scenario: Locate dashboard implementation
- **WHEN** a maintainer inspects the dashboard artifact
- **THEN** its independently owned source, tests, and build assets are under `dashboard/`

### Requirement: Independent agent module
The agent SHALL build and test as an independent module from `agent/`, retaining
the `maat` module path, Go 1.27.1 minimum, and existing dependency versions. The
dashboard SHALL use its independent `maat/dashboard` module with the same Go
minimum and SHALL not import agent internal packages. The root SHALL contain
neither `go.mod` nor `go.work`. Kubernetes/Helm placeholder modules SHALL NOT be
created, and agent internals SHALL NOT be exported for speculative consumers.

#### Scenario: Build agent without future artifacts
- **GIVEN** the Go toolchain required by the existing module
- **WHEN** a maintainer runs `make build` from `agent/`
- **THEN** the existing `maat` executable is produced at `agent/bin/maat`
- **AND** no dashboard or future-artifact dependency is needed

#### Scenario: Build dashboard independently
- **WHEN** a maintainer runs `make build` from `dashboard/`
- **THEN** `dashboard/bin/maat-dashboard` is built without an agent build or root module/workspace

### Requirement: Root command forwarding
The root Makefile SHALL forward every existing target to the agent Makefile:
`all`, `fmt`, `test`, `lint`, `build`, `run`, `clean`, `linux-build`, `fault-build`,
`docker-build`, `compose-up`, `compose-down`, `compose-clean`, and `integration`.
It SHALL preserve command-line Make variable overrides and nonzero child exit
status. Bare `make` SHALL continue to invoke `all`, with its clean and integration
effects. Outputs SHALL reside under `agent/bin/`, and `clean` SHALL remove that
entire generated directory.

#### Scenario: Root build forwards to agent
- **GIVEN** a checkout with the required Go toolchain
- **WHEN** a maintainer runs `make build` at the repository root
- **THEN** the agent recipe runs in `agent/` and produces `agent/bin/maat`
- **AND** the root has no duplicate build recipe

#### Scenario: Child usage failure is visible
- **GIVEN** no command or configuration arguments
- **WHEN** a maintainer runs root `make run`
- **THEN** the existing CLI usage is displayed
- **AND** root make exits nonzero

#### Scenario: Linux build architecture override propagates
- **GIVEN** a maintainer supplies `MAAT_DOCKER_ARCH=arm64` on the root make command
- **WHEN** `linux-build` is resolved
- **THEN** the agent recipe uses the supplied architecture override

#### Scenario: Clean removes generated agent assets
- **GIVEN** binaries and test assets under `agent/bin/`
- **WHEN** a maintainer runs root `make clean`
- **THEN** the complete `agent/bin/` directory is removed
- **AND** database/control volumes are not deleted by this command

### Requirement: README-only future artifacts
The remaining future artifacts `k8s-controller` and `helm-deployment` SHALL each
contain only a README describing purpose and explicitly identifying future-work
status. `k8s-controller` SHALL describe the controller and CRD kind
`PostgresqlCluster`; `helm-deployment` SHALL describe its Helm deployment.
`dashboard` SHALL be identified as an implemented local Compose TUI only after its
implementation and validation. This change SHALL NOT add Kubernetes/Helm code,
modules, manifests, RBAC, charts, dependencies, or targets.

#### Scenario: Inspect planned capabilities
- **GIVEN** the remaining two future-artifact directories
- **WHEN** a maintainer reads their READMEs
- **THEN** their purposes and unimplemented status are clear
- **AND** only README placeholders are present in those directories

#### Scenario: Distinguish dashboard from planned artifacts
- **WHEN** documentation is updated after dashboard implementation
- **THEN** the dashboard's local Compose capabilities and limits are documented as implemented
- **AND** Kubernetes/Helm and production features remain identified as future work

### Requirement: Fresh-lab deployment with unchanged runtime contracts
The moved deployment SHALL support fresh Compose labs with fresh database and
control storage. Existing-lab migration SHALL NOT be required. The refactor SHALL
retain CLI commands, executable name, JSON configuration and status contracts,
Compose project/service identities and configured ports, internal container paths,
and existing HA safety behavior. It SHALL NOT automatically delete existing labs
or authorize replacement containers against surviving identity-bound volumes.

#### Scenario: Failover and rejoin after the move
- **GIVEN** available Docker/Compose and a fresh isolated three-node integration project
- **WHEN** root `make integration` injects primary failure
- **THEN** existing checks verify fencing before replacement promotion, replica following, and guarded old-primary rejoin
- **AND** generated paths resolve under `agent/` without depending on legacy root assets

#### Scenario: Integration cleanup retains evidence
- **GIVEN** the moved runner has created an isolated project
- **WHEN** the default run completes or fails
- **THEN** cleanup stops only its own project and retains its volumes and generated assets
- **AND** a failed run remains nonzero

#### Scenario: Source reorganization encounters legacy storage
- **GIVEN** containers or volumes from a pre-refactor development lab exist
- **WHEN** the repository is reorganized
- **THEN** no automatic deletion, identity substitution, or control-state erasure occurs
- **AND** documentation directs the new layout toward fresh-lab use

### Requirement: Accurate documentation and ignored generated assets
The root README SHALL map artifacts and distinguish implemented versus planned
work. The agent README SHALL document agent operations. Active documentation,
`AGENTS.md`, and OpenSpec context SHALL use correct current paths and commands.
Agent-generated binaries, credentials, integration assets, and Python caches
SHALL be ignored, with continued protection for locally remaining legacy generated
assets. Historical validation SHALL remain identified as historical.

#### Scenario: Follow current operator instructions
- **GIVEN** the updated current documentation
- **WHEN** a maintainer follows a source link or documented build/deployment command
- **THEN** local links resolve and commands use the intended working directory
- **AND** direct runner/binary paths match `agent/deploy/` and `agent/bin/`

#### Scenario: Generated credentials remain untracked
- **GIVEN** legacy or new generated credential locations
- **WHEN** Git ignore rules are checked
- **THEN** secret files and generated integration assets are excluded from tracking

### Requirement: Evidence before completion
Implementation validation SHALL run `make fmt`, `make test`, `make lint`,
`make build`, and `make integration` from the root and verify agent-local build
and test entry points. It SHALL check shell/Compose path resolution and exercise
affected isolated ordinary and fault-runner entry points. Unavailable checks
SHALL be reported with reasons. Passing checks SHALL NOT imply production-ready
or lossless asynchronous HA.

#### Scenario: Report layout refactor verification
- **GIVEN** the proposed source moves are implemented
- **WHEN** completion is reported
- **THEN** observed required-check results and affected-runner results are recorded
- **AND** blocked/skipped checks and existing production/failure-matrix gaps are explicit

### Requirement: Dashboard-specific developer commands
The dashboard SHALL provide artifact-local `fmt`, `test`, `lint`, `build`, `run`,
and `clean` targets. Root `dashboard-fmt`, `dashboard-test`, `dashboard-lint`,
`dashboard-build`, `dashboard-run`, and `dashboard-clean` SHALL forward to them,
preserving variable overrides and failure status. Existing root agent forwarding,
bare `make`, and agent-only `clean` behavior SHALL remain unchanged. Generated
dashboard binaries SHALL be ignored and reside under `dashboard/bin/`.

#### Scenario: Build and clean the dashboard
- **WHEN** root dashboard-build and dashboard-clean are run in sequence
- **THEN** only dashboard output is built/removed and agent lab assets remain untouched

#### Scenario: Preserve agent entry points
- **WHEN** existing root agent targets are invoked after the dashboard is added
- **THEN** they retain their current agent recipes, outputs, and error propagation

### Requirement: Dashboard verification and truthful documentation
CI SHALL check the dashboard module separately. Dashboard implementation SHALL
run agent root `make fmt`, `make test`, `make lint`, `make build`, and
`make integration`, plus dashboard formatting/test/race/lint/build checks and a
dashboard-specific isolated Compose smoke check. Documentation SHALL distinguish
implemented functionality, synthetic design previews, observed validation, and
future features. Unavailable checks SHALL be reported with reasons.

#### Scenario: Report dashboard validation
- **WHEN** dashboard implementation is reported complete
- **THEN** required check outcomes and dashboard smoke evidence are recorded
- **AND** passing results do not claim exhaustive HA validation or lossless asynchronous failover
