#!/usr/bin/env python3
"""Build Mobius CLI binaries for all supported platforms into ./dist."""

from __future__ import annotations

import hashlib
import os
import shutil
import subprocess
import sys
import zipfile
from datetime import datetime, timezone
from pathlib import Path
from typing import NoReturn

PLATFORMS = (
    ("darwin", "arm64"),
    ("darwin", "amd64"),
    ("linux", "amd64"),
    ("linux", "arm64"),
    ("windows", "amd64"),
)


def artifact_name(goos: str, goarch: str) -> str:
    suffix = ".exe" if goos == "windows" else ""
    return f"mobius-{goos}-{goarch}{suffix}"


def release_asset_name(goos: str, goarch: str) -> str:
    if goos == "windows":
        return f"mobius-{goos}-{goarch}.zip"
    return artifact_name(goos, goarch)


def fail(message: str) -> NoReturn:
    print(message, file=sys.stderr)
    raise SystemExit(1)


def run(cmd: list[str], *, cwd: Path | None = None, env: dict[str, str] | None = None) -> None:
    subprocess.run(cmd, cwd=cwd, env=env, check=True)


def capture(cmd: list[str], *, cwd: Path | None = None) -> str:
    result = subprocess.run(cmd, cwd=cwd, check=True, capture_output=True, text=True)
    return result.stdout.strip()


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def format_size(path: Path) -> str:
    size = path.stat().st_size
    units = ("B", "K", "M", "G")
    value = float(size)
    unit = units[0]
    for unit in units:
        if value < 1024 or unit == units[-1]:
            break
        value /= 1024
    if unit == "B":
        return f"{int(value)}{unit}"
    return f"{value:.0f}{unit}"


def main(argv: list[str]) -> int:
    if len(argv) != 2 or not argv[1]:
        fail("Usage: scripts/release.py <version>\nExample: scripts/release.py 0.1.0")

    version = argv[1]
    script_dir = Path(__file__).resolve().parent
    project_dir = script_dir.parent
    dist_dir = project_dir / "dist"

    if dist_dir.exists():
        shutil.rmtree(dist_dir)
    dist_dir.mkdir(parents=True)

    print(f"=== Building Mobius CLI v{version} ===\n")

    commit = capture(["git", "rev-parse", "--short", "HEAD"], cwd=project_dir)
    date = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    ldflags = f"-s -w -X main.version={version} -X main.commit={commit} -X main.date={date}"

    for goos, goarch in PLATFORMS:
        platform = f"{goos}-{goarch}"
        output = dist_dir / artifact_name(goos, goarch)
        print(f">> Building {platform}...")
        env = os.environ.copy()
        env.update({"CGO_ENABLED": "0", "GOOS": goos, "GOARCH": goarch})
        run(
            ["go", "build", "-C", str(project_dir), "-ldflags", ldflags, "-o", str(output), "./cmd/mobius"],
            env=env,
        )
        print(f"   {format_size(output)}")

    windows_exe = dist_dir / artifact_name("windows", "amd64")
    windows_zip = dist_dir / release_asset_name("windows", "amd64")
    print("\n>> Packaging windows-amd64.zip...")
    with zipfile.ZipFile(windows_zip, "w", compression=zipfile.ZIP_DEFLATED) as archive:
        archive.write(windows_exe, arcname="mobius.exe")
    print(f"   {format_size(windows_zip)}")

    print("\n=== SHA-256 Checksums ===")
    checksums_path = dist_dir / "checksums.txt"
    lines: list[str] = []
    for goos, goarch in PLATFORMS:
        filename = release_asset_name(goos, goarch)
        digest = sha256_file(dist_dir / filename)
        lines.append(f"{digest}  {filename}")

    checksums_path.write_text("\n".join(lines) + "\n", encoding="utf-8")
    print(checksums_path.read_text(encoding="utf-8"), end="")
    print(f"\nArtifacts written to {dist_dir}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
