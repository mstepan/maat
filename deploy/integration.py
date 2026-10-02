#!/usr/bin/env python3
"""Explicit destructive fault scenarios for the dedicated maat-dev development lab.

Start and bootstrap the Compose cluster first. This runner never deletes volumes
or recreates containers. SQL markers remain as evidence. Container tests do not
validate physical-host fencing or lossless asynchronous failover.
"""
import argparse
import json
from pathlib import Path
import re
import subprocess
import sys
import time
import urllib.error
import urllib.request
import uuid

ROOT = Path(__file__).resolve().parent.parent
NODES = ("a", "b", "c")
NETWORK = "maat-dev_default"
SOCKET = "/var/lib/maat/control/postgres/socket"
DATA = "/var/lib/maat/postgres/data"
COMPOSE = ["docker", "compose", "--project-name", "maat-dev", "-f", str(ROOT / "compose.yaml")]


class Failure(RuntimeError):
    pass


class NotReady(RuntimeError):
    pass


def require(condition, message):
    if not condition:
        raise Failure(message)


class Lab:
    def __init__(self, timeout):
        self.timeout = timeout
        self.ids = {}
        self.events = []
        self.latest = {}
        self.restore_nodes = set()
        self.disconnected = set()
        self.http = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def event(self, message, **fields):
        entry = {"at": time.time(), "event": message, **fields}
        self.events.append(entry)
        print(json.dumps(entry, sort_keys=True), flush=True)

    def command(self, args, transient=False, timeout=20):
        try:
            result = subprocess.run(args, cwd=ROOT, capture_output=True, text=True, timeout=timeout)
        except (OSError, subprocess.TimeoutExpired) as error:
            raise (NotReady if transient else Failure)("command unavailable or timed out: " + args[0]) from error
        if result.returncode:
            # Docker and PostgreSQL errors can include credentials: retain exit
            # status, never arbitrary command output or daemon response bodies.
            raise (NotReady if transient else Failure)(f"command {args[0]} exited {result.returncode}")
        return result.stdout.strip()

    def inspect(self, node):
        data = json.loads(self.command(["docker", "inspect", self.ids[node]]))[0]
        labels = data.get("Config", {}).get("Labels", {})
        require(data.get("Id") == self.ids[node], "container incarnation changed")
        require(all(labels.get(key) == value for key, value in {
            "maat.cluster": "maat_dev", "maat.node": node,
            "com.docker.compose.project": "maat-dev", "com.docker.compose.service": node,
        }.items()), f"unexpected Docker identity for {node}")
        require(data.get("HostConfig", {}).get("RestartPolicy", {}).get("Name") in ("", "no"),
                "automatic container restart must be disabled")
        return data

    def identity(self, node):
        ids = self.command(["docker", "ps", "--all", "--quiet", "--no-trunc",
                            "--filter", "label=maat.cluster=maat_dev",
                            "--filter", "label=maat.node=" + node]).splitlines()
        require(len(ids) == 1 and re.fullmatch(r"[0-9a-f]{64}", ids[0]),
                f"missing or ambiguous managed container {node}")
        if node in self.ids:
            require(ids[0] == self.ids[node], f"replacement container detected for {node}")
        self.ids[node] = ids[0]
        return self.inspect(node)

    def status(self, node):
        try:
            with self.http.open(f"http://127.0.0.1:{18080 + NODES.index(node)}/status", timeout=2) as response:
                raw = response.read((1 << 20) + 1)
            require(len(raw) <= 1 << 20, "oversized status response")
            status = json.loads(raw)
        except (OSError, urllib.error.URLError, ValueError) as error:
            raise NotReady(f"status unavailable for {node}") from error
        local = status.get("local", {})
        require(status.get("node_id") == node and local.get("cluster_id") == "maat_dev"
                and local.get("container_id") == self.ids[node], f"status identity mismatch for {node}")
        require(status.get("state", {}).get("ClusterID") == "maat_dev", "wrong Raft cluster")
        self.latest[node] = status
        return status

    def sql(self, node, query):
        return self.command(COMPOSE + ["exec", "-T", "--user", "postgres", node,
                            "psql", "-h", SOCKET, "-U", "postgres", "-d", "postgres",
                            "-AtX", "-v", "ON_ERROR_STOP=1", "-c", query], transient=True, timeout=8)

    def role(self, node):
        row = self.sql(node, "SELECT pg_is_in_recovery(), (pg_control_system()).system_identifier::text, "
                       "COALESCE((SELECT sender_host FROM pg_stat_wal_receiver),''), "
                       "COALESCE((SELECT status FROM pg_stat_wal_receiver),'');").split("|")
        require(len(row) == 4 and row[0] in ("t", "f") and row[1].isdigit(), "invalid PostgreSQL identity")
        return {"recovery": row[0] == "t", "system_id": row[1], "upstream": row[2], "receiver": row[3]}

    def poll(self, label, check, timeout=None, interval=1):
        deadline = time.monotonic() + (self.timeout if timeout is None else timeout)
        last = "not ready"
        while True:
            try:
                result = check()
                if result:
                    return result
            except NotReady as error:
                last = str(error)
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise Failure(f"timed out waiting for {label}: {last}")
            time.sleep(min(interval, remaining))

    def stable(self, expected=None):
        def check():
            statuses = {n: self.status(n) for n in NODES}
            authorities = {(s["state"]["Generation"], s["state"]["Primary"]) for s in statuses.values()}
            if len(authorities) != 1:
                raise NotReady("Raft followers have not converged")
            generation, primary = authorities.pop()
            if generation < 1 or primary not in NODES:
                raise NotReady("bootstrap incomplete")
            if expected:
                require((generation, primary) == expected, "unexpected authority change")
            roles = {n: self.role(n) for n in NODES}
            writable = [n for n, role in roles.items() if not role["recovery"]]
            require(len(writable) <= 1, "multiple writable PostgreSQL instances")
            if writable != [primary]:
                raise NotReady("authorized primary not verified writable")
            system_id = roles[primary]["system_id"]
            for node, status in statuses.items():
                local = status["local"]
                db = local.get("database", {})
                if local.get("error") or local.get("recovery_state") or not db.get("healthy"):
                    raise NotReady(f"database observation not healthy on {node}")
                require(roles[node]["system_id"] == system_id, "PostgreSQL system identifiers differ")
                if node != primary and (roles[node]["upstream"] != primary or roles[node]["receiver"] != "streaming"):
                    raise NotReady(f"replication has not converged on {node}")
                transition = status["state"].get("Transition")
                if transition and transition["Phase"] != "complete":
                    raise NotReady("failover transition incomplete")
            return generation, primary
        return self.poll("healthy three-node topology", check)

    def marker(self, primary):
        marker = uuid.uuid4().hex
        self.sql(primary, "CREATE TABLE IF NOT EXISTS public.maat_integration_markers "
                 "(id text PRIMARY KEY, created_at timestamptz NOT NULL DEFAULT now()); "
                 f"INSERT INTO public.maat_integration_markers(id) VALUES ('{marker}');")
        self.verify_marker(marker)
        self.event("marker replay verified", marker=marker)
        return marker

    def verify_marker(self, marker):
        def check():
            for node in NODES:
                if self.sql(node, f"SELECT count(*) FROM public.maat_integration_markers WHERE id='{marker}';") != "1":
                    raise NotReady(f"marker not replayed by {node}")
            return True
        self.poll("marker replay on every node", check)

    def checkpoint(self, primary):
        self.sql(primary, "CHECKPOINT;")
        watermark = self.sql(primary, "SELECT pg_current_wal_flush_lsn();")
        require(re.fullmatch(r"[0-9A-F]+/[0-9A-F]+", watermark), "invalid WAL watermark")
        def check():
            for node in NODES:
                if node != primary and self.sql(node, f"SELECT pg_last_wal_replay_lsn() >= '{watermark}'::pg_lsn;") != "t":
                    raise NotReady("checkpoint WAL not replayed")
            for node in NODES:
                if node != primary:
                    self.sql(node, "CHECKPOINT;")
            statuses = {node: self.status(node) for node in NODES}
            timeline = statuses[primary]["local"]["database"]["timeline"]
            if not all(s["local"]["database"]["timeline"] == timeline for s in statuses.values()):
                raise NotReady("replica restartpoint timeline has not caught up")
            return True
        self.poll("checkpoint and timeline alignment", check)

    def stopped(self, node):
        state = self.inspect(node)["State"]
        require(all(name in state for name in ("Running", "Restarting", "Paused")), "unknown Docker state")
        return not any(state[name] for name in ("Running", "Restarting", "Paused"))

    def history(self, state, old, generation):
        transition = state["Transition"]
        require(transition["OldPrimary"] == old and transition["OldContainerID"] == self.ids[old],
                "transition fenced an unexpected identity")
        events = [event for event in state["History"] if event["TransitionID"] == transition["ID"]]
        kinds = [event["Kind"] for event in events]
        required = ["fencing", "authorize", "promoting", "reconfiguring", "complete"]
        require(all(kind in kinds for kind in required), "transition history is missing safety steps")
        positions = [kinds.index(kind) for kind in required]
        require(positions == sorted(positions) and len(set(positions)) == len(positions), "incorrect failover order")
        authorization = events[kinds.index("authorize")]
        require(authorization["Generation"] == generation + 1, "authorization generation mismatch")
        require(authorization["Transition"]["FenceProof"] == transition["ID"] + ":" + self.ids[old],
                "authorization has no incarnation-bound fencing proof")
        self.event("transition history verified", history=events)

    def await_failover(self, generation, old):
        survivors = [node for node in NODES if node != old]
        def check():
            statuses, roles = {}, {}
            for node in survivors:
                try:
                    statuses[node] = self.status(node)
                    role = self.role(node)
                    roles[node] = role
                except NotReady:
                    continue
                if not role["recovery"]:
                    # Inspect after observing a writer; inspection before the SQL
                    # could race with a legitimate fence followed by promotion.
                    require(self.stopped(old), "replacement writable without observed old-primary isolation")
                    state = statuses[node]["state"]
                    require(state["Generation"] == generation + 1 and state["Primary"] == node,
                            "writable node does not have the expected authorization")
            require(sum(not role["recovery"] for role in roles.values()) <= 1, "multiple replacement writers")
            if len(statuses) != len(survivors) or len(roles) != len(survivors):
                raise NotReady("survivor observations unavailable")
            for status in statuses.values():
                state = status["state"]
                require(state["Generation"] <= generation + 1, "more than one failover generation increment")
                if state["Generation"] != generation + 1 or state["Primary"] == old:
                    raise NotReady("replacement not authorized")
                if not state.get("Transition") or state["Transition"]["Phase"] != "complete":
                    raise NotReady("replacement transition incomplete")
            authorities = {(s["state"]["Generation"], s["state"]["Primary"]) for s in statuses.values()}
            if len(authorities) != 1:
                raise NotReady("survivors disagree on authority")
            require(self.stopped(old), "old primary no longer isolated")
            state = next(iter(statuses.values()))["state"]
            replacement = state["Primary"]
            if roles[replacement]["recovery"]:
                raise NotReady("replacement has not become writable")
            for node, role in roles.items():
                require(role["system_id"] == roles[replacement]["system_id"], "survivor system identifiers differ")
                if node != replacement and (role["upstream"] != replacement or role["receiver"] != "streaming"):
                    raise NotReady("surviving replica has not changed upstream")
            self.history(state, old, generation)
            return generation + 1, replacement
        return self.poll("verified failover", check)

    def start(self, node):
        self.identity(node)
        self.command(COMPOSE + ["start", node])
        self.restore_nodes.discard(node)
        self.event("container started through guarded entrypoint", node=node)

    def restore_network(self, node):
        self.identity(node)
        if NETWORK not in self.inspect(node).get("NetworkSettings", {}).get("Networks", {}):
            self.command(["docker", "network", "connect", "--alias", node, NETWORK, self.ids[node]])
        self.disconnected.discard(node)
        self.event("test network disconnect restored", node=node)

    def scenario(self, name):
        for node in NODES:
            self.identity(node)
        generation, primary = self.stable()
        self.checkpoint(primary)
        marker = self.marker(primary)
        self.event("scenario started", scenario=name, generation=generation, primary=primary)
        if name == "agent-restart":
            before = self.status(primary)
            pid = before.get("agent_pid")
            require(isinstance(pid, int) and pid > 1, "status must expose a valid agent_pid")
            incarnation = before["local"]["incarnation"]
            self.identity(primary)
            self.command(COMPOSE + ["exec", "-T", "--user", "postgres", primary,
                                    "sh", "-c", 'kill -KILL "$1"', "sh", str(pid)])
            def restarted():
                require(not self.role(primary)["recovery"], "agent loss changed PostgreSQL primary role")
                status = self.status(primary)
                if status["state"]["Generation"] == 0:
                    raise NotReady("restarted agent is replaying durable Raft state")
                require((status["state"]["Generation"], status["state"]["Primary"]) == (generation, primary),
                        "agent-only restart changed authority")
                return status["local"]["incarnation"] != incarnation
            self.poll("agent incarnation change with PostgreSQL still writable", restarted)
            expected = (generation, primary)
        elif name in ("replica-process", "replica-container"):
            replica = next(node for node in NODES if node != primary)
            self.identity(replica)
            if name == "replica-container":
                self.restore_nodes.add(replica)
                self.command(["docker", "kill", self.ids[replica]])
                require(self.stopped(replica), "replica container did not stop")
            else:
                self.command(COMPOSE + ["exec", "-T", "--user", "postgres", replica,
                                        "pg_ctl", "-D", DATA, "-m", "immediate", "-w", "stop"])
            # Write while a replica is unavailable and prove authority remains
            # unchanged beyond the configured failure-detection threshold.
            marker = uuid.uuid4().hex
            self.sql(primary, f"INSERT INTO public.maat_integration_markers(id) VALUES ('{marker}');")
            config = json.loads((ROOT / "deploy" / f"{primary}.json").read_text())
            duration = max(5, (config["failure_threshold"] + 2) * config["observation_interval_seconds"])
            deadline = time.monotonic() + duration
            def unchanged():
                status = self.status(primary)
                require((status["state"]["Generation"], status["state"]["Primary"]) == (generation, primary),
                        "replica failure changed primary authorization")
                require(not self.role(primary)["recovery"], "replica failure changed primary role")
                if name == "replica-container":
                    require(self.stopped(replica), "failed replica container restarted without operator action")
                return time.monotonic() >= deadline
            self.poll("unchanged primary during replica failure", unchanged, duration + 20)
            if name == "replica-container":
                self.start(replica)
            expected = (generation, primary)
        elif name == "majority":
            replicas = [node for node in NODES if node != primary]
            for node in replicas:
                self.identity(node)
                self.restore_nodes.add(node)
                self.command(COMPOSE + ["stop", "--timeout", "20", node], timeout=30)
            config = json.loads((ROOT / "deploy" / f"{primary}.json").read_text())
            duration = max(10, (config["failure_threshold"] + 3) * config["observation_interval_seconds"])
            deadline = time.monotonic() + duration
            def unchanged():
                status = self.status(primary)
                require((status["state"]["Generation"], status["state"]["Primary"]) == (generation, primary),
                        "authority changed without majority")
                require(not self.role(primary)["recovery"], "majority loss unexpectedly changed primary role")
                require(all(self.stopped(n) for n in replicas), "test replica resumed unexpectedly")
                return time.monotonic() >= deadline
            self.poll("unchanged authority throughout majority loss", unchanged, duration + 20)
            for node in replicas:
                self.start(node)
            expected = (generation, primary)
        else:
            self.identity(primary)
            if name == "partition":
                network = json.loads(self.command(["docker", "network", "inspect", NETWORK]))[0]
                require(network.get("Name") == NETWORK and network.get("Labels", {}).get("com.docker.compose.project") == "maat-dev",
                        "unexpected network identity")
                require(NETWORK in self.inspect(primary)["NetworkSettings"]["Networks"], "primary is not on managed network")
                self.disconnected.add(primary)
                self.command(["docker", "network", "disconnect", NETWORK, self.ids[primary]])
                require(not self.role(primary)["recovery"], "partition did not leave old PostgreSQL alive")
                self.event("partition injected with old database verified writable", node=primary)
            elif name == "container":
                self.command(["docker", "kill", self.ids[primary]])
                self.event("primary container killed", node=primary)
            else:
                self.command(COMPOSE + ["exec", "-T", "--user", "postgres", primary,
                                        "pg_ctl", "-D", DATA, "-m", "immediate", "-w", "stop"])
                self.event("PostgreSQL process stopped", node=primary)
            if name == "authorized-crash":
                def authorized():
                    for node in NODES:
                        if node == primary:
                            continue
                        status = self.status(node)
                        transition = status["state"].get("Transition")
                        if transition and transition["Phase"] in ("authorized", "promoting"):
                            candidate = transition["Candidate"]
                            if self.role(candidate)["recovery"]:
                                return candidate
                    return None
                candidate = self.poll("authorized candidate before promotion", authorized, interval=0.05)
                self.command(["docker", "kill", self.ids[candidate]])
                require(self.stopped(primary) and self.stopped(candidate), "crash did not remove quorum")
                self.start(primary)
                def waiting():
                    status = self.status(primary)
                    if status["state"]["Generation"] != generation + 1 or not status.get("raft_leader"):
                        raise NotReady("returned agent is waiting for Raft replay and election")
                    require(not status["local"]["database"]["healthy"], "old primary restarted during unfinished promotion")
                    return True
                self.poll("old primary remains stopped under pending authorization", waiting)
                self.start(candidate)
                self.event("authorized candidate returned after crash and leader change", node=candidate)
            expected = self.await_failover(generation, primary)
            if primary in self.disconnected:
                self.restore_network(primary)
            self.start(primary)
        self.stable(expected)
        self.verify_marker(marker)
        self.checkpoint(expected[1])
        self.event("scenario passed", scenario=name, generation=expected[0], primary=expected[1])

    def cleanup(self):
        # Restore only faults injected here. A fenced old primary remains stopped
        # on failure unless the normal success path verified transition completion.
        errors = []
        for node in sorted(self.disconnected.copy()):
            try:
                self.restore_network(node)
            except Exception as error:
                errors.append(f"network {node}: {error}")
        for node in sorted(self.restore_nodes.copy()):
            try:
                self.start(node)
            except Exception as error:
                errors.append(f"replica {node}: {error}")
        return errors

    def evidence(self):
        statuses = {}
        for node in self.ids:
            try:
                statuses[node] = self.status(node)
            except (Failure, NotReady, ValueError, KeyError) as error:
                statuses[node] = {"unavailable": str(error), "last_observed": self.latest.get(node)}
        return {"container_ids": self.ids, "statuses": statuses, "events": self.events}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--scenario", choices=("smoke", "agent-restart", "majority", "partition", "container", "authorized-crash", "replica-process", "replica-container", "all"), default="smoke")
    parser.add_argument("--timeout", type=int, default=120, help="maximum seconds per convergence phase")
    args = parser.parse_args()
    if args.timeout <= 0:
        parser.error("--timeout must be positive")
    lab = Lab(args.timeout)
    failed = False
    try:
        scenarios = ("smoke", "agent-restart", "majority", "partition", "container", "authorized-crash", "replica-process", "replica-container") if args.scenario == "all" else (args.scenario,)
        for scenario in scenarios:
            lab.scenario(scenario)
    except (Failure, NotReady, KeyError, ValueError, KeyboardInterrupt) as error:
        failed = True
        lab.event("integration failed", reason=str(error))
        print(json.dumps(lab.evidence(), indent=2, sort_keys=True), file=sys.stderr)
    finally:
        errors = lab.cleanup()
        if errors:
            failed = True
            print(json.dumps({"cleanup_errors": errors}), file=sys.stderr)
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
