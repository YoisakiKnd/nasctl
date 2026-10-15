#!/usr/bin/env bash
# Explicit development/CI Linux verification. Uses only a unique /run unit and
# generated originals, leaves its private proof for inspection, stops on exit.
set -euo pipefail
if [[ ${1:-} != --fixture-only || $# != 2 || $(uname -s) != Linux || $(id -u) == 0 ]]; then
  echo 'usage (non-root, isolated Linux VM/CI): verify-native-album-linux.sh --fixture-only ABSOLUTE_BINARY_DIRECTORY' >&2
  exit 2
fi
nasctl_binary_dir=$2
if [[ $nasctl_binary_dir != /* ]]; then exit 2; fi
nasctl_repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd -- "$nasctl_repo_root"
nasctl_source_fixture=$(mktemp -d /tmp/nasctl-kernel-source-XXXXXXXX)
chmod 700 "$nasctl_source_fixture"
go run ./internal/synth/cmdgen "$nasctl_source_fixture/synth"
printf 'nasctl-kernel-source-v1\n' > "$nasctl_source_fixture/.nasctl-source-fixture"
nasctl_kernel_fixture=$(sudo mktemp -d /run/nasctl-kernel-album-XXXXXXXX)
sudo chown "$(id -u):$(id -g)" "$nasctl_kernel_fixture"
python3 scripts/uitest/seed-kernel-album.py "$nasctl_kernel_fixture" --source "$nasctl_source_fixture" --binary-dir "$nasctl_binary_dir" --fixture-only
nasctl_kernel_unit="$(basename "$nasctl_kernel_fixture").service"
trap 'sudo systemctl stop "$nasctl_kernel_unit" 2>/dev/null || true' EXIT
sudo cp "$nasctl_kernel_fixture/unit.service" "/run/systemd/system/$nasctl_kernel_unit"
sudo systemd-analyze verify "/run/systemd/system/$nasctl_kernel_unit"
sudo systemctl daemon-reload
sudo systemctl start "$nasctl_kernel_unit"
python3 scripts/uitest/kernel-album.py "$nasctl_kernel_fixture" --fixture-only
sudo systemctl show "$nasctl_kernel_unit" -p ControlGroup -p MemoryMax -p MemoryHigh -p Delegate -p ProtectSystem -p ProtectControlGroups > "$nasctl_kernel_fixture/unit-proof.txt"
echo "Native album kernel proof retained at $nasctl_kernel_fixture"
