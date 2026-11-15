#!/usr/bin/env bash
# Installs the crossplane CLI into $RUNNER_TEMP/bin and verifies it against a
# pinned checksum. Bump the version and the checksum together.
set -euo pipefail

version="v2.4.3"
sha256="44593fe59c011cd4c4f421b2c8d7a6acc57ca2b80204a7f4fbbb253002732fd2"

dir="${RUNNER_TEMP:?}/bin"
mkdir -p "$dir"
curl -fsSL -o "$dir/crossplane" "https://releases.crossplane.io/stable/${version}/bin/linux_amd64/crossplane"
echo "${sha256}  $dir/crossplane" | sha256sum --check --strict
chmod +x "$dir/crossplane"
echo "$dir" >> "$GITHUB_PATH"
"$dir/crossplane" version --client
