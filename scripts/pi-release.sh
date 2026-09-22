#!/usr/bin/env bash
# Cross-compiles lanmsg-cli, lanmsg-remote-cli, and lanmsg-server for every
# supported platform, from a plain checkout of this repo — meant to run on
# the Raspberry Pi relay itself (or any machine with Go 1.27+), NOT inside
# the hardened lanmsg-server.service unit: that unit's ProtectSystem=strict
# and ReadWritePaths=/etc/lanmsg /var/lib/lanmsg (deploy/lanmsg-server.service)
# deliberately block a writable source tree or running `go build`/`git` from
# within it. Run this as a normal shell session under your own login instead.
#
# This script only builds. It never signs anything — that's a deliberate
# separation (see cmd/lanmsg-signrelease and docs/DESIGN.md §12.2): the
# signing key must live on a machine kept separate from wherever the build
# ran, so a compromised build machine alone can never produce a binary a
# client will actually trust.
#
# Usage:
#   ./scripts/pi-release.sh [output-dir]
#
# Full release workflow (see docs/SETUP.md's "Updates" section):
#   1. On the Pi (or wherever): git pull, bump internal/version.Version, run
#      this script.
#   2. scp the output directory to your own trusted, separate signing machine.
#   3. There: lanmsg-signrelease sign -key ... -spec ... -prev ... -out-dir ...
#      (re-derives SHA-256 from the bytes it received — never trusts this
#      script's own hashes, since the whole point is not trusting the build
#      machine with signing authority).
#   4. scp the signed manifest.json + manifest.json.sig + artifacts back to
#      the relay's updates directory (cfg.UpdatesDir(), normally
#      /var/lib/lanmsg/updates/ on a systemd install).

set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

out_dir="${1:-out}"
mkdir -p "$out_dir"

echo "== building from $(git rev-parse --short HEAD 2>/dev/null || echo 'unknown commit') =="
if ! git diff --quiet 2>/dev/null; then
  echo "warning: working tree has uncommitted changes; the build below reflects them anyway" >&2
fi

build() {
  local pkg=$1 name=$2 os=$3 arch=$4 goarm=${5:-}
  local ext=""
  [ "$os" = "windows" ] && ext=".exe"
  local out="$out_dir/${name}-${os}-${arch}${goarm:+v$goarm}${ext}"
  echo "  $out"
  GOARM="$goarm" CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
    go build -trimpath -ldflags="-s -w" -o "$out" "./cmd/$pkg"
}

echo "== lanmsg-cli =="
build lanmsg-cli lanmsg-cli linux   amd64
build lanmsg-cli lanmsg-cli linux   arm64
build lanmsg-cli lanmsg-cli linux   arm   7
build lanmsg-cli lanmsg-cli windows amd64
build lanmsg-cli lanmsg-cli darwin  amd64
build lanmsg-cli lanmsg-cli darwin  arm64

echo "== lanmsg-remote-cli =="
build lanmsg-remote-cli lanmsg-remote-cli linux   amd64
build lanmsg-remote-cli lanmsg-remote-cli linux   arm64
build lanmsg-remote-cli lanmsg-remote-cli linux   arm   7
build lanmsg-remote-cli lanmsg-remote-cli windows amd64
build lanmsg-remote-cli lanmsg-remote-cli darwin  amd64
build lanmsg-remote-cli lanmsg-remote-cli darwin  arm64
build lanmsg-remote-cli lanmsg-remote-cli android arm64

echo "== lanmsg-server (for your own manual relay upgrade — not a self-update artifact) =="
build lanmsg-server lanmsg-server linux amd64
build lanmsg-server lanmsg-server linux arm64
build lanmsg-server lanmsg-server linux arm  7

echo
echo "Done. $out_dir/ is ready to copy to your signing machine:"
echo
echo "  scp -r $out_dir youruser@your-desktop:/tmp/lanmsg-release/"
echo
echo "Then on that machine, write a release spec naming which of these"
echo "actually changed (see cmd/lanmsg-signrelease's package doc comment for"
echo "the spec format), and run \`lanmsg-signrelease sign\`."
