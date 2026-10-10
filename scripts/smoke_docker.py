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
import urllib.parse

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
        assert not status["oauth_configured"]
        def api(method, path, body=None):
            payload = None if body is None else json.dumps(body).encode()
            headers = {"Content-Type": "application/json", "X-CSRF-Token": setup["csrf_token"]}
            with opener.open(urllib.request.Request(base + "/api" + path, payload, headers, method=method), timeout=15) as response:
                return json.load(response)
        account = api("POST", "/accounts", {"name": "Rclone container protocol test"})
        flow = api("POST", "/accounts/" + account["id"] + "/rclone/authorize", {"version": account["version"]})
        path = "/accounts/" + account["id"] + "/rclone/authorize?flow_id=" + flow["id"]
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            flow = api("GET", path)
            if flow["status"] in ("waiting", "failed"):
                break
            time.sleep(0.1)
        assert flow["status"] == "waiting", flow.get("message")
        auth = urllib.parse.urlparse(flow["authorization_url"])
        params = urllib.parse.parse_qs(auth.query)
        assert auth.hostname == "login.microsoftonline.com"
        assert params["client_id"] == ["b15665d9-eda6-4092-8539-0eec376afd59"]
        assert params["redirect_uri"] == ["http://localhost:53682/"]
        # Relay a real Rclone denial callback inside the container. No login,
        # code exchange, or user OneDrive grant is performed.
        callback = "http://localhost:53682/?" + urllib.parse.urlencode({"error": "access_denied", "state": params["state"][0]})
        api("POST", "/accounts/" + account["id"] + "/rclone/callback?flow_id=" + flow["id"], {"url": callback})
        while time.monotonic() < deadline:
            flow = api("GET", path)
            if flow["status"] == "failed":
                break
            time.sleep(0.1)
        assert flow["status"] == "failed"
        fixture = {"access_token": "container-test-only-access", "refresh_token": "container-test-only-refresh", "token_type": "Bearer", "expiry": "2027-01-01T00:00:00Z"}
        config = "[test-only]\ntype = onedrive\ndrive_id = untrusted-hint\ndrive_type = personal\ntoken = " + json.dumps(fixture)
        preview = api("POST", "/rclone/import/preview", {"config": config})
        assert preview["total"] == 1 and "container-test-only-access" not in json.dumps(preview)
        imported = api("POST", "/accounts/" + account["id"] + "/rclone/import", {"version": account["version"], "config": config, "remote": "test-only"})
        assert imported["status"] == "awaiting_drive_selection" and not imported["verified"] and not imported["drive_id"]
        assert "container-test-only-access" not in json.dumps(api("GET", "/accounts"))
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
        assert "53682/tcp" not in inspect["NetworkSettings"]["Ports"]
        report = {"passed": True, "image": inspect["Image"], "platform": "Debian 12.12 Linux container", "uid": 1000,
                  "read_only_root": True, "read_only_source_bind": True, "dropped_capabilities": True,
                  "no_new_privileges": True, "healthcheck": "passed", "http_setup_login_directory_browser": "passed",
                  "rclone_standard_authorize_remote_denial": "passed without exposing 53682",
                  "rclone_import_preview_encrypted_save": "passed with synthetic protocol fixture; no real Microsoft grant",
                  "restic": docker("exec", cid, "restic", "version"), "rclone": docker("exec", cid, "rclone", "version").splitlines()[0]}
        (work / "report.json").write_text(json.dumps(report, ensure_ascii=False, indent=2))
        print(json.dumps({"passed": True, "report": str(work / "report.json")}, indent=2))
    finally:
        docker("rm", "-f", cid)


if __name__ == "__main__":
    run()
