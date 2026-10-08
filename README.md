# maat

A monorepo for PostgreSQL high availability, with independent artifact modules.
The implemented agent runs beside each PostgreSQL instance and uses HashiCorp
Raft for durable HA authorization and native PostgreSQL physical replication.

**Status: development controller; not production-ready.** Asynchronous failover
can lose acknowledged transactions. The dashboard monitors the local Compose lab;
the Kubernetes and Helm artifacts remain future work.

| Artifact | Status | Purpose |
| --- | --- | --- |
| [agent](agent/README.md) | Implemented | Three-node PostgreSQL 18 HA agent, Docker Compose lab, and fault runners |
| [dashboard](dashboard/README.md) | Implemented | Terminal overview, node details, sampled replication lag, and native selected-node psql |
| [k8s-controller](k8s-controller/README.md) | Future work | Kubernetes controller and `PostgresqlCluster` CRD |
| [helm-deployment](helm-deployment/README.md) | Future work | Helm deployment for the Kubernetes/CRD artifact |

`agent/` has module path `maat` and executable `maat`; `dashboard/` has independent
module path `maat/dashboard` and executable `maat-dashboard`. There is no root Go
module or workspace. The dashboard core and UI use deployment-independent
contracts; Compose discovery/session execution and HTTP status mapping are
isolated adapters. Kubernetes support is future work.

## Development

Requires Go 1.27.1 or newer. The Compose lab additionally requires Docker Engine
with Compose, Python 3, and OpenSSL. Run these commands from the repository root:

```sh
make fmt
make test
make lint
make build
make integration
```

The root Makefile forwards the existing agent targets to `agent/`. The same targets
work directly there, for example `make -C agent build`. Binaries and generated
integration assets live in `agent/bin/`; lab credentials live in ignored
`agent/.secrets/`. `make clean` removes all of `agent/bin/`, including retained
test assets. Bare `make` also cleans and starts an integration fault-injection
lab; use explicit targets to choose those effects.

`make run` shows CLI usage and exits nonzero without arguments. Direct `go`
commands and Docker Compose commands run from `agent/`. From the repository root,
explicit fault-runner paths begin with `agent/deploy/` and binary paths begin with
`agent/bin/`. See [agent operations](agent/README.md) for usage and safety limits.

Dashboard commands from the repository root:

```sh
make dashboard-fmt dashboard-test dashboard-lint dashboard-build
make dashboard-run                        # existing maat-dev lab, interactive terminal
make dashboard-run ARGS='--project NAME'   # another existing local lab
make dashboard-clean                      # removes dashboard/bin/, including smoke assets
```

The dashboard discovers published loopback status ports and refreshes every two
seconds. Use arrows or `j/k`, Enter for details, `p` for unrestricted native
`psql` as postgres, `\q` to return, `?` for help, and `q` to quit. Monitoring only
reads status and Docker metadata. Lag is sampled bytes with age and explicit
stale/unavailable labels; it cannot establish final transaction loss or current
Raft quorum. See [dashboard operations](dashboard/README.md) for requirements,
architecture, controls, and the isolated terminal smoke command.

## Fresh development labs

The reorganized layout targets fresh Compose labs; existing containers and
volumes have no migration workflow. Use fresh database and control storage
together. Never recreate containers against surviving identity-bound volumes.
The source refactor does not automatically remove existing labs or their data.

With no existing `maat-dev` lab or old volumes, `make compose-up` starts the
development cluster. `make compose-down` stops it while retaining storage.
`make compose-clean` permanently removes that lab's containers, volumes, local
images, network, and credentials; use it only when intentionally discarding the
lab. Integration uses a separate fresh project and retains its volumes by default.

The [technical specification](<docs/Custom PostgreSQL HA Controller — Technical Specification.md>)
defines agent architecture and safety. The [validation record](docs/validation.md)
distinguishes observed results from remaining failure scenarios. Shared
[OpenSpec capabilities](openspec/specs) and [repository guidance](AGENTS.md)
apply across the monorepo.

## Why maat?

The name comes from [Maat (Ma'at)](https://en.wikipedia.org/wiki/Maat), the ancient
Egyptian concept of truth, balance, order, and justice. The controller aims to
maintain an orderly PostgreSQL cluster through consensus, reconciliation, and a
single authorized primary.
