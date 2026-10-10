#!/usr/bin/env python3
"""Verify the independent offline rescue helper against an actual local E2E backup.

The helper and manual Restic commands never read SQLite, original state, node keys,
or the original source. Run e2e_local.py --extended first and supply its directory.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess

ROOT = Path(__file__).resolve().parents[1]


def digest(path):
    h = hashlib.sha256()
    with path.open("rb") as f:
        for chunk in iter(lambda: f.read(1024 * 1024), b""):
            h.update(chunk)
    return h.hexdigest()


def run(args):
    work = args.work.resolve()
    assert not (work / "original-state").exists()
    assert not (work / "original-state-cache").exists()
    assert not any((work / "source").iterdir()), "original source must be absent"
    proof = work / "offline-rescue"
    proof.mkdir(mode=0o700)
    password = proof / "test-password"
    password.write_text("Local-test-recovery-2026!")
    password.chmod(0o600)
    certificates = sorted(work.glob("drive*/VaultPilot/*/catalog/*.ok"), key=lambda p: p.stat().st_mtime_ns, reverse=True)
    assert certificates, "no actual committed catalog"
    certificate = certificates[0]
    catalog = certificate.with_suffix(".vpc")
    out = proof / "register"
    common = [str(ROOT / "bin/vprescue"), "-kit", str(work / "recovery-kit.json"), "-catalog", str(catalog), "-password-file", str(password)]
    result = subprocess.run(common + ["-commit", str(certificate), "-out", str(out)], capture_output=True, text=True)
    assert result.returncode == 0, result.stderr
    assert not (out / "catalog.zip").exists(), "temporary plaintext ZIP left behind"
    assert out.stat().st_mode & 0o077 == 0
    control = json.loads((out / "control.json").read_text())
    repositories = {r["id"]: r for r in control["repositories"]}
    expected_report = json.loads((work / "report.json").read_text())
    expectations = {item["point_id"]: item["files"] for item in expected_report.get("restore_expectations", [])}
    checked = []
    for point in control["points"]:
        if point.get("deleted_at"):
            continue
        entries = [json.loads(line) for line in (out / "points" / (point["id"] + ".jsonl")).read_text().splitlines()]
        files = [e for e in entries if e["kind"] == "file" and e["path"] == "中文 文件.txt"]
        if not files:
            continue
        entry = files[0]
        repo = repositories[entry["repository_id"]]
        location = next(loc for loc in repo["locations"] if loc["path"] == repo["path"] and loc.get("verified"))
        matches = list(work.glob("drive*/" + location["path"] + "/config"))
        assert len(matches) == 1, "current physical repo must exist exactly once"
        repo_path = matches[0].parent
        secret_file = out / (repo["id"] + ".password")
        assert secret_file.stat().st_mode & 0o077 == 0
        env = dict(os.environ, RESTIC_PASSWORD_FILE=str(secret_file), RESTIC_CACHE_DIR=str(proof / "restic-cache"))
        snapshots = subprocess.run([str(args.restic), "-r", str(repo_path), "snapshots", "--json"], env=env, capture_output=True, text=True)
        assert snapshots.returncode == 0, snapshots.stderr
        snapshot_id = next(s["snapshot_id"] for s in point["snapshots"] if s["repository_id"] == repo["id"] and not s.get("empty"))
        assert any(s["id"] == snapshot_id for s in json.loads(snapshots.stdout)), "full snapshot reference missing"
        target = proof / ("restored-" + point["id"])
        restored = subprocess.run([str(args.restic), "-r", str(repo_path), "restore", snapshot_id, "--target", str(target), "--include", entry["snapshot_path"], "--verify"], env=env, capture_output=True, text=True)
        assert restored.returncode == 0, restored.stderr
        actual = target / entry["snapshot_path"].lstrip("/")
        expected = expectations.get(point["id"], {}).get(entry["path"])
        if expected is None:
            # Compatibility with older reports: these engine restores were already
            # checked against source hashes before the original source was removed.
            baseline = [p for p in (work / "restore").glob("version*/**/中文 文件.txt") if p.is_file()]
            assert digest(actual) in {digest(p) for p in baseline}
        else:
            assert digest(actual) == expected
        checked.append({"point_id": point["id"], "repository_id": repo["id"], "sha256": digest(actual)})
    assert len(checked) >= 2, "manual restore must verify both historical versions"
    tampered = proof / "tampered.ok"
    raw = bytearray(certificate.read_bytes())
    raw[-1] ^= 1
    tampered.write_bytes(raw)
    rejected_out = proof / "must-not-exist"
    rejected = subprocess.run(common + ["-commit", str(tampered), "-out", str(rejected_out)], capture_output=True)
    assert rejected.returncode != 0 and not rejected_out.exists()
    prepared = certificate.with_suffix(".prepare")
    assert prepared.is_file()
    rejected = subprocess.run(common + ["-commit", str(prepared), "-out", str(rejected_out)], capture_output=True)
    assert rejected.returncode != 0 and not rejected_out.exists(), "prepared receipt must not masquerade as completed certificate"
    password.write_text("Wrong-recovery-password!")
    rejected = subprocess.run(common + ["-commit", str(certificate), "-out", str(rejected_out)], capture_output=True)
    assert rejected.returncode != 0 and not rejected_out.exists()
    password.unlink()
    report = {"passed": True, "backend": "local actual Restic, independent offline vprescue", "original_state_used": False,
              "manual_historical_file_restores": checked, "tampered_certificate_rejected": True, "wrong_password_rejected": True, "uncommitted_prepare_rejected": True,
              "protected_output_permissions": True, "temporary_plaintext_zip_removed": True}
    path = proof / "report.json"
    path.write_text(json.dumps(report, ensure_ascii=False, indent=2))
    print(json.dumps({"passed": True, "report": str(path), "historical_versions": len(checked)}, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("work", type=Path)
    parser.add_argument("--restic", type=Path, default=ROOT / ".tools/restic")
    run(parser.parse_args())
