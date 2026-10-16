#!/usr/bin/env bash
# CI/developer packaging on Linux with GNU tar. Nothing here runs on the NAS.
set -euo pipefail
cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.."
nasctl_arch=${1:-}
case "$nasctl_arch" in
  amd64|arm64) nasctl_goarch=$nasctl_arch ;;
  armv7) nasctl_goarch=arm ;;
  *) echo 'usage: package-linux.sh amd64|arm64|armv7' >&2; exit 2 ;;
esac
nasctl_go=${GO:-go}
nasctl_out=${OUT_DIR:-dist}
nasctl_base_version=$(cat VERSION)
[[ $nasctl_base_version =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo 'invalid VERSION' >&2; exit 2; }
nasctl_version="$nasctl_base_version-dev"
if [[ -n ${RELEASE_VERSION:-} ]]; then
  [[ $RELEASE_VERSION == "$nasctl_base_version" ]] || { echo 'RELEASE_VERSION must match VERSION' >&2; exit 2; }
  nasctl_version=$RELEASE_VERSION
fi
nasctl_epoch=${SOURCE_DATE_EPOCH:-$(git show -s --format=%ct HEAD)}
[[ $nasctl_epoch =~ ^[0-9]{1,12}$ ]] || { echo 'invalid SOURCE_DATE_EPOCH' >&2; exit 2; }
nasctl_time=$(date -u -d "@$nasctl_epoch" +%Y-%m-%dT%H:%M:%SZ)
nasctl_commit=$(git rev-parse HEAD)
nasctl_target="linux/$nasctl_goarch"
if [[ $nasctl_arch == armv7 ]]; then nasctl_target='linux/arm/GOARM7'; fi
mkdir -p -- "$nasctl_out"
nasctl_stage=$(mktemp -d "$nasctl_out/.nasctl-package-XXXXXXXX")
trap 'rm -rf -- "$nasctl_stage"' EXIT
nasctl_name="nasctl-linux-$nasctl_arch"
nasctl_package="$nasctl_stage/$nasctl_name"
mkdir -p "$nasctl_package"/{configs,systemd,docs}
nasctl_flags="-s -w -X github.com/YoisakiKnd/nasctl/internal/buildinfo.Version=$nasctl_version -X github.com/YoisakiKnd/nasctl/internal/buildinfo.Commit=$nasctl_commit -X github.com/YoisakiKnd/nasctl/internal/buildinfo.BuildTime=$nasctl_time -X github.com/YoisakiKnd/nasctl/internal/buildinfo.Target=$nasctl_target"
for nasctl_program in nasctl; do
  CGO_ENABLED=0 GOWORK=off GOOS=linux GOARCH="$nasctl_goarch" GOARM=7 \
    "$nasctl_go" build -trimpath -buildvcs=false -ldflags "$nasctl_flags" \
    -o "$nasctl_package/$nasctl_program" "./cmd/$nasctl_program"
  case "$nasctl_arch" in
    amd64) nasctl_elf='ELF 64-bit LSB.*x86-64' ;;
    arm64) nasctl_elf='ELF 64-bit LSB.*aarch64' ;;
    armv7) nasctl_elf='ELF 32-bit LSB.*ARM' ;;
  esac
  file "$nasctl_package/$nasctl_program" | grep -Eq "$nasctl_elf"
  if readelf -d "$nasctl_package/$nasctl_program" | grep -q NEEDED || \
     readelf -l "$nasctl_package/$nasctl_program" | grep -q interpreter; then
    echo "unexpected dynamic dependency: $nasctl_program" >&2; exit 1
  fi
done
cp configs/nasctl.example.toml "$nasctl_package/configs/"
cp systemd/nasctl-web.service systemd/nasctl-album-thumbs.service systemd/nasctl-album-thumbs.timer "$nasctl_package/systemd/"
cp LICENSE "$nasctl_package/"
cp -R third_party/licenses "$nasctl_package/licenses"
for nasctl_doc in DEPLOYMENT UPGRADE FRONTEND AUTH ALBUM ANIME FILES MEDIA BACKUPS STATE-BACKUPS SETTINGS PHOTO-VAULT WEBDAV-BACKUP DESKTOP-APPS ONECLOUD-MEDIA; do
  cp "docs/$nasctl_doc.md" "$nasctl_package/docs/"
done
{
  echo "nasctl $nasctl_version"
  echo "source commit: $nasctl_commit"
  echo "built: $nasctl_time"
  echo "toolchain: $("$nasctl_go" version)"
  echo "target: $nasctl_target, CGO_ENABLED=0, static"
  echo
  echo 'Verify inside this directory: sha256sum -c SHA256SUMS'
  echo 'Install the single nasctl binary; album processing runs as `nasctl album` on demand.'
  echo 'Follow docs/DEPLOYMENT.md for first setup, permissions and upgrades.'
  echo 'Upgrading: read docs/UPGRADE.md. From 0.3.x only the binary changes; from 0.2.x or earlier, albumctl became nasctl album.'
  if [[ -n ${RELEASE_VERSION:-} ]]; then
    echo "Release-version build: $nasctl_version. Publication is controlled by the release workflow."
  else
    echo 'This is a CI development build, not a tagged stable release.'
  fi
  echo 'Cross-compilation is not evidence of real NAS hardware validation.'
} > "$nasctl_package/BUILD.txt"
(
  cd "$nasctl_package"
  find . -type f ! -name SHA256SUMS -print0 | sort -z | xargs -0 sha256sum > SHA256SUMS
  sha256sum -c SHA256SUMS
)
tar --sort=name --mtime="@$nasctl_epoch" --owner=0 --group=0 --numeric-owner \
  -C "$nasctl_stage" -cf - "$nasctl_name" | gzip -n > "$nasctl_out/$nasctl_name.tar.gz"
(
  cd "$nasctl_out"
  sha256sum "$nasctl_name.tar.gz" > "$nasctl_name.tar.gz.sha256"
)
echo "Packaged $nasctl_out/$nasctl_name.tar.gz"
