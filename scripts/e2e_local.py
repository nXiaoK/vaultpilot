#!/usr/bin/env python3
"""Real Restic API round-trip including recovery from an entirely new state directory.

Uses two local storage entries, not OneDrive. No mock backup engine or mock data API.
Artifacts stay under test-artifacts; test credentials are disposable.
"""
import argparse
import hashlib
import http.cookiejar
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import time
import urllib.error
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
TERMINAL = {"completed", "failed", "partial_failed", "cancelled", "interrupted"}
LOGIN_PASSWORD = "Local-test-login-2026!"
RECOVERY_PASSWORD = "Local-test-recovery-2026!"


class API:
    def __init__(self, port):
        self.url = "http://127.0.0.1:%s/api" % port
        self.csrf = ""
        self.opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))

    def call(self, method, path, body=None, headers=None):
        payload = None if body is None else json.dumps(body, ensure_ascii=False).encode()
        h = {"Content-Type": "application/json", "X-CSRF-Token": self.csrf}
        h.update(headers or {})
        request = urllib.request.Request(self.url + path, payload, h, method=method)
        try:
            with self.opener.open(request, timeout=60) as response:
                data = response.read()
                result = json.loads(data) if data else None
        except urllib.error.HTTPError as error:
            raise AssertionError("%s %s: %s %s" % (method, path, error.code, error.read().decode())) from error
        if isinstance(result, dict) and result.get("csrf_token"):
            self.csrf = result["csrf_token"]
        return result

    def wait(self, task, timeout=180):
        current = self.wait_terminal(task, timeout)
        if current["status"] != "completed":
            logs = self.call("GET", "/tasks/" + task["id"] + "/logs?limit=100")
            raise AssertionError(json.dumps({"task": current, "logs": logs}, ensure_ascii=False, indent=2))
        return current

    def wait_terminal(self, task, timeout=180):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            current = self.call("GET", "/tasks/" + task["id"])
            if current["status"] in TERMINAL:
                return current
            time.sleep(0.25)
        raise AssertionError("task timeout: " + task["id"])

    def wait_failure(self, task, expected_code, timeout=180):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            current = self.call("GET", "/tasks/" + task["id"])
            if current["status"] in TERMINAL:
                assert current["status"] in {"failed", "partial_failed"}, current
                assert current["code"] == expected_code, current
                return current
            time.sleep(0.25)
        raise AssertionError("expected failure timed out: " + task["id"])


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def start_server(work, state, restic, rclone):
    port = free_port()
    env = dict(os.environ, VP_ADDR="127.0.0.1:%s" % port, VP_PUBLIC_URL="http://127.0.0.1:%s" % port,
               VP_STATE_DIR=str(state), VP_CACHE_DIR=str(state.parent / (state.name + "-cache")),
               VP_SOURCE_ROOTS=str(work / "source"), VP_RESTORE_ROOTS=str(work / "restore"),
               VP_ALLOW_LOCAL="true", VP_RESTIC_BIN=str(restic), VP_RCLONE_BIN=str(rclone))
    handle = open(work / (state.name + ".log"), "wb")
    process = subprocess.Popen([str(ROOT / "bin/vaultpilot")], cwd=ROOT, env=env, stdout=handle, stderr=handle)
    api = API(port)
    deadline = time.monotonic() + 20
    while time.monotonic() < deadline:
        if process.poll() is not None:
            raise AssertionError("server failed: " + (work / (state.name + ".log")).read_text())
        try:
            api.call("GET", "/status")
            return process, handle, api
        except (OSError, AssertionError):
            time.sleep(0.1)
    process.terminate()
    raise AssertionError("server startup timeout")


def stop_server(process, handle):
    process.terminate()
    try:
        process.wait(timeout=20)
    except subprocess.TimeoutExpired:
        process.kill()
        process.wait()
    handle.close()


