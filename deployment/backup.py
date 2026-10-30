#!/usr/bin/env python3
"""Offline logical backup and isolated restore. Python standard library only."""

import argparse
import datetime
import hashlib
import json
import os
import re
import secrets
import stat
import subprocess
import sys
import tarfile
import tempfile
import time
from pathlib import Path


class Refusal(Exception):
    pass


def require(condition, message):
    if not condition:
        raise Refusal(message)


def run(argv, *, output=None, input_file=None):
    # Docker/PG diagnostics can include credentials: never relay stderr or argv.
    result = subprocess.run(
        argv,
        stdin=input_file,
        stdout=output or subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )
    require(
        result.returncode == 0,
        "Docker/database operation failed; diagnostics withheld. Inspect the selected engine privately.",
    )
    return result.stdout if output is None else None


class Docker:
    def __init__(self, context):
        self.base = ["docker", "--context", context]

    def call(self, *args, **kwargs):
        return run(self.base + list(args), **kwargs)

    def items(self, kind, *filters):
        args = [kind, "ls", "-q"]
        if kind == "container":
            args.append("-a")
        for value in filters:
            args += ["--filter", value]
        ids = self.call(*args).decode().split()
        return json.loads(self.call(kind, "inspect", *ids)) if ids else []


def protected_directory(path):
    path = Path(path).absolute()
    require(not path.exists() and not path.is_symlink(), "Destination must be a new directory.")
    path.mkdir(mode=0o700)  # No parents: operator explicitly chooses an existing parent.
    directory_fd = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(directory_fd)
    finally:
        os.close(directory_fd)
    return path.resolve()


def write_json(path, value):
    with path.open("x", encoding="utf-8") as stream:
        json.dump(value, stream, indent=2, sort_keys=True)
        stream.write("\n")
        stream.flush()
        os.fsync(stream.fileno())


def digest(path):
    value = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            value.update(block)
    return value.hexdigest()


def schema_name(value):
    require(
        value
        and re.fullmatch(r"[a-z_][a-z0-9_]{0,62}", value)
        and value != "public"
        and not value.startswith("pg_"),
        "Schema must be an explicit ordinary application identifier (not public/pg_*).",
    )
    return value


def project_name(value):
    require(
        value and re.fullmatch(r"[a-z0-9][a-z0-9_-]{0,79}", value),
        "Supply a valid explicit project name.",
    )
    return value


def service(containers, name):
    matches = [
        item
        for item in containers
        if item["Config"]["Labels"].get("com.docker.compose.service") == name
        and item["Config"]["Labels"].get("com.docker.compose.oneoff", "false").lower() != "true"
    ]
    require(
        len(matches) == 1,
        "Source requires exactly one retained container for service " + name + ".",
    )
    return matches[0]


def volume_at(container, destination):
    matches = [item for item in container["Mounts"] if item["Destination"] == destination]
    require(
        len(matches) == 1 and matches[0]["Type"] == "volume",
        "Expected named source volume is missing.",
    )
    return matches[0]["Name"]


def source_state(docker, project):
    containers = docker.items("container", "label=com.docker.compose.project=" + project)
    paper, db = service(containers, "paper"), service(containers, "db")
    require(
        db["State"].get("Running") and db["State"].get("Health", {}).get("Status") == "healthy",
        "Source database must be running and healthy.",
    )
    for item in containers:
        if item["Config"]["Labels"].get("com.docker.compose.service") in ("paper", "train"):
            require(
                not item["State"].get("Running") and not item["State"].get("Restarting"),
                "Stop all source paper/train writers first; this command never stops them.",
            )
    volumes = {
        "bundle": volume_at(paper, "/bundle"),
        "recordings": volume_at(paper, "/recordings"),
        "database": volume_at(db, "/var/lib/postgresql"),
    }
    require(len(set(volumes.values())) == 3, "Source volumes must be distinct.")
    # Check other Compose projects and standalone containers too.
    for item in docker.items("container"):
        if item["Id"] == db["Id"] or not (
            item["State"].get("Running") or item["State"].get("Restarting")
        ):
            continue
        require(
            not any(m.get("Name") in volumes.values() and m.get("RW") for m in item["Mounts"]),
            "A running container can write a retained source volume; stop it first.",
        )
    return containers, paper, db, volumes


