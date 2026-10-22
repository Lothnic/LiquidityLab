"""Generate checked-in Go/Python bindings using the pinned codegen extra.

Install protoc-gen-go v1.36.12 and protoc-gen-go-grpc v1.6.2 on PATH, or
set PROTOC_GEN_GO and PROTOC_GEN_GO_GRPC to those executables.
"""

import argparse
import os
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
FILES = (
    "gen/go/liquiditylab/v1/execution.pb.go",
    "gen/go/liquiditylab/v1/execution_grpc.pb.go",
    "src/liquiditylab/v1/execution_pb2.py",
    "src/liquiditylab/v1/execution_pb2_grpc.py",
)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check", action="store_true", help="fail on generated-code drift")
    args = parser.parse_args()
    plugins = {}
    for name, expected in (("go", "v1.36.12"), ("go-grpc", "1.6.2")):
        command = f"protoc-gen-{name}"
        path = os.environ.get(command.upper().replace("-", "_")) or shutil.which(command)
        if path is None:
            parser.error(f"install {command} {expected} or set its executable path")
        reported = subprocess.check_output([path, "--version"], text=True).strip().split()[-1]
        if reported.removeprefix("v") != expected.removeprefix("v"):
            parser.error(f"{command}: expected {expected}, found {reported}")
        plugins[name] = path
    from importlib.metadata import version

    import grpc_tools

    if version("grpcio-tools") != "1.84.1":
        parser.error("codegen requires grpcio-tools==1.84.1; use the locked codegen extra")
    with tempfile.TemporaryDirectory(prefix="liquiditylab-codegen-") as scratch:
        destination = Path(scratch) if args.check else ROOT
        for path in (destination / "gen/go", destination / "src"):
            path.mkdir(parents=True, exist_ok=True)
        command = [sys.executable, "-m", "grpc_tools.protoc", "-I", str(ROOT / "proto")]
        # grpc_tools is imported above to fail with a useful missing-extra error.
        assert grpc_tools.__file__
        for name, plugin in plugins.items():
            command.extend((f"--plugin=protoc-gen-{name}={plugin}",
                            f"--{name}_out={destination / 'gen/go'}",
                            f"--{name}_opt=paths=source_relative"))
        command.extend((f"--python_out={destination / 'src'}",
                        f"--grpc_python_out={destination / 'src'}",
                        "liquiditylab/v1/execution.proto"))
        subprocess.run(command, cwd=ROOT / "proto", check=True)
        if args.check:
            drift = [path for path in FILES if not (ROOT / path).exists()
                     or (ROOT / path).read_bytes() != (destination / path).read_bytes()]
            if drift:
                parser.exit(1, "Generated code differs: " + ", ".join(drift) + "\n")
        print("Protocol bindings match" if args.check else "Protocol bindings generated")


if __name__ == "__main__":
    main()
