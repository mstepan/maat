# maat

A custom PostgreSQL high-availability controller written in Go, similar in
concept to Patroni, without etcd. One agent runs alongside each PostgreSQL
instance; the agents collectively form a Raft control plane, while PostgreSQL
handles native physical streaming replication.

**Status:** specification and initial Go entry point only. The current program
prints `Maat up and running...`; HA orchestration is not implemented yet.

The [technical specification](<docs/Custom PostgreSQL HA Controller — Technical Specification.md>)
defines the target architecture and safety requirements.

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

## Target architecture

The initial topology is three nodes, allowing Raft to retain a majority after
one node fails:

```text
Node A                 Node B                 Node C
Go agent               Go agent               Go agent
PostgreSQL             PostgreSQL             PostgreSQL
    └──────── agents coordinate through Raft ────────┘
        PostgreSQL primary streams WAL to replicas
```

| Component | Responsibility |
| --- | --- |
| Raft control plane | Leader election, membership, cluster generation, primary authorization, desired topology, and failover coordination |
| Go agent | PostgreSQL observation and control, fencing, replication configuration, and idempotent reconciliation |
| PostgreSQL data plane | WAL generation, streaming, replay, physical replication, timelines, and replication slots |

There is no separate control-plane service. Use an existing Go Raft
implementation; the MVP specifies HashiCorp Raft with durable state.
Raft does not replicate database data, and a Raft leader does not automatically
become the PostgreSQL primary. HA generation and PostgreSQL timeline are
distinct: generation identifies primary authorization, while timeline identifies
database history.

## Planned MVP

- Three Go agents and PostgreSQL instances, with Raft membership and leader election.
- Native asynchronous physical streaming replication and health monitoring.
- A fencing abstraction, verified promotion, and replica upstream reconfiguration.
- Idempotent reconciliation of PostgreSQL against Raft-authorized desired state.
- Old-primary rejoin through `pg_rewind`, or a fresh `pg_basebackup` when rewind
  cannot be used safely.

Asynchronous replication can lose transactions that have not reached the promoted
replica. Lossless failover is not guaranteed. Synchronous replication, maintenance
mode, transport TLS, node authentication, administrative authorization, and
metrics are planned extensions; none are implemented in the current entry point.

## Failover and safety

The planned failover sequence is:

1. Detect primary failure and establish Raft leadership with quorum.
2. Validate the candidate's membership, health, recovery state, freshness,
   timeline, and acceptable WAL/replay position.
3. Fence the old primary and verify it cannot accept writes.
4. Commit the new primary authorization and increment the HA generation in Raft.
5. Promote PostgreSQL and verify it is no longer in recovery.
6. Reconfigure remaining replicas to follow the new primary and verify replication.

Failure detection or a failed TCP connection is insufficient authorization or
fencing. At most one node may be authorized as primary in the current generation;
a new primary must not be authorized while the old primary may still accept
writes. Stale nodes must not self-promote, and quorum loss must not trigger
unilateral promotion.

Transitions must use explicit, recoverable states, such as
`REPLICA → CANDIDATE → FENCING → PROMOTING → PRIMARY`, and survive agent crashes
between fencing, authorization, and promotion. Every PostgreSQL control operation
must be followed by observation and verification.

A returning old primary must be prevented from accepting application writes,
rewound or reinitialized, and verified as a replica before being marked healthy.
Agent failure and PostgreSQL failure require separate handling; losing an agent
alone does not automatically change the database role.

Automatic failover must not be described as production-ready until the failure
scenarios in specification section 34 have been validated, including partitions,
quorum loss, fencing failures, concurrent promotion attempts, and transition
crashes.

## Development

Requires Go 1.27.1 or newer, as declared in `go.mod`. These commands operate on
the current Go program; they do not provision a PostgreSQL cluster.

```sh
make run    # run the application
make build  # build bin/maat
make test   # run tests
make fmt    # format Go files
make vet    # check for common mistakes
make clean  # remove the built binary
```

| Path | Contents |
| --- | --- |
| `main.go` | Initial executable entry point |
| `go.mod` | Go module and toolchain requirement |
| `Makefile` | Build, run, format, test, and vet commands |
| `docs/Custom PostgreSQL HA Controller — Technical Specification.md` | Target design and failure scenarios |
| `AGENTS.md` | Repository guidance for coding agents |

See [AGENTS.md](AGENTS.md) for implementation constraints and validation guidance.