def sql(docker, db_id, statement):
    return (
        docker.call(
            "exec",
            db_id,
            "psql",
            "-X",
            "-qAt",
            "-v",
            "ON_ERROR_STOP=1",
            "-U",
            "liquiditylab",
            "-d",
            "liquiditylab",
            "-c",
            statement,
        )
        .decode()
        .strip()
    )


def account_inventory(docker, db_id, schema):
    return json.loads(
        sql(
            docker,
            db_id,
            f"SELECT coalesce(json_agg(row_to_json(a) ORDER BY tenant_id, account_id), '[]') FROM "
            f"(SELECT tenant_id, account_id, revision, instrument_revision, md5(snapshot::text) AS snapshot_md5 "
            f'FROM "{schema}".accounts) a;',
        )
    )


def database_inventory(docker, db_id, schema):
    # Hash canonical rows outside PostgreSQL; no extension or unbounded capture.
    # Include receipts, journals and sequences, not just account snapshots.
    relations = json.loads(
        sql(
            docker,
            db_id,
            "SELECT coalesce(json_agg(json_build_object('name', c.relname, 'kind', c.relkind) "
            "ORDER BY c.relname), '[]') FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace "
            f"WHERE n.nspname='{schema}' AND c.relkind IN ('r','S');",
        )
    )
    result = {}
    for relation in relations:
        name = relation["name"]
        require(
            re.fullmatch(r"[a-z_][a-z0-9_]{0,62}", name), "Unsupported application relation name."
        )
        table = f'"{schema}"."{name}"'
        rows = (
            f"SELECT last_value,is_called FROM {table}"
            if relation["kind"] == "S"
            else f"SELECT * FROM {table}"
        )
        statement = f'COPY (SELECT to_jsonb(t)::text AS row FROM ({rows}) t ORDER BY to_jsonb(t)::text COLLATE "C") TO STDOUT;'
        with tempfile.TemporaryFile() as stream:
            docker.call(
                "exec",
                db_id,
                "psql",
                "-X",
                "-qAt",
                "-v",
                "ON_ERROR_STOP=1",
                "-U",
                "liquiditylab",
                "-d",
                "liquiditylab",
                "-c",
                statement,
                output=stream,
            )
            stream.seek(0)
            sha, count, size = hashlib.sha256(), 0, 0
            for block in iter(lambda: stream.read(1024 * 1024), b""):
                sha.update(block)
                count += block.count(b"\n")
                size += len(block)
        result[name] = {
            "kind": relation["kind"],
            "rows": count,
            "bytes": size,
            "sha256": sha.hexdigest(),
        }
    require(result, "Application schema has no relations.")
    return result


def archive_inventory(path):
    result, seen = {}, set()
    with tarfile.open(path, "r:") as archive:
        for item in archive:
            pure = Path(item.name)
            require(
                item.name
                and pure.parts
                and not pure.is_absolute()
                and ".." not in pure.parts
                and "." not in pure.parts
                and pure.as_posix() == item.name
                and "\\" not in item.name
                and item.name not in seen
                and (item.isfile() or item.isdir()),
                "Archive contains unsafe paths or nonordinary entries.",
            )
            seen.add(item.name)
            value = {
                "kind": "directory" if item.isdir() else "file",
                "uid": item.uid,
                "gid": item.gid,
                "mode": item.mode & 0o777,
            }
            if item.isfile():
                sha = hashlib.sha256()
                with archive.extractfile(item) as stream:
                    for block in iter(lambda: stream.read(1024 * 1024), b""):
                        sha.update(block)
                value.update(size=item.size, sha256=sha.hexdigest())
            result[item.name] = value
    return result


