"""Focused safety checks for the offline deployment backup tooling."""

import hashlib
import importlib.util
import io
import json
import os
import subprocess
import sys
import tarfile
from pathlib import Path
from types import SimpleNamespace

import pytest

BACKUP_SCRIPT = Path(__file__).resolve().parents[1] / "deployment" / "backup.py"
SPEC = importlib.util.spec_from_file_location("deployment_backup", BACKUP_SCRIPT)
backup = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(backup)


def make_tar(path, members):
    """Write (name, kind, data) members, allowing intentionally unsafe tar entries."""
    with tarfile.open(path, "w") as archive:
        for name, kind, data in members:
            item = tarfile.TarInfo(name)
            item.uid = 123
            item.gid = 456
            item.mode = 0o640
            if kind == "file":
                item.size = len(data)
                archive.addfile(item, io.BytesIO(data))
            else:
                item.type = kind
                item.linkname = "target"
                archive.addfile(item)


@pytest.mark.parametrize(
    "members",
    [
        [("../escape", "file", b"x")],
        [("/absolute", "file", b"x")],
        [("./entry", "file", b"x")],
        [(".", tarfile.DIRTYPE, b"")],
        [("dir//entry", "file", b"x")],
        [("dir/../entry", "file", b"x")],
        [("dir\\entry", "file", b"x")],
        [("entry", "file", b"first"), ("entry", "file", b"second")],
        [("symlink", tarfile.SYMTYPE, b"")],
        [("hardlink", tarfile.LNKTYPE, b"")],
    ],
    ids=[
        "traversal",
        "absolute",
        "dot-component",
        "root-dot",
        "double-separator",
        "parent-component",
        "backslash",
        "duplicate",
        "symlink",
        "hardlink",
    ],
)
def test_archive_inventory_rejects_unsafe_members(tmp_path, members):
    path = tmp_path / "unsafe.tar"
    make_tar(path, members)

    with pytest.raises(backup.Refusal):
        backup.archive_inventory(path)


def test_archive_inventory_records_ordinary_file_hash_and_metadata(tmp_path):
    path = tmp_path / "ordinary.tar"
    payload = b"policy bundle bytes\n"
    make_tar(path, [("policy/model.bin", "file", payload)])

    assert backup.archive_inventory(path) == {
        "policy/model.bin": {
            "kind": "file",
            "uid": 123,
            "gid": 456,
            "mode": 0o640,
            "size": len(payload),
            "sha256": hashlib.sha256(payload).hexdigest(),
        }
    }


def test_protected_directory_requires_new_path_and_uses_private_mode(tmp_path):
    existing = tmp_path / "existing"
    existing.mkdir()
    with pytest.raises(backup.Refusal):
        backup.protected_directory(existing)

    created = backup.protected_directory(tmp_path / "private")
    assert created.is_dir()
    assert created.stat().st_mode & 0o777 == 0o700


def make_valid_backup(path):
    path.mkdir(mode=0o700)
    for name, content in (("schema.dump", b"database"),):
        (path / name).write_bytes(content)
    for name in ("bundle.tar", "recordings.tar"):
        make_tar(path / name, [("data.bin", "file", name.encode())])

    metadata = {
        "format": 1,
        "source": {"schema": "paper_beta", "project": "source-project"},
        "database_image": "sha256:" + "a" * 64,
        "archive_helper_image": "sha256:" + "b" * 64,
        "relations": {"accounts": {"kind": "r", "rows": 1, "bytes": 1, "sha256": "c"}},
        "files": {
            name: backup.archive_inventory(path / (name + ".tar"))
            for name in ("bundle", "recordings")
        },
    }
    (path / "metadata.json").write_text(json.dumps(metadata), encoding="utf-8")
    manifest = {
        "format": 1,
        "complete": True,
        "sha256": {
            name: backup.digest(path / name)
            for name in ("metadata.json", "schema.dump", "bundle.tar", "recordings.tar")
        },
    }
    (path / "manifest.json").write_text(json.dumps(manifest), encoding="utf-8")
    for item in path.iterdir():
        item.chmod(0o600)
    path.chmod(0o700)
    return path


def test_verified_backup_accepts_consistent_metadata_and_archives(tmp_path):
    path = make_valid_backup(tmp_path / "backup")

    verified, metadata = backup.verified_backup(path)
    assert verified == path.resolve()
    assert metadata["database_image"] == "sha256:" + "a" * 64
    assert metadata["files"]["bundle"]["data.bin"]["kind"] == "file"


def test_verified_backup_refuses_checksum_permission_and_hardlink_violations(tmp_path):
    checksum_bad = make_valid_backup(tmp_path / "checksum")
    (checksum_bad / "schema.dump").write_bytes(b"changed")
    with pytest.raises(backup.Refusal, match="checksum"):
        backup.verified_backup(checksum_bad)

    permission_bad = make_valid_backup(tmp_path / "permission")
    (permission_bad / "schema.dump").chmod(0o644)
    with pytest.raises(backup.Refusal, match="owner-only"):
        backup.verified_backup(permission_bad)

    hardlink_bad = make_valid_backup(tmp_path / "hardlink")
    os.link(hardlink_bad / "schema.dump", hardlink_bad / "linked-copy")
    with pytest.raises(backup.Refusal, match="unlinked"):
        backup.verified_backup(hardlink_bad)


def test_restore_refuses_source_project_before_contacting_docker(tmp_path, monkeypatch):
    path = make_valid_backup(tmp_path / "backup")

    def fail_if_docker_is_constructed(_context):
        pytest.fail("source-project refusal must happen before any Docker access")

    monkeypatch.setattr(backup, "Docker", fail_if_docker_is_constructed)
    args = SimpleNamespace(
        backup=str(path),
        project="source-project",
        context="default",
        destination=str(tmp_path / "restore"),
    )

    with pytest.raises(backup.Refusal, match="differ from source"):
        backup.restore(args)

    assert not (tmp_path / "restore").exists()


def test_archive_helper_dereferences_regular_hardlinks(tmp_path):
    source = tmp_path / "source"
    source.mkdir()
    (source / "first").write_bytes(b"same inode")
    os.link(source / "first", source / "second")
    script = backup.ARCHIVE.replace("Path('/source')", "Path(" + repr(str(source)) + ")")
    result = subprocess.run([sys.executable, "-c", script], capture_output=True)
    assert result.returncode == 0
    with tarfile.open(fileobj=io.BytesIO(result.stdout)) as archive:
        members = archive.getmembers()
        assert {item.name for item in members} == {"first", "second"}
        assert all(item.isfile() for item in members)
        assert all(archive.extractfile(item).read() == b"same inode" for item in members)
