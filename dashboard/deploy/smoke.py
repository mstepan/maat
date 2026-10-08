#!/usr/bin/env python3
"""Real-terminal dashboard smoke in a fresh disposable Compose project.

Requires agent/bin/maat-linux and dashboard/bin/maat-dashboard. Stops containers
and retains volumes/assets; never operates on an existing development lab.
"""
import fcntl
import os
from pathlib import Path
import pty
import re
import select
import signal
import struct
import subprocess
import sys
import tempfile
import termios
import time

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "agent" / "deploy"))
from integration import DATA, NODES, Failure, require  # noqa: E402
from transition_crashes import CrashLab  # noqa: E402


class Terminal:
    def __init__(self, project):
        self.master, self.slave = pty.openpty()
        self.before = termios.tcgetattr(self.master)
        self.width, self.height = 100, 30
        fcntl.ioctl(self.slave, termios.TIOCSWINSZ, struct.pack("HHHH", 30, 100, 0, 0))
        def foreground():
            os.setsid()
            fcntl.ioctl(0, termios.TIOCSCTTY, 0)
        self.process = subprocess.Popen(
            [str(ROOT / "dashboard" / "bin" / "maat-dashboard"), "--project", project],
            stdin=self.slave, stdout=self.slave, stderr=self.slave,
            env=dict(os.environ, TERM="xterm-256color"), preexec_fn=foreground)
        self.output = b""
        self.closed = False

    def send(self, text):
        os.write(self.master, text.encode())

    def resize(self, width, height):
        self.width, self.height = width, height
        fcntl.ioctl(self.slave, termios.TIOCSWINSZ, struct.pack("HHHH", height, width, 0, 0))

    def expect(self, text, timeout=20, screen=True):
        deadline = time.monotonic() + timeout
        next_redraw = 0
        base_height = self.height
        while time.monotonic() < deadline:
            if screen and time.monotonic() >= next_redraw:
                # A real resize forces complete frames; ordinary draws are cursor deltas.
                self.resize(self.width, base_height + int(self.height == base_height))
                next_redraw = time.monotonic() + 1
            clean = re.sub(rb"\x1b\[[0-?]*[ -/]*[@-~]", b"", self.output)
            clean = re.sub(rb"\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)", b"", clean)
            clean = re.sub(rb"\x1b[()][0-2A-Z]", b"", clean).decode(errors="replace")
            if text in clean:
                if screen:
                    self.resize(self.width, base_height)
                self.output = b""
                return
            require(self.process.poll() is None, "dashboard exited before " + text)
            if select.select([self.master], [], [], 0.1)[0]:
                self.output += os.read(self.master, 65536)
                require(len(self.output) < 4 << 20, "terminal output exceeded smoke limit")
        print("terminal expectation failed: " + text, file=sys.stderr, flush=True)
        raise Failure("terminal did not display " + text)

    def fresh(self):
        self.output = b""
        while select.select([self.master], [], [], 0)[0]:
            os.read(self.master, 65536)

    def close(self, terminate=False):
        try:
            if self.process.poll() is None:
                self.process.terminate() if terminate else self.send("q")
                deadline = time.monotonic() + 5
                while self.process.poll() is None and time.monotonic() < deadline:
                    if select.select([self.master], [], [], 0.1)[0]:
                        os.read(self.master, 65536)
                require(self.process.poll() is not None, "dashboard did not quit")
            require(self.process.returncode == 0, "dashboard did not exit normally")
            after = termios.tcgetattr(self.master)
            require(after == self.before, "terminal attributes not restored")
        finally:
            if self.process.poll() is None:
                self.process.terminate()
                self.process.wait(timeout=5)
            self.closed = True
            os.close(self.master)
            os.close(self.slave)


def psql(terminal):
    terminal.fresh()
    terminal.send("p")
    terminal.expect("postgres=#", screen=False)
    terminal.send("SELECT rtrim(pg_read_file('/etc/hostname'), chr(10)) || '|' || pg_is_in_recovery()::text || '|' || current_user AS dashboard_target;\n")
    terminal.expect("instance-b|true|postgres", screen=False)
    terminal.send("SELECT 'jkpq?' AS dashboard_keys;\n")
    terminal.expect("(1 row)", screen=False)
    terminal.send("SELECT pg_sleep(30);\n")
    time.sleep(0.5)
    terminal.send("\x03")
    terminal.expect("canceling statement due to user request", screen=False)
    terminal.send("\\q\n")
    terminal.expect("Node: instance-b")


