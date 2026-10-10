#!/usr/bin/env python3
"""Start a disposable hardened Docker container and verify its real HTTP/runtime.

Does not modify production state, cloud accounts, or an existing container.
"""
import http.cookiejar
import json
from pathlib import Path
import subprocess
import time
import urllib.request

ROOT = Path(__file__).resolve().parents[1]


def docker(*args):
    return subprocess.check_output(["docker", *args], text=True).strip()


def run():
    work = ROOT / "test-artifacts" / ("docker-" + time.strftime("%Y%m%d-%H%M%S"))
    source = work / "source"
    source.mkdir(parents=True)
    (source / "container-readable.txt").write_text("read-only source mount\n")
    name = "vaultpilot-smoke-" + str(int(time.time()))
    cid = docker("run", "-d", "--name", name, "--user", "1000:1000", "--read-only", "--cap-drop=ALL",
                 "--security-opt", "no-new-privileges:true", "-p", "127.0.0.1::8080",
                 "--tmpfs", "/tmp:size=512m,mode=1777", "--tmpfs", "/app/state:uid=1000,gid=1000,mode=0700",
                 "--tmpfs", "/app/cache:uid=1000,gid=1000,mode=0700", "--tmpfs", "/restore:uid=1000,gid=1000,mode=0700",
                 "--mount", "type=bind,src=" + str(source) + ",dst=/sources,readonly",
                 "-e", "VP_ADDR=0.0.0.0:8080", "-e", "VP_STATE_DIR=/app/state", "-e", "VP_CACHE_DIR=/app/cache",
                 "-e", "VP_SOURCE_ROOTS=/sources", "-e", "VP_RESTORE_ROOTS=/restore", "vaultpilot:0.1.0")
    try:
        port = docker("port", cid, "8080/tcp").rsplit(":", 1)[1]
        base = "http://127.0.0.1:" + port
        opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            try:
                with opener.open(base + "/api/status", timeout=3) as response:
                    status = json.load(response)
                break
            except OSError:
                time.sleep(0.15)
        else:
            raise AssertionError("container startup failed: " + docker("logs", cid))
        assert not status["initialized"]
        with opener.open(base, timeout=3) as response:
            assert b"<div id=\"app\">" in response.read()
        payload = json.dumps({"username": "smoke-admin", "password": "Docker-smoke-login-2026!", "recovery_password": "Docker-smoke-recovery-2026!", "mode": "create"}).encode()
        with opener.open(urllib.request.Request(base + "/api/setup", payload, {"Content-Type": "application/json"}, method="POST")) as response:
            setup = json.load(response)
        with opener.open(base + "/api/status") as response:
            status = json.load(response)
        assert status["initialized"] and status["authenticated"] and status["unlocked"]
        assert status["engines"]["restic"] and status["engines"]["rclone"]
        with opener.open(base + "/api/browse?path=/sources") as response:
            browse = json.load(response)
        assert browse["path"] == "/sources"
        docker("exec", cid, "/app/healthcheck")
        assert docker("exec", cid, "id", "-u") == "1000"
        assert "12.12" in docker("exec", cid, "cat", "/etc/debian_version")
        inspect = json.loads(docker("inspect", cid))[0]
        assert inspect["HostConfig"]["ReadonlyRootfs"]
        assert "ALL" in inspect["HostConfig"]["CapDrop"]
        assert any("no-new-privileges" in value for value in inspect["HostConfig"]["SecurityOpt"])
        assert any(m["Destination"] == "/sources" and not m["RW"] for m in inspect["Mounts"])
        report = {"passed": True, "image": inspect["Image"], "platform": "Debian 12.12 Linux container", "uid": 1000,
                  "read_only_root": True, "read_only_source_bind": True, "dropped_capabilities": True,
                  "no_new_privileges": True, "healthcheck": "passed", "http_setup_login_directory_browser": "passed",
                  "restic": docker("exec", cid, "restic", "version"), "rclone": docker("exec", cid, "rclone", "version").splitlines()[0]}
        (work / "report.json").write_text(json.dumps(report, ensure_ascii=False, indent=2))
        print(json.dumps({"passed": True, "report": str(work / "report.json")}, indent=2))
    finally:
        docker("rm", "-f", cid)


if __name__ == "__main__":
    run()
