#!/usr/bin/env bash
# Installs the crossplane CLI into $RUNNER_TEMP/bin and verifies it against a
# pinned checksum. Bump the version and the checksum together. The CLI is
# released on its own (github.com/crossplane/cli) from cli.crossplane.io.
#
# The bundle is used rather than the bare binary: upstream's published
# checksum for the bare v2.4.1 binary does not match the file it serves,
# while the bundle's does.
set -euo pipefail

version="v2.4.1"
sha256="d533f5475aa1ea94f147b9abc0eda0c9575b4599cdc62f7ef25e496f2629fa07"

dir="${RUNNER_TEMP:?}/bin"
bundle="${RUNNER_TEMP}/crossplane-cli.tar.gz"
mkdir -p "$dir"
curl -fsSL -o "$bundle" "https://cli.crossplane.io/stable/${version}/bundle/linux_amd64/crossplane-cli.tar.gz"
echo "${sha256}  ${bundle}" | sha256sum --check --strict
tar -xzf "$bundle" -C "$dir" crossplane
rm -f "$bundle"
echo "$dir" >> "$GITHUB_PATH"
"$dir/crossplane" version --client