def main():
    for path in (ROOT / "agent/bin/maat-linux", ROOT / "dashboard/bin/maat-dashboard"):
        require(path.is_file(), "build required binary: " + str(path))
    directory = Path(tempfile.mkdtemp(prefix="maat-dashboard-", dir=ROOT / "dashboard/bin"))
    lab = CrashLab(120, directory, ROOT / "agent/bin/maat-linux", project_prefix="maat-dashboard")
    terminal = None
    paused = {}
    try:
        lab.create()
        authority = lab.stable()
        require(authority[1] == "instance-a", "unexpected initial primary")
        terminal = Terminal(lab.project)
        terminal.expect("3/3 current status responses")
        terminal.send("j\r")
        terminal.expect("Node: instance-b")
        # Full cluster and container IDs prove this is the isolated project's node.
        terminal.expect(lab.cluster)
        terminal.expect(lab.ids["instance-b"])
        psql(terminal)
        lab.event("native psql and Ctrl-C returned to selected replica details")

        pid = lab.status("instance-b")["agent_pid"]
        lab.signal("instance-b", "STOP", pid)
        paused["instance-b"] = pid
        terminal.fresh()
        terminal.expect("LAST KNOWN")
        require(lab.role("instance-b")["recovery"], "agent failure changed database role")
        psql(terminal)
        lab.signal("instance-b", "CONT", paused.pop("instance-b"))
        lab.stable(authority)
        terminal.fresh()
        terminal.expect("3/3 current status responses")
        lab.event("agent-only failure retained target and recovered")

        # Keep the replica agent reachable while removing Raft majority. It must
        # report the DB failure without automatically restarting this replica.
        for node in ("instance-a", "instance-c"):
            pid = lab.status(node)["agent_pid"]
            lab.signal(node, "STOP", pid)
            paused[node] = pid
        terminal.fresh()
        terminal.expect("STALE", timeout=50)
        lab.event("reachable replica displayed stale sampled lag without majority")
        lab.execute("instance-b", ["pg_ctl", "-D", DATA, "-m", "immediate", "-w", "stop"])
        terminal.fresh()
        terminal.expect("Database observation healthy: false")
        terminal.send("p")
        terminal.expect("psql session failed or could not be launched")
        require(not lab.status("instance-b")["local"]["database"]["healthy"], "dashboard started PostgreSQL")
        lab.event("PostgreSQL-only failure and failed psql launch restored UI")
        for node, pid in list(paused.items()):
            lab.signal(node, "CONT", pid)
            del paused[node]
        lab.stable(authority)
        terminal.fresh()
        terminal.expect("3/3 current status responses")

        lab.command(lab.compose + ["stop", "--timeout", "20", "instance-b"], timeout=30)
        terminal.fresh()
        terminal.expect("Runtime: stopped")
        terminal.send("p")
        terminal.expect("instance is unavailable")
        require(lab.stopped("instance-b"), "dashboard started stopped instance")
        lab.start("instance-b")
        lab.stable(authority)
        terminal.fresh()
        terminal.expect("3/3 current status responses")
        terminal.send("\x1b")
        terminal.expect("Nodes")
        terminal.send("\r")
        terminal.expect("Node: instance-b")
        terminal.resize(70, 20)
        terminal.expect("Resize terminal to at least 80 x 24")
        terminal.resize(80, 24)
        terminal.expect("Node: instance-b")
        terminal.send("j" * 45)
        terminal.expect("actual final transaction loss")
        terminal.close()
        terminal = Terminal(lab.project)
        terminal.expect("3/3 current status responses")
        terminal.close(terminate=True)
        terminal = None
        lab.event("stopped-node refusal, dynamic-port recovery, navigation and terminal restoration passed")
        lab.event("dashboard smoke passed", directory=str(directory))
    finally:
        for node, pid in paused.items():
            lab.signal(node, "CONT", pid)
        try:
            if terminal is not None and not terminal.closed:
                if terminal.process.poll() is None:
                    terminal.process.kill()
                    terminal.process.wait(timeout=5)
                os.close(terminal.master)
                os.close(terminal.slave)
        finally:
            lab.stop()


if __name__ == "__main__":
    try:
        main()
    except (Failure, OSError, subprocess.TimeoutExpired) as error:
        print(str(error), file=sys.stderr)
        sys.exit(1)