# Helpers run exact local source images, with no image pull, network, secrets or app entrypoint.
ARCHIVE = r"""
import os, stat, sys, tarfile
from pathlib import Path
root = Path('/source')
with tarfile.open(fileobj=sys.stdout.buffer, mode='w|', dereference=True) as archive:
    for directory, dirs, files in os.walk(root, followlinks=False):
        for name in sorted(dirs + files):
            path = Path(directory) / name
            mode = path.lstat().st_mode
            if stat.S_ISSOCK(mode):
                continue
            if not (stat.S_ISREG(mode) or stat.S_ISDIR(mode)):
                raise SystemExit('unsupported source entry')
            archive.add(path, arcname=path.relative_to(root).as_posix(), recursive=False)
"""

EXTRACT = r"""
import os, sys, tarfile
from pathlib import Path, PurePosixPath
root = Path('/target')
if any(root.iterdir()):
    raise SystemExit('target volume is not empty')
directories = []
seen = set()
with tarfile.open(fileobj=sys.stdin.buffer, mode='r|') as archive:
    for item in archive:
        name = PurePosixPath(item.name)
        if (not item.name or not name.parts or name.is_absolute() or '..' in name.parts or '.' in name.parts
            or str(name) != item.name or chr(92) in item.name or item.name in seen
            or not (item.isfile() or item.isdir())):
            raise SystemExit('unsafe archive member')
        seen.add(item.name)
        path = root.joinpath(*name.parts)
        path.parent.mkdir(parents=True, exist_ok=True)
        if item.isdir():
            path.mkdir(exist_ok=True)
            directories.append((path, item))
        else:
            with path.open('xb') as output, archive.extractfile(item) as source:
                import shutil
                shutil.copyfileobj(source, output)
            os.chown(path, item.uid, item.gid)
            os.chmod(path, item.mode & 0o777)
            os.utime(path, (item.mtime, item.mtime))
for path, item in reversed(directories):
    os.chown(path, item.uid, item.gid)
    os.chmod(path, item.mode & 0o777)
    os.utime(path, (item.mtime, item.mtime))
os.chown(root, 10001, 10001)
os.chmod(root, 0o755)
"""

FILE_INVENTORY = r"""
import hashlib, json, os, stat
from pathlib import Path
root = Path('/source')
result = {}
for directory, dirs, files in os.walk(root, followlinks=False):
    for name in dirs + files:
        path = Path(directory) / name
        info = path.lstat()
        if not (stat.S_ISREG(info.st_mode) or stat.S_ISDIR(info.st_mode)):
            raise SystemExit('nonordinary restored entry')
        value = {'kind': 'directory' if stat.S_ISDIR(info.st_mode) else 'file',
                 'uid': info.st_uid, 'gid': info.st_gid, 'mode': info.st_mode & 0o777}
        if stat.S_ISREG(info.st_mode):
            sha = hashlib.sha256()
            with path.open('rb') as stream:
                for block in iter(lambda: stream.read(1024 * 1024), b''):
                    sha.update(block)
            value.update(size=info.st_size, sha256=sha.hexdigest())
        result[path.relative_to(root).as_posix()] = value
print(json.dumps(result, sort_keys=True))
"""


def helper(docker, image, volume, script, *, archive=None, output=None):
    args = [
        "run",
        "--rm",
        "--pull=never",
        "--network=none",
        "--read-only",
        "--memory=512m",
        "--cpus=1",
        "--pids-limit=64",
        "--security-opt=no-new-privileges",
        "--cap-drop=ALL",
        "--user",
        "0:0" if archive else "10001:10001",
    ]
    if archive:
        args += ["--cap-add=CHOWN", "--cap-add=DAC_OVERRIDE", "--cap-add=FOWNER", "-i"]
    args += [
        "--mount",
        "type=volume,src=" + volume + ",dst=" + ("/target" if archive else "/source,readonly"),
        "--entrypoint",
        "python3",
        image,
        "-c",
        script,
    ]
    return docker.call(*args, input_file=archive, output=output)


