#!/usr/bin/env bash
# Installs the crossplane CLI into $RUNNER_TEMP/bin and verifies it against a
# pinned checksum. Bump the version and the checksum together. The CLI is
# released on its own (github.com/crossplane/cli) from cli.crossplane.io.
set -euo pipefail

version="v2.4.1"
sha256="ad0661eed7d2c0b20f5a0f8217e54755d6808ad7f6960660326d761338f311e0"

dir="${RUNNER_TEMP:?}/bin"
mkdir -p "$dir"
curl -fsSL -o "$dir/crossplane" "https://cli.crossplane.io/stable/${version}/bin/linux_amd64/crossplane"
echo "${sha256}  $dir/crossplane" | sha256sum --check --strict
chmod +x "$dir/crossplane"
echo "$dir" >> "$GITHUB_PATH"
"$dir/crossplane" version --client
