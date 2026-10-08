## MODIFIED Requirements

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

## ADDED Requirements

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
