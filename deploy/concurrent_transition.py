#!/usr/bin/env python3
"""Race two real agents' validated proposals across a Raft leader change.

Requires bin/maat-faults-linux built with -tags maat_faults. Creates its own
Compose project; the ordinary maat-dev cluster is never modified. --cleanup
explicitly removes only this run's disposable containers and volumes.
"""
import argparse
from concurrent.futures import ThreadPoolExecutor
import json
from pathlib import Path
import uuid

from integration import DATA, NODES, Failure, NotReady, require
from transition_crashes import CrashLab

ROOT = Path(__file__).resolve().parent.parent
def run(lab):
    lab.create()
    for node in NODES:
        lab.identity(node)
    generation, primary = lab.stable()
    lab.checkpoint(primary)
    marker = lab.marker(primary)
    replicas = [node for node in NODES if node != primary]
    for node in replicas:
        lab.arm(node, "before-begin")
    lab.execute(primary, ["pg_ctl", "-D", DATA, "-m", "immediate", "-w", "stop"])

    def first_proposal():
        for node in replicas:
            reached = lab.reached(node)
            if reached:
                require(reached["point"] == "before-begin", "wrong proposal boundary")
                return node, reached
        return None

    first, reached = lab.poll("first eligible leader proposal", first_proposal, interval=0.05)
    initial = lab.status(first)
    require(initial["raft_is_leader"], "first proposer was not the leader")
    require(initial["state"]["Generation"] == generation, "authority changed before proposal")
    first_pid = reached["pid"]
    second = next(node for node in replicas if node != first)
    stopped = False
    try:
        lab.signal(first, "STOP", first_pid)
        stopped = True

        def second_proposal():
            reached = lab.reached(second)
            if not reached:
                return None
            status = lab.status(second)
            require(reached["point"] == "before-begin" and status["raft_is_leader"],
                    "second proposer is not an eligible new leader")
            require(status["raft_term"] > initial["raft_term"], "Raft term did not advance")
            require(status["state"]["Generation"] == generation, "proposal already changed authority")
            return status

        current = lab.poll("second leader proposal at the same generation", second_proposal, interval=0.05)
        lab.signal(first, "CONT", first_pid)
        stopped = False

        def stepdown():
            status = lab.status(first)
            return not status["raft_is_leader"] and status["raft_leader"] == second

        lab.poll("old proposer recognizes the new leader", stepdown, interval=0.05)
        lab.event("overlapping proposals ready", candidates=[first, second], generation=generation,
                  previous_term=initial["raft_term"], current_term=current["raft_term"])

        with ThreadPoolExecutor(max_workers=2) as executor:
            list(executor.map(lab.release, replicas))
        expected = lab.await_failover(generation, primary)
        require(expected == (generation + 1, second), "stale proposer replaced current leader authorization")
        state = lab.status(second)["state"]
        attempts = [event for event in state["History"]
                    if event["Kind"] == "begin" and event["Generation"] == generation]
        authorizations = [event for event in state["History"]
                          if event["Kind"] == "authorize" and event["Generation"] == generation + 1]
        require(len(attempts) == 1 and len(authorizations) == 1,
                "competing proposals committed multiple transitions or authorizations")
        require(attempts[0]["Transition"]["Candidate"] == second,
                "old leader's pending proposal was committed")
        lab.event("concurrent authorization verified", generation=expected[0], primary=expected[1],
                  committed_transitions=len(attempts), committed_authorizations=len(authorizations))
        lab.start(primary)
        lab.stable(expected)
        lab.verify_marker(marker)
        lab.event("concurrent transition scenario passed", generation=expected[0], primary=expected[1],
                  rejected_candidate=first, accepted_candidate=second,
                  committed_transitions=len(attempts), committed_authorizations=len(authorizations))
    finally:
        if stopped:
            lab.signal(first, "CONT", first_pid)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, default=ROOT / "bin/maat-faults-linux")
    parser.add_argument("--timeout", type=int, default=150)
    parser.add_argument("--cleanup", action="store_true", help="remove this run's disposable Compose volumes")
    args = parser.parse_args()
    if args.timeout < 1 or not args.binary.is_file():
        parser.error("a positive timeout and existing fault-enabled Linux binary are required")
    directory = ROOT / "bin" / ("concurrent-" + uuid.uuid4().hex[:12])
    directory.mkdir(parents=True, mode=0o700)
    lab = CrashLab(args.timeout, directory, args.binary.resolve())
    try:
        run(lab)
    except (Failure, NotReady, OSError, ValueError, KeyError, KeyboardInterrupt) as error:
        lab.event("concurrent transition scenario failed", reason=str(error))
        print(json.dumps(lab.evidence(), indent=2, sort_keys=True))
        return 1
    finally:
        lab.stop(remove_volumes=args.cleanup)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
