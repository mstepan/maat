#!/usr/bin/env python3
"""Deterministic real-agent SIGKILL matrix in fresh, isolated Compose projects.

Build a test binary with -tags maat_faults and pass --binary. Ordinary cleanup
stops containers and retains volumes. --remove-test-volumes explicitly deletes
only this invocation's newly created disposable projects after evidence capture.
"""
import argparse
import json
from pathlib import Path
import secrets
import sys
import tempfile
import time
import urllib.error
import uuid

from integration import DATA, NODES, ROOT, SOCKET, Failure, Lab, NotReady, require

CONTROL = "/var/lib/maat/control"
IMAGE = "postgres:18-bookworm@sha256:3725f4e2499eef5134592b3b4ab79a543ed7f8e533b05b5b637af926630f6650"
POINTS = tuple(point for kind in ("begin", "fencing", "authorize", "promoting", "reconfiguring", "complete")
               for point in ("before-" + kind, "after-" + kind)) + (
                   "before-fence", "after-fence", "after-fence-verification", "before-promote", "after-promote")
PREAUTH = {"before-begin", "after-begin", "before-fencing", "after-fencing", "before-fence",
           "after-fence", "after-fence-verification", "before-authorize"}


class CrashLab(Lab):
    def __init__(self, timeout, directory, binary):
        super().__init__(timeout)
        self.directory = Path(directory)
        self.directory.mkdir(parents=True, exist_ok=True)
        self.project = "maat-crash-" + uuid.uuid4().hex[:12]
        self.cluster = self.project.replace("-", "_")
        self.compose = ["docker", "compose", "--project-name", self.project, "-f", str(self.directory / "compose.json")]
        self.ports = {}
        services, volumes = {}, {}
        for name in ("postgres-password", "replication-password"):
            path = self.directory / name
            path.write_text(secrets.token_hex(24))
            path.chmod(0o600)
        for node in NODES:
            config = json.loads((ROOT / "deploy" / (node + ".json")).read_text())
            config["cluster_id"] = self.cluster
            (self.directory / (node + ".json")).write_text(json.dumps(config))
            volumes.update({node + "-data": {}, node + "-control": {}})
            services[node] = {
                "image": IMAGE, "hostname": node, "init": True, "restart": "no",
                "entrypoint": ["/usr/local/bin/maat-entrypoint"], "stop_grace_period": "20s",
                "labels": {"maat.cluster": self.cluster, "maat.node": node},
                "ports": ["127.0.0.1::8000"],
                "secrets": ["postgres-password", "replication-password"],
                "volumes": [node + "-data:/var/lib/maat/postgres", node + "-control:/var/lib/maat/control",
                            str(self.directory / (node + ".json")) + ":/etc/maat/config.json:ro",
                            str(Path(binary).resolve()) + ":/usr/local/bin/maat:ro",
                            str(ROOT / "deploy" / "entrypoint.sh") + ":/usr/local/bin/maat-entrypoint:ro",
                            "/var/run/docker.sock:/var/run/docker.sock"],
            }
        document = {"services": services, "volumes": volumes,
                    "secrets": {name: {"file": str(self.directory / name)} for name in ("postgres-password", "replication-password")}}
        (self.directory / "compose.json").write_text(json.dumps(document))

    def create(self):
        self.command(self.compose + ["up", "-d"], timeout=90)
        for node in NODES:
            self.ids[node] = self.command(self.compose + ["ps", "--all", "--quiet", node])
            self.inspect(node)
            self.ports[node] = int(self.command(self.compose + ["port", node, "8000"]).rsplit(":", 1)[1])
        self.event("isolated cluster created", project=self.project)

    def inspect(self, node):
        data = json.loads(self.command(["docker", "inspect", self.ids[node]]))[0]
        labels = data["Config"]["Labels"]
        require(data["Id"] == self.ids[node] and labels.get("maat.cluster") == self.cluster
                and labels.get("maat.node") == node and labels.get("com.docker.compose.project") == self.project,
                "disposable container identity mismatch")
        require(data["HostConfig"]["RestartPolicy"]["Name"] in ("no", ""), "unsafe restart policy")
        return data

    def identity(self, node):
        return self.inspect(node)

    def status(self, node):
        try:
            with self.http.open(f"http://127.0.0.1:{self.ports[node]}/status", timeout=2) as response:
                status = json.load(response)
        except (OSError, urllib.error.URLError, ValueError) as error:
            raise NotReady(f"status unavailable for {node}") from error
        require(status["node_id"] == node and status["local"]["cluster_id"] == self.cluster
                and status["local"]["container_id"] == self.ids[node], "unexpected status identity")
        self.latest[node] = status
        return status

    def execute(self, node, args, transient=False):
        return self.command(self.compose + ["exec", "-T", "--user", "postgres", node] + args, transient=transient)

    def sql(self, node, query):
        return self.execute(node, ["psql", "-h", SOCKET, "-U", "postgres", "-d", "postgres",
                                   "-AtX", "-v", "ON_ERROR_STOP=1", "-c", query], transient=True)

    def start(self, node):
        self.inspect(node)
        self.command(self.compose + ["start", node])
        self.ports[node] = int(self.command(self.compose + ["port", node, "8000"]).rsplit(":", 1)[1])

    def arm(self, node, point):
        require(point in POINTS, "unknown fault point")
        self.execute(node, ["sh", "-c", 'rm -f "$1/fault-reached" "$1/fault-release"; printf %s "$2" > "$1/fault-arm"',
                            "sh", CONTROL, point])

    def reached(self, node):
        try:
            return json.loads(self.execute(node, ["cat", CONTROL + "/fault-reached"], transient=True))
        except (NotReady, ValueError):
            return None

    def release(self, node):
        self.execute(node, ["touch", CONTROL + "/fault-release"])

    def signal(self, node, signal, pid):
        require(signal in ("STOP", "CONT", "KILL") and isinstance(pid, int) and pid > 1, "invalid agent signal")
        self.execute(node, ["sh", "-c", 'kill "-$1" "$2"', "sh", signal, str(pid)])

    def stop(self, remove_volumes=False):
        self.command(self.compose + ["stop", "--timeout", "20"], timeout=75)
        if remove_volumes:
            for node in self.ids:
                self.inspect(node)
            self.command(self.compose + ["down", "--volumes"], timeout=60)

    def assert_boundary(self, point, candidate, generation, old):
        status = self.status(candidate)
        state = status["state"]
        preauth = point in PREAUTH
        expected_phase = {
            "before-begin": None, "after-begin": "validating", "before-fencing": "validating",
            "after-fencing": "fencing", "before-authorize": "fencing", "after-authorize": "authorized",
            "before-promoting": "authorized", "after-promoting": "promoting",
            "before-reconfiguring": "promoting", "after-reconfiguring": "reconfiguring",
            "before-complete": "reconfiguring", "after-complete": "complete",
            "before-fence": "fencing", "after-fence": "fencing", "after-fence-verification": "fencing",
            "before-promote": "promoting", "after-promote": "promoting",
        }[point]
        require((state.get("Transition") or {}).get("Phase") == expected_phase, "incorrect committed boundary phase")
        require(state["Generation"] == generation + (0 if preauth else 1), "incorrect boundary generation")
        require(state["Primary"] == (old if preauth else candidate), "incorrect boundary authorization")
        promoted = point in {"after-promote", "before-reconfiguring", "after-reconfiguring", "before-complete", "after-complete"}
        require(self.role(candidate)["recovery"] != promoted, "incorrect boundary SQL role")
        if point not in {"before-begin", "after-begin", "before-fencing", "after-fencing", "before-fence"}:
            require(self.stopped(old), "old primary not isolated at fenced boundary")
        self.event("exact boundary verified", point=point, candidate=candidate, generation=state["Generation"],
                   phase=(state.get("Transition") or {}).get("Phase"), sql_recovery=not promoted,
                   incarnation=status["local"]["incarnation"])
        return status

    def crash_case(self, point, mode):
        generation, old = self.stable()
        self.checkpoint(old)
        marker = self.marker(old)
        # Allow each receiving agent to cache the primary evidence before failure.
        self.poll("fresh primary samples", lambda: all(self.status(n)["primary_observation_age_seconds"] is not None for n in NODES))
        for node in NODES:
            if node != old:
                self.arm(node, point)
        self.execute(old, ["pg_ctl", "-D", DATA, "-m", "immediate", "-w", "stop"])
        def arrived():
            for node in NODES:
                if node != old:
                    hit = self.reached(node)
                    if hit:
                        return node, hit
            return None
        candidate, hit = self.poll("exact fault handshake " + point, arrived, interval=0.1)
        before = self.assert_boundary(point, candidate, generation, old)
        survivor = next(n for n in NODES if n not in (old, candidate))
        # Disarm the other node: only the observed leader's boundary is tested.
        if point != "before-begin":
            self.execute(survivor, ["rm", "-f", CONTROL + "/fault-arm"])
        paused = []
        if mode == "quorum":
            for node in (old, survivor):
                if not self.stopped(node):
                    pid = self.status(node)["agent_pid"]
                    self.signal(node, "STOP", pid)
                    paused.append((node, pid))
        elif mode == "leadership":
            self.signal(candidate, "STOP", hit["pid"])
            old_was_fenced = self.stopped(old)
            if old_was_fenced:
                self.start(old)
            def changed():
                for node in (old, survivor):
                    status = self.status(node)
                    if status.get("raft_is_leader") and status["state"]["Generation"] != 0:
                        require(status["state"]["Generation"] == before["state"]["Generation"], "new leader changed authorization")
                        return node
                return None
            leader = self.poll("different elected leader while candidate stopped", changed)
            if old_was_fenced and (before["state"].get("Transition") or {}).get("Phase") not in ("reconfiguring", "complete"):
                require(not self.status(old)["local"]["database"]["healthy"], "old primary restarted before promotion verification")
            self.event("leadership changed at boundary", point=point, leader=leader)
            require(self.role(candidate)["recovery"] == self.role_before(point), "agent pause changed database role")
        self.signal(candidate, "KILL", hit["pid"])
        def restarted():
            status = self.status(candidate)
            if mode != "quorum" and status["state"]["Generation"] == 0:
                return False
            return status if status["local"]["incarnation"] != before["local"]["incarnation"] else False
        after = self.poll("new agent incarnation from durable state", restarted)
        if mode == "quorum":
            deadline = time.monotonic() + 6
            while time.monotonic() < deadline:
                state = self.status(candidate)["state"]
                require(state["Generation"] in (0, before["state"]["Generation"]), "authority advanced without quorum")
                if state["Generation"]:
                    require((state.get("Transition") or {}).get("Phase") == (before["state"].get("Transition") or {}).get("Phase"), "transition advanced without quorum")
                require(self.role(candidate)["recovery"] == self.role_before(point), "database promoted without quorum")
                time.sleep(0.25)
            self.event("no-quorum hold verified", point=point, seconds=6)
            for node, pid in paused:
                self.signal(node, "CONT", pid)
            self.poll("durable authority applied after quorum restoration",
                      lambda: self.status(candidate)["state"]["Generation"] == before["state"]["Generation"])
        if point in PREAUTH:
            # The primary is unhealthy or fenced and no new primary sample can
            # refill the restarted candidate's intentionally empty evidence cache.
            def blocked():
                status = self.status(candidate)
                require(status["state"]["Generation"] == generation and status["state"]["Primary"] == old,
                        "restart fabricated new primary authority")
                require(self.role(candidate)["recovery"] and self.role(survivor)["recovery"], "preauthorization restart promoted PostgreSQL")
                require(status["primary_observation_age_seconds"] is None, "restart retained primary observation cache")
                return status if status.get("raft_leader") and status.get("reconciliation_error") and (any(word in status["reconciliation_error"] for word in ("stale", "missing")) or (point == "before-begin" and "not freshly verified writable" in status["reconciliation_error"])) else None
            blocked_status = self.poll("explicit missing-evidence fail-closed result", blocked)
            self.event("restart blocked on absent primary evidence", reason=blocked_status["reconciliation_error"])
        else:
            if mode == "leadership" and point in {"after-reconfiguring", "before-complete", "after-complete"}:
                expected = self.stable((generation + 1, candidate))
                self.history(self.status(candidate)["state"], old, generation)
            else:
                if mode == "leadership" and point == "after-promote":
                    self.poll("guarded old primary re-fenced after candidate restart", lambda: self.stopped(old))
                expected = self.await_failover(generation, old)
            require(expected == (generation + 1, candidate), "resumption replaced committed candidate")
            for node in (candidate, survivor):
                require(self.sql(node, f"SELECT count(*) FROM public.maat_integration_markers WHERE id='{marker}';") == "1", "committed marker disappeared")
        self.event("crash scenario passed", point=point, mode=mode)

    @staticmethod
    def role_before(point):
        return point not in {"after-promote", "before-reconfiguring", "after-reconfiguring", "before-complete", "after-complete"}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True)
    parser.add_argument("--point", choices=POINTS + ("all",), default="all")
    parser.add_argument("--mode", choices=("crash", "quorum", "leadership"), default="crash")
    parser.add_argument("--timeout", type=int, default=90)
    parser.add_argument("--remove-test-volumes", action="store_true")
    parser.add_argument("--report", default="/tmp/maat-transition-crashes.json")
    args = parser.parse_args()
    if args.timeout <= 0:
        parser.error("--timeout must be positive")
    if not Path(args.binary).is_file():
        parser.error("--binary must name an existing fault-enabled Linux binary")
    report = []
    for point in POINTS if args.point == "all" else (args.point,):
        (ROOT / "bin").mkdir(exist_ok=True)
        directory = tempfile.mkdtemp(prefix="maat-transition-", dir=ROOT / "bin")
        lab = CrashLab(args.timeout, directory, args.binary)
        failed = False
        try:
            lab.create()
            lab.crash_case(point, args.mode)
        except (Failure, NotReady, KeyError, ValueError, KeyboardInterrupt) as error:
            failed = True
            lab.event("scenario failed", reason=str(error), point=point, mode=args.mode)
        finally:
            record = {"point": point, "mode": args.mode, "passed": not failed, "project": lab.project,
                      "directory": directory, **lab.evidence()}
            report.append(record)
            Path(args.report).write_text(json.dumps(report, indent=2, sort_keys=True))
            # A paused container must be unpaused before stop; touch only ours.
            for node in lab.ids:
                if lab.inspect(node)["State"]["Paused"]:
                    lab.command(["docker", "unpause", lab.ids[node]])
            lab.stop(args.remove_test_volumes)
        if failed:
            return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