def add_accounts(api, work, budget=128 * 1024 * 1024, maintenance_reserve=2 * 1024 * 1024):
    return [api.call("POST", "/accounts", {
        "name": "真实本地存储 %s" % number, "backend": "local", "local_path": str(work / ("drive%s" % number)),
        "budget_bytes": budget, "maintenance_reserve_bytes": maintenance_reserve,
    }) for number in (1, 2)]


def hash_bytes(content):
    return hashlib.sha256(content).hexdigest()


def verify_tree(target, expected, absent=()):
    for name, digest in expected.items():
        matches = [p for p in target.rglob(name) if p.is_file() and not p.is_symlink()]
        assert len(matches) == 1, (name, [str(p) for p in matches])
        assert hash_bytes(matches[0].read_bytes()) == digest, name
    for name in absent:
        assert not list(target.rglob(name)), "deleted/excluded file resurrected: " + name
    assert list(target.rglob("空目录")), "empty directory missing"
    links = list(target.rglob("链接"))
    assert len(links) == 1 and links[0].is_symlink(), "symlink metadata missing"


def run(args):
    work = (ROOT / "test-artifacts" / ("e2e-" + time.strftime("%Y%m%d-%H%M%S"))).resolve()
    for name in ("source", "source/空目录", "drive1", "drive2", "restore"):
        (work / name).mkdir(parents=True, exist_ok=True)
    source = work / "source"
    original = {"中文 文件.txt": "VaultPilot 中文与空格\n".encode(), "换行\n文件.txt": b"newline path\x00content",
                "删除.txt": b"old historical content", "大文件 A.bin": os.urandom(20 * 1024 * 1024),
                "大文件 B.bin": os.urandom(20 * 1024 * 1024)}
    for name, content in original.items():
        (source / name).write_bytes(content)
    (source / "skip.tmp").write_bytes(b"must not be backed up")
    (source / "链接").symlink_to("中文 文件.txt")
    report = {"backend": "local directories, real Restic", "restic_version": subprocess.check_output([str(args.restic), "version"]).decode().strip(),
              "original_state_used_for_recovery": False, "checks": []}
    process, handle, api = start_server(work, work / "original-state", args.restic, args.rclone)
    try:
        api.call("POST", "/setup", {"username": "admin", "password": LOGIN_PASSWORD, "recovery_password": RECOVERY_PASSWORD, "mode": "create"})
        initial_budget = 34 * 1024 * 1024
        accounts = add_accounts(api, work, budget=initial_budget, maintenance_reserve=1024 * 1024)
        pool = api.call("POST", "/pools", {"name": "双存储联调池", "account_ids": [a["id"] for a in accounts], "catalog_replicas": 2, "shard_target_bytes": 24 * 1024 * 1024})
        src = api.call("POST", "/sources", {"name": "测试源", "path": str(source), "include_hidden": True})
        plan = api.call("POST", "/plans", {"name": "端到端备份", "source_ids": [src["id"]], "pool_id": pool["id"], "timezone": "Asia/Shanghai", "enabled": False, "excludes": ["*.tmp"], "retention": {"daily": 7, "weekly": 4, "monthly": 12}, "schedule": "0 2 * * *"})
        preview = api.wait(api.call("POST", "/plans/" + plan["id"] + "/preview", {}))
        assert preview.get("result"), "allocation preview is empty"
        assert preview["result"]["logical_bytes"] > initial_budget, "source must exceed any one account budget"
        assert len({shard["account_id"] for shard in preview["result"]["shards"]}) == 2
        first = api.wait(api.call("POST", "/plans/" + plan["id"] + "/run", {}, {"Idempotency-Key": "first-run"}))
        duplicate = api.call("POST", "/plans/" + plan["id"] + "/run", {}, {"Idempotency-Key": "first-run"})
        assert duplicate["id"] == first["id"], "idempotency violated"
        point1 = api.call("GET", "/recovery-points")["items"][0]
        kit = api.call("POST", "/security/recovery-kit", {"password": RECOVERY_PASSWORD, "admin_password": LOGIN_PASSWORD})
        initial_repository_count = len(api.call("GET", "/repositories")["items"])
        report["allocation_evidence"] = {"single_account_budget_bytes": initial_budget,
                                         "single_source_logical_bytes": preview["result"]["logical_bytes"],
                                         "allocated_account_count": 2}
        # Expand only after proving the first point exceeds each single-account budget.
        accounts = api.call("GET", "/accounts")["items"]
        accounts = [api.call("PUT", "/accounts/" + account["id"], {
            **account, "budget_bytes": 128 * 1024 * 1024,
            "maintenance_reserve_bytes": 2 * 1024 * 1024}) for account in accounts]
        report["checks"].append("first backup, two catalog replicas, idempotency, allocation preview")
        (source / "中文 文件.txt").write_bytes("modified 第二版\n".encode())
        (source / "删除.txt").unlink()
        (source / "换行\n文件.txt").rename(source / "重命名.txt")
        added_after_kit = os.urandom(5 * 1024 * 1024)
        (source / "恢复包导出后新增.bin").write_bytes(added_after_kit)
        api.wait(api.call("POST", "/plans/" + plan["id"] + "/run", {}, {"Idempotency-Key": "second-run"}))
        later_repository_count = len(api.call("GET", "/repositories")["items"])
        assert later_repository_count > initial_repository_count, "later file did not create a repository after kit export"
        report["recovery_kit_evidence"] = {"exported_before_repository_count": initial_repository_count,
                                           "later_repository_count": later_repository_count,
                                           "kit_reexported": False}
        point2 = api.call("GET", "/recovery-points")["items"][0]
        assert point2["id"] != point1["id"]
        if args.extended:
            # Preserve the first point; explicitly clean only a superseded unprotected point.
            point1 = api.call("POST", "/recovery-points/" + point1["id"] + "/protect", {"protected": True, "version": point1["version"]})
            third = api.wait(api.call("POST", "/plans/" + plan["id"] + "/run", {}, {"Idempotency-Key": "unchanged-run"}))
            assert third["uploaded_bytes"] < first["uploaded_bytes"] // 2, "unchanged run uploaded the whole original data"
            newest = api.call("GET", "/recovery-points")["items"][0]
            retention = api.call("POST", "/maintenance/retention/preview", {"plan_id": plan["id"]})
            assert not retention["blocked"] and point1["id"] in retention["keep_ids"]
            assert point2["id"] in retention["delete_ids"] and point1["id"] not in retention["delete_ids"]
            api.wait(api.call("POST", "/maintenance/retention", {"plan_id": plan["id"], "point_ids": [point2["id"]], "confirm": True}))
            assert point2["id"] not in {p["id"] for p in api.call("GET", "/recovery-points")["items"]}
            point2 = newest
            repos = api.call("GET", "/repositories")["items"]
            assert len({r["account_id"] for r in repos}) == 2, "file shards were not allocated to both storage entries"
            repository = next(r for r in repos if sum(other["account_id"] == r["account_id"] for other in repos) > 1)
            target = next(a for a in accounts if a["id"] != repository["account_id"])
            source_account = next(a for a in accounts if a["id"] == repository["account_id"])
            old_repo = Path(source_account["local_path"]) / repository["path"]
            migration = api.wait(api.call("POST", "/maintenance/migrate", {"repository_id": repository["id"], "target_account_id": target["id"], "confirm": True}))
            assert migration["result"]["old_copy_preserved"] and old_repo.is_dir()
            api.wait(api.call("POST", "/maintenance/check", {"repository_ids": [repository["id"]], "mode": "full"}))
            moved = next(r for r in api.call("GET", "/repositories")["items"] if r["id"] == repository["id"])
            current_index = next(i for i, location in enumerate(moved["locations"])
                                 if location["account_id"] == moved["account_id"] and location["path"] == moved["path"])
            old_index = next(i for i, location in enumerate(moved["locations"])
                             if location["account_id"] == source_account["id"] and location["path"] == repository["path"])
            cleanup = {"repository_id": repository["id"], "cleanup_old": True,
                       "old_location_index": old_index, "old_location_path": repository["path"], "confirm": True}
            api.wait_failure(api.call("POST", "/maintenance/migrate", {
                **cleanup, "old_location_index": current_index, "old_location_path": moved["path"]}),
                "CURRENT_LOCATION_PROTECTED")
            api.wait_failure(api.call("POST", "/maintenance/migrate", {
                **cleanup, "old_location_path": repository["path"] + "-stale"}), "VERSION_CONFLICT")
            assert old_repo.is_dir(), "rejected cleanup removed old copy"
            new_repo = Path(target["local_path"]) / moved["path"]
            pack = next(path for path in (new_repo / "data").rglob("*") if path.is_file())
            original_pack = pack.read_bytes()
            original_pack_mode = pack.stat().st_mode & 0o777
            try:
                # A structure-only check cannot prove these bytes; cleanup must read and authenticate the data pack.
                damaged_pack = bytearray(original_pack)
                damaged_pack[len(damaged_pack) // 2] ^= 1
                pack.chmod(original_pack_mode | 0o200)
                pack.write_bytes(damaged_pack)
                api.wait_failure(api.call("POST", "/maintenance/migrate", cleanup), "MIGRATION_CHECK_FAILED")
                assert old_repo.is_dir(), "failed current-location verification deleted last valid copy"
            finally:
                pack.write_bytes(original_pack)
                pack.chmod(original_pack_mode)
            cleaned = api.wait(api.call("POST", "/maintenance/migrate", cleanup))
            assert cleaned["result"]["old_copy_deleted"] and cleaned["result"]["new_location_verified"]
            assert cleaned["result"]["snapshot_references_checked"] > 0
            assert not old_repo.exists() and new_repo.is_dir(), "cleanup removed the current repository or retained old data"
            api.wait_failure(api.call("POST", "/maintenance/migrate", cleanup), "OLD_COPY_ALREADY_DELETED")
            api.wait(api.call("POST", "/maintenance/check", {"repository_ids": [repository["id"]], "mode": "full"}))
            report["checks"].append("incremental unchanged data, protected retention/forget/prune, real whole-repository migration and full data check")
            report["checks"].append("confirmed old-copy cleanup, current/stale-location rejection, corrupted-pack full-check rejection, old location removed and current snapshots remain readable")
        (work / "recovery-kit.json").write_text(json.dumps(kit))
        latest_hashes = {name: hash_bytes(content) for name, content in original.items()
                         if name not in ("中文 文件.txt", "删除.txt", "换行\n文件.txt")}
        latest_hashes.update({"中文 文件.txt": hash_bytes("modified 第二版\n".encode()),
                              "重命名.txt": hash_bytes(original["换行\n文件.txt"]),
                              "恢复包导出后新增.bin": hash_bytes(added_after_kit)})
        # Recorded while the original source still exists, before state and source destruction.
        report["restore_expectations"] = [
            {"point_id": point1["id"], "source_id": src["id"],
             "files": {name: hash_bytes(content) for name, content in original.items()},
             "absent": ["skip.tmp", "重命名.txt", "恢复包导出后新增.bin"], "directories": ["", "空目录"],
             "symlinks": {"链接": "中文 文件.txt"}},
            {"point_id": point2["id"], "source_id": src["id"], "files": latest_hashes,
             "absent": ["删除.txt", "换行\n文件.txt", "skip.tmp"], "directories": ["", "空目录"],
             "symlinks": {"链接": "中文 文件.txt"}},
        ]
        report["checks"].append("historical versions after modify, delete and rename")
    finally:
        stop_server(process, handle)
    # Move every original state/cache file out of access, then remove the live source.
    shutil.rmtree(work / "original-state")
    shutil.rmtree(work / "original-state-cache")
    shutil.rmtree(source)
    source.mkdir()
    process, handle, api = start_server(work, work / "fresh-state", args.restic, args.rclone)
    try:
        api.call("POST", "/setup", {"username": "recovery-admin", "password": LOGIN_PASSWORD, "recovery_password": RECOVERY_PASSWORD, "mode": "recovery", "recovery_kit": kit})
        status = api.call("GET", "/status")
        assert status["mode"] == "recovery"
        new_accounts = add_accounts(api, work)
        assert all(a["id"] not in {old["id"] for old in accounts} for a in new_accounts)
        api.wait(api.call("POST", "/security/discover", {}))
        if args.extended:
            api.wait(api.call("POST", "/security/discover", {}))
        discovered = api.call("GET", "/recovery-points")["items"]
        assert {point1["id"], point2["id"]} <= {p["id"] for p in discovered}, "catalog rebuild missing history"
        for number, point in ((1, point1), (2, point2)):
            target = work / "restore" / ("version%s" % number)
            request = {"point_id": point["id"], "target": str(target), "overwrite": False, "allow_partial": False}
            api.call("POST", "/restore/preview", request)
            api.wait(api.call("POST", "/restore", request))
            if number == 1:
                verify_tree(target, {n: hash_bytes(v) for n, v in original.items()}, ("skip.tmp", "重命名.txt", "恢复包导出后新增.bin"))
            else:
                expected = {n: hash_bytes(v) for n, v in original.items() if n not in ("中文 文件.txt", "删除.txt", "换行\n文件.txt")}
                expected.update({"中文 文件.txt": hash_bytes("modified 第二版\n".encode()), "重命名.txt": hash_bytes(original["换行\n文件.txt"]),
                                 "恢复包导出后新增.bin": hash_bytes(added_after_kit)})
                verify_tree(target, expected, ("删除.txt", "换行\n文件.txt", "skip.tmp"))
        report["checks"].append("fresh state, new account IDs, absent original source, both versions restored with SHA-256")
        report["checks"].append("Chinese, spaces, newline, empty directory and symbolic link preserved")
        report["checks"].append("recovery kit exported before later repository creation discovers new repository key and restores added file")
        if args.extended:
            request = {"point_id": point2["id"], "paths": [{"source_id": src["id"], "path": "中文 文件.txt"}], "target": str(work / "restore" / "single"), "overwrite": False}
            preview = api.call("POST", "/restore/preview", request)
            assert preview["file_count"] == 1 and not preview["blocked"]
            api.wait(api.call("POST", "/restore", request))
            single = next((work / "restore" / "single").rglob("中文 文件.txt"))
            assert single.read_bytes() == "modified 第二版\n".encode()
            single.write_bytes(b"existing target must be preserved")
            assert api.call("POST", "/restore/preview", request)["code"] == "RESTORE_CONFLICT"
            failed = api.wait_terminal(api.call("POST", "/restore", request))
            assert failed["status"] == "failed" and single.read_bytes() == b"existing target must be preserved"
            request["overwrite"] = True
            api.wait(api.call("POST", "/restore", request))
            assert single.read_bytes() == "modified 第二版\n".encode()
            offline = work / "drive1-offline"
            (work / "drive1").rename(offline)
            try:
                request = {"point_id": point2["id"], "target": str(work / "restore" / "partial"), "allow_partial": True}
                preview = api.call("POST", "/restore/preview", request)
                assert preview["missing_file_count"] > 0 and not preview["blocked"]
                partial = api.wait_terminal(api.call("POST", "/restore", request))
                assert partial["status"] == "partial_failed" and partial["result"]["skipped"] > 0 and partial["result"]["restored"] > 0
            finally:
                offline.rename(work / "drive1")
            report["checks"].append("repeat discovery, single-file restore, conflict preservation, explicit overwrite and partial offline-account restore")
    finally:
        stop_server(process, handle)
    report["passed"] = True
    (work / "report.json").write_text(json.dumps(report, ensure_ascii=False, indent=2))
    print(json.dumps({"passed": True, "report": str(work / "report.json"), "checks": report["checks"]}, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--restic", type=Path, default=ROOT / ".tools/restic")
    parser.add_argument("--rclone", type=Path, default=ROOT / ".tools/rclone")
    parser.add_argument("--extended", action="store_true", help="also exercise check, retention, migration and conflict/partial restore")
    run(parser.parse_args())
