#!/usr/bin/env python3
"""Reproducible provider acceptance with private receipts and exact cleanup."""
import json
import os
from pathlib import Path
import signal
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import urllib.request
from types import FrameType
from typing import Any, Callable

ROOT = Path(__file__).resolve().parent.parent


def execute(args: list[str], *, timeout: float = 40, env: dict[str, str] | None = None,
            cwd: Path | None = None) -> subprocess.CompletedProcess[str]:
    return subprocess.run(args, text=True, capture_output=True, timeout=timeout, env=env, cwd=cwd)


class LiveTest:
    def __init__(self, provider: str) -> None:
        self.provider = provider
        self.port = int(os.environ.get("AGENTCTL_LIVE_TEST_PORT", 18091 if provider == "claude" else 18092))
        if not 1024 <= self.port <= 65535 or self.port == 38093:
            raise ValueError("unsafe isolated port")
        test_dir = ROOT / ".agentctl-live-test"
        if execute(["git", "check-ignore", "-q", str(test_dir)], cwd=ROOT).returncode:
            raise RuntimeError("live-test directory must be ignored")
        test_dir.mkdir(mode=0o700, exist_ok=True)
        parent = test_dir / provider
        parent.mkdir(mode=0o700, exist_ok=True)
        self.directory = Path(tempfile.mkdtemp(prefix="run.", dir=parent))
        self.data = self.directory / "data"
        self.work = self.directory / "work"
        for path in (self.data, self.work, self.directory / "cursors", self.directory / "bin"):
            path.mkdir(mode=0o700)
        self.env = dict(os.environ, AGENTD_URL=f"http://127.0.0.1:{self.port}",
                        AGENTD_TOKEN_FILE=str(self.data / "auth.token"),
                        AGENTCTL_CURSOR_DIR=str(self.directory / "cursors"))
        self.model = os.environ.get("AGENTCTL_LIVE_TEST_MODEL", "claude-haiku-4-5" if provider == "claude" else "gpt-6-luna")
        self.daemon = None
        self.sessions = []
        self.records = []
        self.commands = []
        self.binary = str(self.directory / "bin" / "agentctl")
        print(f"{provider}: evidence directory {self.directory}", flush=True)

    def ctl(self, *args: str, codes: tuple[int, ...] | None = (0,), timeout: float = 40) -> subprocess.CompletedProcess[str]:
        result = execute([self.binary, "--json", *args], env=self.env, timeout=timeout)
        record = {"command": ["agentctl", "--json", *args], "exit": result.returncode,
                  "stdout": result.stdout.strip(), "stderr": result.stderr.strip()}
        self.commands.append(record)
        (self.directory / "commands.json").write_text(json.dumps(self.commands, indent=2))
        if codes is not None and result.returncode not in codes:
            raise RuntimeError(json.dumps(record))
        return result

    def value(self, *args: str) -> Any:
        return json.loads(self.ctl(*args).stdout)

    def record(self, step: str, passed: bool, detail: str) -> None:
        self.records.append({"step": step, "status": "PASS" if passed else "FAIL", "detail": detail})
        (self.directory / "report.json").write_text(json.dumps(self.records, indent=2))
        print(f"{self.provider}: {'PASS' if passed else 'FAIL'} {step}: {detail}", flush=True)

    def start(self) -> None:
        with socket.socket() as listener:
            listener.bind(("127.0.0.1", self.port))
        for name in ("agentd", "agentctl"):
            result = execute(["go", "build", "-buildvcs=false", "-o", str(self.directory / "bin" / name), f"./cmd/{name}"], cwd=ROOT, timeout=120)
            if result.returncode:
                raise RuntimeError(result.stderr)
        # The daemon's recovery/pruner lists all AgentD containers. Filter
        # those listings to this run, so isolated tests cannot adopt/stop any
        # installed-daemon or sibling-test session. No global Docker changes.
        docker_proxy = self.directory / "bin" / "docker"
        docker_proxy.write_text((ROOT / "scripts" / "agentctl_docker_scope.py").read_text())
        docker_proxy.chmod(0o700)
        daemon_env = dict(self.env, PATH=str(self.directory / "bin") + os.pathsep + os.environ["PATH"],
                          AGENTCTL_LIVE_REAL_DOCKER=shutil.which("docker"), AGENTCTL_LIVE_SCOPE=self.directory.name)
        log = open(self.directory / "agentd.log", "w")
        self.daemon = subprocess.Popen([str(self.directory / "bin" / "agentd"), "--host", "127.0.0.1", "--port", str(self.port),
                                        "--runtime", "docker", "--credential-sync", "--data-dir", str(self.data)], env=daemon_env, stdout=log, stderr=log)
        log.close()
        for _ in range(350):
            if self.daemon.poll() is not None:
                raise RuntimeError("isolated daemon exited; inspect private agentd.log")
            if (self.data / "auth.token").exists() and self.ctl("ls", codes=None).returncode == 0:
                # HTTP listening precedes Docker readiness. This read-only health
                # probe does not drive a provider session or expose credentials.
                req = urllib.request.Request(self.env["AGENTD_URL"] + "/health",
                    headers={"Authorization": "Bearer " + (self.data / "auth.token").read_text().strip()})
                try:
                    with urllib.request.urlopen(req, timeout=5) as response:
                        health = json.load(response)
                    (self.directory / "health.json").write_text(json.dumps(health, indent=2))
                    if health.get("status") == "ok" and (self.provider != "claude" or (self.data / "credentials" / "claude-credentials.json").exists()):
                        return
                except Exception:
                    pass
            time.sleep(.2)
        raise RuntimeError("isolated daemon did not become ready; inspect health.json/agentd.log")

    def run(self, prompt: str, restricted: bool = False) -> str:
        args = ["run", "--provider", self.provider, "--model", self.model, "--cwd", str(self.work),
                "--runtime", "docker", "--keep-warm", "10m", "--context", "clean", "--permissions", "restricted" if restricted else "unrestricted",
                "--approval-policy", "on-request" if restricted else "never"]
        if self.provider == "codex":
            args += ["--effort", "low"]
        sid = self.value(*args, prompt)["session_id"]
        self.sessions.append(sid)
        return sid

    def events(self, sid: str, pattern: str = ".", kind: str | None = None) -> list[dict[str, Any]]:
        args = ["grep", sid, pattern]
        if kind:
            args += ["--type", kind]
        return self.value(*args) or []

    def tail(self, sid: str) -> int:
        return max((e["sequence"] for e in self.events(sid)), default=0)

    def wait(self, sid: str, caller: str = "turns", until: str = "turn_end,error",
             codes: tuple[int, ...] = (10,), timeout: str = "150s") -> subprocess.CompletedProcess[str]:
        return self.ctl("--as", caller, "wait", sid, "--until", until, "--timeout", timeout, codes=codes, timeout=170)

    def until(self, check: Callable[[], Any], seconds: float = 45) -> Any:
        deadline = time.monotonic() + seconds
        while time.monotonic() < deadline:
            result = check()
            if result:
                return result
            time.sleep(.2)
        raise RuntimeError("condition did not become true before deadline")

    def assistant_has(self, sid: str, token: str, after: int = 0) -> bool:
        for event in self.events(sid, token):
            if event["sequence"] <= after:
                continue
            p = event["payload"]
            item = p.get("item", {})
            if event["type"] in ("content.delta", "content.completed") and token in p.get("text", ""):
                return True
            if item.get("type") == "agentMessage" and token in item.get("text", ""):
                return True
        return False

    def tool_started(self, sid: str, after: int) -> bool:
        for e in self.events(sid, ".", "tool.call"):
            p = e["payload"]
            if e["sequence"] > after and (p.get("calls") or p.get("item", {}).get("type") == "commandExecution"):
                return True
        return False

    def flow(self) -> None:
        # A real foreground workload with start/finish effects prevents a fast
        # model response from being mistaken for an interrupted command.
        fixture = self.work / "long-turn.sh"
        fixture.write_text('#!/bin/sh\nset -eu\nprintf started > "$1.started"\nsleep 60\nprintf finished > "$1.finished"\nprintf LONG_TURN_FINISHED\n')
        sid = self.run("Reply exactly LIVE_TEST_ORBIT_7319. No tools.")
        self.record("run", True, sid + " Docker maintain 10m")
        result = self.wait(sid)
        self.record("wait", True, f"exit={result.returncode}; turn.completed")
        status = self.value("status", sid)
        self.record("status", status.get("turn") not in (None, "-"), json.dumps(status))
        first = self.ctl("since", sid).stdout
        second = self.ctl("since", sid).stdout
        self.record("since x2", bool(first.strip()) and not second.strip(), f"first={len(first)} bytes second={len(second)} bytes")
        found = self.assistant_has(sid, "LIVE_TEST_ORBIT_7319")
        self.record("grep", found, "assistant output contains LIVE_TEST_ORBIT_7319")
        self.ctl("prompt", sid, "Reply exactly FOLLOWUP_ORBIT_8421. No tools.")
        self.wait(sid)
        self.record("prompt", self.assistant_has(sid, "FOLLOWUP_ORBIT_8421"), "same maintained session: FOLLOWUP_ORBIT_8421")

        before = self.tail(sid)
        self.ctl("prompt", sid, "Run bash ./long-turn.sh steer with a 120000ms timeout in the foreground. This is an interrupt test fixture; wait for completion. Do not background it.")
        self.until(lambda: self.tool_started(sid, before) and (self.work / "steer.started").exists())
        self.ctl("steer", sid, "Stop the sleep task. Reply exactly STEERED_ORBIT_9532 and do not use more tools.")
        self.until(lambda: self.assistant_has(sid, "STEERED_ORBIT_9532", before), seconds=100)
        # Claude steering can produce an aborted turn followed by its replacement.
        self.until(lambda: any(e["sequence"] > before and e["type"] == "turn.completed" for e in self.events(sid)))
        self.ctl("--as", "turns", "since", sid)  # drain all steering turn boundaries
        self.record("steer", True, "active shell sleep changed to STEERED_ORBIT_9532")

        self.approvals_flow()
        before = self.tail(sid)
        self.ctl("prompt", sid, "Run bash ./long-turn.sh interrupt with a 120000ms timeout in the foreground. This is an interrupt test fixture. Wait for completion, then reply INTERRUPT_SHOULD_NOT_COMPLETE. Do not background it.")
        self.until(lambda: self.tool_started(sid, before) and (self.work / "interrupt.started").exists())
        self.ctl("interrupt", sid)
        interrupted = self.wait(sid, codes=(10, 12), timeout="90s")
        ends = [e for e in self.events(sid) if e["sequence"] > before and e["type"] in ("turn.completed", "turn.failed")]
        ok = any(e["payload"].get("status") == "interrupted" or (e["payload"].get("native_subtype") == "error_during_execution" and e["payload"].get("result", {}).get("terminal_reason") == "aborted_tools") for e in ends)
        self.record("interrupt", ok and not self.assistant_has(sid, "INTERRUPT_SHOULD_NOT_COMPLETE", before) and not (self.work / "interrupt.finished").exists(),
                    f"exit={interrupted.returncode}; turn ends={json.dumps([{'seq': e['sequence'], 'type': e['type'], 'status': e['payload'].get('status'), 'terminal_reason': e['payload'].get('result', {}).get('terminal_reason')} for e in ends])}")
        # Interrupt ends a turn; terminating the warm session creates its immutable receipt.
        self.ctl("terminate", sid)
        self.wait(sid, caller="terminal", until="done", codes=(13,))
        value = self.value("result", sid)
        self.record("result", value.get("receipt", {}).get("reason") == "terminated" and "final_assistant_text" in value,
                    json.dumps(value))

    def approvals_flow(self) -> None:
        sid = self.run("Reply READY only. No tools.", restricted=True)
        self.wait(sid)
        for decision, marker in (("approve", "AGENTCTL_APPROVED_7319"), ("deny", "AGENTCTL_DENIED_8421")):
            before = self.tail(sid)
            path = f"/var/tmp/{sid}-{marker}"
            prompt = (f"Use Bash/shell to run exactly: printf {marker} > {path} . This is outside the workspace, so request escalated permissions/approval for that exact command. "
                      "Invoke the tool now; do not ask for permission in prose. The tool approval callback handles consent. Do not use an alternative path or command. If denied, stop and reply DENIED without retry. If allowed, reply ALLOWED.")
            self.ctl("prompt", sid, prompt)
            self.wait(sid, caller="turns", until="approval,error,turn_end", codes=(11,))
            pending = self.value("approvals", sid)["approvals"]
            if not pending:
                raise RuntimeError("approval event had no live pending request")
            request_id = json.dumps(pending[0]["request_id"])
            self.ctl(decision, sid, request_id)
            self.wait(sid)
            # Independent readback inspects only this test's container. All
            # provider session/control operations still go through agentctl.
            ids = execute(["docker", "ps", "-q", "--filter", f"label=agentruntime.session_id={sid}"]).stdout.split()
            if len(ids) != 1:
                raise RuntimeError("expected one test-owned container")
            readback = execute(["docker", "exec", ids[0], "cat", path])
            ok = (readback.returncode == 0 and readback.stdout == marker) if decision == "approve" else readback.returncode == 1
            self.record(decision, ok, f"request_id={request_id}; method={pending[0]['method']}; file readback exit={readback.returncode} stdout={readback.stdout!r}")
        self.ctl("terminate", sid)
        self.wait(sid, caller="terminal", until="done", codes=(13,))
        result = self.value("result", sid)
        if "DENIED" not in result.get("final_assistant_text", ""):
            raise RuntimeError("ordinary result fallback lost final assistant text")

    def cleanup(self) -> None:
        errors = []
        if self.daemon and self.daemon.poll() is None:
            for sid in self.sessions:
                try:
                    self.ctl("terminate", sid, codes=None)
                except Exception as exc:
                    errors.append(str(exc))
            self.daemon.terminate()
            try:
                self.daemon.wait(timeout=15)
            except subprocess.TimeoutExpired:
                self.daemon.kill()
                self.daemon.wait(timeout=5)
        # Scope cleanup also catches a container if dispatch timed out before
        # agentctl could return its session ID.
        args = ["docker", "ps", "-aq", "--filter", f"label=agentctl.live_test={self.directory.name}"]
        ids = execute(args).stdout.split()
        if ids:
            result = execute(["docker", "rm", "-f", *ids])
            if result.returncode:
                errors.append(result.stderr)
        if execute(args).stdout.strip():
            errors.append("test-owned containers remain")
        self.record("cleanup", not errors, "; ".join(errors) if errors else "isolated daemon stopped; no owned containers remain")


def main() -> int:
    if len(sys.argv) != 2 or sys.argv[1] not in ("claude", "codex"):
        sys.exit("Usage: scripts/agentctl-live-test.sh claude|codex")
    os.umask(0o077)
    test = LiveTest(sys.argv[1])
    def stop(signum: int, frame: FrameType | None) -> None:
        raise KeyboardInterrupt(f"signal {signum}")
    signal.signal(signal.SIGTERM, stop)
    try:
        test.start()
        test.flow()
    except (Exception, KeyboardInterrupt) as exc:
        test.record("flow", False, str(exc))
    finally:
        test.cleanup()
    return 1 if any(r["status"] == "FAIL" for r in test.records) else 0


if __name__ == "__main__":
    sys.exit(main())