def backup(args):
    project, schema = project_name(args.project), schema_name(args.schema)
    docker = Docker(args.context)
    containers, paper, db, volumes = source_state(docker, project)
    require(
        sql(docker, db["Id"], "SHOW server_version;").startswith("18.6"),
        "Expected PostgreSQL 18.6 source.",
    )
    inventory = account_inventory(docker, db["Id"], schema)
    tables = database_inventory(docker, db["Id"], schema)
    destination = protected_directory(args.destination)
    with (destination / "schema.dump").open("xb") as output:
        docker.call(
            "exec",
            db["Id"],
            "pg_dump",
            "-U",
            "liquiditylab",
            "-d",
            "liquiditylab",
            "--format=custom",
            "--strict-names",
            "--schema=" + schema,
            output=output,
        )
    for name in ("bundle", "recordings"):
        with (destination / (name + ".tar")).open("xb") as output:
            helper(docker, paper["Image"], volumes[name], ARCHIVE, output=output)
    # Refuse a completed backup if a writer was restarted during collection.
    _, current_paper, current_db, current_volumes = source_state(docker, project)
    require(
        current_paper["Id"] == paper["Id"]
        and current_db["Id"] == db["Id"]
        and current_volumes == volumes,
        "Source containers/volumes changed during backup.",
    )
    require(
        account_inventory(docker, db["Id"], schema) == inventory,
        "Source accounts changed during backup.",
    )
    require(
        database_inventory(docker, db["Id"], schema) == tables,
        "Source database changed during backup.",
    )
    archives = {
        name: archive_inventory(destination / (name + ".tar")) for name in ("bundle", "recordings")
    }
    allowed = {
        "PAPER_MODE",
        "PAPER_SOURCE",
        "PAPER_SCHEMA",
        "PAPER_ACCOUNT",
        "PAPER_DURATION",
        "INITIAL_BASE",
        "INITIAL_QUOTE",
    }
    config = dict(
        pair.split("=", 1)
        for pair in paper["Config"].get("Env", [])
        if pair.split("=", 1)[0] in allowed
    )
    require(
        config.get("PAPER_SCHEMA", "paper_beta") == schema,
        "Requested schema differs from source paper configuration.",
    )
    metadata = {
        "format": 1,
        "created_utc": datetime.datetime.now(datetime.timezone.utc).isoformat(),
        "source": {
            "context": args.context,
            "project": project,
            "schema": schema,
            "database_user": "liquiditylab",
            "database_name": "liquiditylab",
            "volumes": volumes,
        },
        "images": {
            item["Config"]["Labels"].get("com.docker.compose.service"): item["Image"]
            for item in containers
        },
        "archive_helper_image": paper["Image"],
        "database_image": db["Image"],
        "paper_configuration": config,
        "accounts": inventory,
        "relations": tables,
        "files": archives,
        "scope": "Entire application schema and immutable bundle/recordings; no secrets or policy socket.",
        "limitations": "Offline restore only; active-session recovery is a separate gate. Model remains unvalidated.",
    }
    write_json(destination / "metadata.json", metadata)
    files = ("schema.dump", "bundle.tar", "recordings.tar", "metadata.json")
    for name in files:
        with (destination / name).open("rb") as stream:
            os.fsync(stream.fileno())
    write_json(
        destination / ".manifest.pending",
        {
            "format": 1,
            "complete": True,
            "sha256": {name: digest(destination / name) for name in files},
        },
    )
    os.rename(destination / ".manifest.pending", destination / "manifest.json")
    directory_fd = os.open(destination, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(directory_fd)
    finally:
        os.close(directory_fd)
    print("Completed protected backup: " + str(destination))


def verified_backup(path):
    path = Path(path).resolve()
    require(path.is_dir(), "Backup directory is missing.")
    require(stat.S_IMODE(path.stat().st_mode) & 0o077 == 0, "Backup directory must be owner-only.")
    for name in ("manifest.json", "metadata.json", "schema.dump", "bundle.tar", "recordings.tar"):
        item = path / name
        require(
            item.is_file() and not item.is_symlink() and item.stat().st_nlink == 1,
            "Backup requires ordinary, unlinked files.",
        )
        require(stat.S_IMODE(item.stat().st_mode) & 0o077 == 0, "Backup files must be owner-only.")
    manifest = json.loads((path / "manifest.json").read_text())
    require(
        manifest.get("format") == 1
        and manifest.get("complete") is True
        and set(manifest.get("sha256", {}))
        == {"metadata.json", "schema.dump", "bundle.tar", "recordings.tar"},
        "Backup has no supported completion manifest.",
    )
    for name, expected in manifest["sha256"].items():
        require(digest(path / name) == expected, "Backup checksum verification failed.")
    metadata = json.loads((path / "metadata.json").read_text())
    require(metadata.get("format") == 1, "Unsupported backup metadata format.")
    schema_name(metadata["source"]["schema"])
    for key in ("database_image", "archive_helper_image"):
        require(
            re.fullmatch(r"sha256:[a-f0-9]{64}", metadata[key]),
            "Backup requires exact source image IDs.",
        )
    require(
        metadata.get("relations") and set(metadata.get("files", {})) == {"bundle", "recordings"},
        "Backup lacks full database/file verification metadata.",
    )
    for name in ("bundle", "recordings"):
        require(
            archive_inventory(path / (name + ".tar")) == metadata["files"][name],
            "Archive file inventory differs from metadata.",
        )
    return path, metadata


def target_absent(docker, project, names):
    require(
        not docker.items("container", "label=com.docker.compose.project=" + project),
        "Target project already has containers.",
    )
    require(
        not docker.items("volume", "label=com.docker.compose.project=" + project),
        "Target project already has volumes.",
    )
    require(
        not docker.items("network", "label=com.docker.compose.project=" + project),
        "Target project already has networks.",
    )
    existing = {item["Name"] for item in docker.items("volume")}
    require(not existing.intersection(names), "A target volume already exists.")
    require(
        not docker.items("container", "name=^/" + project + "-db-1$"),
        "Target database container already exists.",
    )


def restore(args):
    source, metadata = verified_backup(args.backup)
    project = project_name(args.project)
    require(
        project != metadata["source"]["project"], "Target project must differ from source project."
    )
    docker = Docker(args.context)
    names = {name: project + "_" + name for name in ("database", "bundle", "recordings")}
    target_absent(docker, project, names.values())
    for key in ("database_image", "archive_helper_image"):
        require(
            json.loads(docker.call("image", "inspect", metadata[key]))[0]["Id"] == metadata[key],
            "Exact source image is unavailable on the selected engine.",
        )
    destination = Path(args.destination).resolve(strict=False)
    require(
        source != destination and source not in destination.parents,
        "Restore destination must be outside the backup.",
    )
    destination = protected_directory(destination)
    (destination / "postgres_password").write_text(secrets.token_hex(32) + "\n")
    spec = {
        "name": project,
        "services": {
            "db": {
                "image": metadata["database_image"],
                "pull_policy": "never",
                "network_mode": "none",
                "restart": "no",
                "mem_limit": "512m",
                "cpus": 1,
                "pids_limit": 128,
                "environment": {
                    "POSTGRES_USER": "liquiditylab",
                    "POSTGRES_DB": "liquiditylab",
                    "POSTGRES_PASSWORD_FILE": "/run/secrets/postgres_password",
                },
                "secrets": ["postgres_password"],
                "volumes": ["database:/var/lib/postgresql"],
                "healthcheck": {
                    "test": ["CMD", "pg_isready", "-U", "liquiditylab", "-d", "liquiditylab"],
                    "interval": "2s",
                    "timeout": "3s",
                    "retries": 60,
                },
            }
        },
        "volumes": {name: {"name": volume} for name, volume in names.items()},
        "secrets": {"postgres_password": {"file": "./postgres_password"}},
    }
    write_json(destination / "compose.json", spec)
    target_absent(docker, project, names.values())
    compose = ["compose", "--project-name", project, "--file", str(destination / "compose.json")]
    docker.call(*compose, "up", "--detach", "--no-deps", "--no-build", "--pull", "never", "db")
    db_id = docker.call(*compose, "ps", "-q", "db").decode().strip()
    require(db_id, "New target database was not created.")
    for _ in range(90):
        state = json.loads(docker.call("container", "inspect", db_id))[0]["State"]
        if state.get("Health", {}).get("Status") == "healthy":
            break
        require(state.get("Running"), "New target database stopped before initialization.")
        time.sleep(1)
    else:
        raise Refusal("New target database did not become healthy in 90 seconds.")
    require(
        sql(
            docker,
            db_id,
            "SELECT count(*) FROM pg_namespace WHERE nspname !~ '^pg_' AND nspname NOT IN ('public','information_schema');",
        )
        == "0"
        and sql(
            docker,
            db_id,
            "SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public';",
        )
        == "0",
        "Target database is not empty; restore refused.",
    )
    for name in ("bundle", "recordings"):
        # Compose only creates volumes used by db; reserve the other fresh volumes explicitly.
        require(
            not docker.items("volume", "name=^" + names[name] + "$"),
            "Target file volume appeared during restore.",
        )
        docker.call(
            "volume",
            "create",
            "--label",
            "com.docker.compose.project=" + project,
            "--label",
            "com.docker.compose.volume=" + name,
            names[name],
        )
        with (source / (name + ".tar")).open("rb") as archive:
            helper(docker, metadata["archive_helper_image"], names[name], EXTRACT, archive=archive)
    with (source / "schema.dump").open("rb") as dump:
        docker.call(
            "exec",
            "-i",
            db_id,
            "pg_restore",
            "-U",
            "liquiditylab",
            "-d",
            "liquiditylab",
            "--exit-on-error",
            "--no-owner",
            "--no-privileges",
            input_file=dump,
        )
    inventory = account_inventory(docker, db_id, metadata["source"]["schema"])
    require(
        inventory == metadata["accounts"], "Restored account inventory differs from the backup."
    )
    relations = database_inventory(docker, db_id, metadata["source"]["schema"])
    require(
        relations == metadata["relations"],
        "Restored database rows or sequences differ from the backup.",
    )
    for name in ("bundle", "recordings"):
        restored = json.loads(
            helper(docker, metadata["archive_helper_image"], names[name], FILE_INVENTORY)
        )
        require(
            restored == metadata["files"][name],
            "Restored file contents or permissions differ from the backup.",
        )
    write_json(
        destination / "restore.json",
        {
            "format": 1,
            "offline_restore_complete": True,
            "completed_utc": datetime.datetime.now(datetime.timezone.utc).isoformat(),
            "target_context": args.context,
            "target_project": project,
            "target_database_container": db_id,
            "target_volumes": names,
            "backup_manifest_sha256": digest(source / "manifest.json"),
            "accounts": inventory,
            "verified_relations": relations,
            "verified_file_volumes": ["bundle", "recordings"],
            "limitations": "Only isolated DB/files restored. No app started, network, published port or recovery claim. Model remains unvalidated.",
        },
    )
    print("Completed isolated offline restore: " + str(destination))


def main():
    os.umask(0o077)
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    for name in ("backup", "restore"):
        command = sub.add_parser(name)
        command.add_argument("--context", default=os.environ.get("DOCKER_CONTEXT", "default"))
        command.add_argument(
            "--project",
            default=os.environ.get("COMPOSE_PROJECT_NAME"),
            help="Explicit source/target Compose project; restore must use a unique new name",
        )
        command.add_argument(
            "--destination", required=True, help="New directory; parent must already exist"
        )
        if name == "backup":
            command.add_argument("--schema", default=os.environ.get("PAPER_SCHEMA"))
        else:
            command.add_argument("--backup", required=True)
    args = parser.parse_args()
    try:
        (backup if args.command == "backup" else restore)(args)
    except (Refusal, OSError, ValueError, KeyError, tarfile.TarError) as error:
        # Never print arbitrary exception text: filesystem/JSON/Docker inputs may be sensitive.
        message = (
            str(error)
            if isinstance(error, Refusal)
            else "Invalid input or local operation failed; details withheld."
        )
        print("Refused/incomplete: " + message, file=sys.stderr)
        print(
            "Partial artifacts are retained. No automatic volume/container deletion or source shutdown was performed.",
            file=sys.stderr,
        )
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
