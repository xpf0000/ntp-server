#!/usr/bin/env bash
set -euo pipefail

bin_dir="${1:?compiled binaries directory is required}"
output_dir="${2:?release output directory is required}"
mkdir -p "$output_dir"

archives=()
for os in darwin linux windows; do
  for arch in amd64 arm64; do
    name="ntp-server-${os}-${arch}"
    binary="$bin_dir/$name"
    if [[ "$os" == windows ]]; then
      binary="$binary.exe"
    fi
    archive="$name.zip"
    python3 -m zipfile -c "$output_dir/$archive" "$binary" LICENSE README.md configs/server.example.json configs/flyenv.example.json
    archives+=("$archive")
  done
done

(cd "$output_dir" && sha256sum "${archives[@]}" > SHA256SUMS)
