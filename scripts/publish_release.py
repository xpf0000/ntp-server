"""Publish missing NTP binaries and make SHA256SUMS match the final assets."""

import argparse
import hashlib
import json
import re
import subprocess
import tempfile
from pathlib import Path

ARCHIVES = tuple(
    f"ntp-server-{os}-{arch}.zip"
    for os in ("darwin", "linux", "windows")
    for arch in ("amd64", "arm64")
)
CHECKSUMS = "SHA256SUMS"


def gh(*args):
    return subprocess.run(
        ["gh", *args], check=True, text=True, capture_output=True
    ).stdout


def release_info(tag):
    return json.loads(gh("release", "view", tag, "--json", "isDraft,assets"))


def download(tag, name, directory):
    gh("release", "download", tag, "--pattern", name, "--dir", str(directory))
    return directory / name


def parse_checksums(path):
    checksums = {}
    for line in path.read_text(encoding="utf-8").splitlines():
        match = re.fullmatch(r"([a-fA-F0-9]{64}) [ *](.+)", line)
        if not match or match[2] in checksums:
            raise RuntimeError("existing SHA256SUMS has invalid entries")
        checksums[match[2]] = match[1].lower()
    return checksums


def publish(tag, dist):
    expected = {*ARCHIVES, CHECKSUMS}
    for archive in ARCHIVES:
        if not (dist / archive).is_file():
            raise RuntimeError(f"built archive is missing: {archive}")

    try:
        state = release_info(tag)
    except subprocess.CalledProcessError as error:
        if "release not found" not in error.stderr.lower() and "HTTP 404" not in error.stderr:
            raise
        gh("release", "create", tag, "--verify-tag", "--generate-notes", "--draft")
        state = {"isDraft": True, "assets": []}

    existing = {asset["name"]: asset["size"] for asset in state["assets"]}
    empty = [name for name in expected if existing.get(name) == 0]
    if empty:
        raise RuntimeError(f"release contains empty assets: {', '.join(sorted(empty))}")

    if expected <= existing.keys():
        print(f"{tag} already contains all seven release assets")
    else:
        with tempfile.TemporaryDirectory() as temporary:
            scratch = Path(temporary)
            sums = {}
            missing = []
            for name in ARCHIVES:
                if name in existing:
                    path = download(tag, name, scratch)
                else:
                    path = dist / name
                    missing.append(str(path))
                sums[name] = hashlib.sha256(path.read_bytes()).hexdigest()

            checksum_path = scratch / CHECKSUMS
            if CHECKSUMS in existing:
                if parse_checksums(download(tag, CHECKSUMS, scratch)) != sums:
                    raise RuntimeError("existing SHA256SUMS does not match the final archive bytes")
            else:
                checksum_path.write_text(
                    "".join(f"{digest}  {name}\n" for name, digest in sums.items()),
                    encoding="utf-8",
                    newline="\n",
                )

            if missing:
                gh("release", "upload", tag, *missing)
            if CHECKSUMS not in existing:
                gh("release", "upload", tag, str(checksum_path))

        uploaded = {asset["name"] for asset in release_info(tag)["assets"] if asset["size"] > 0}
        if expected - uploaded:
            raise RuntimeError(f"release is missing assets: {', '.join(sorted(expected - uploaded))}")
        print(f"Verified all seven release assets for {tag}")

    if state["isDraft"]:
        gh("release", "edit", tag, "--draft=false")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("tag")
    parser.add_argument("dist", type=Path)
    args = parser.parse_args()
    try:
        publish(args.tag, args.dist)
    except (RuntimeError, subprocess.CalledProcessError) as error:
        parser.exit(1, f"{getattr(error, 'stderr', None) or error}\n")
