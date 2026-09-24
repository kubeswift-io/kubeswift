#!/usr/bin/env bash
# Fail when a Containerfile's FROM names a base image by tag alone.
#
# A tag is mutable: whoever controls the upstream repository, or its registry,
# decides what `debian:bookworm-slim` is on the day we build, and the release
# signs and attests whatever that turned out to be. swiftletd's base runs
# privileged on every node. Pinned as tag@sha256:<digest>, a build uses the
# image that was reviewed, and a new base arrives as a Dependabot PR (the
# docker ecosystem in .github/dependabot.yml updates tag and digest together)
# that CI builds and scans before it merges.
#
# A FROM that names an earlier stage of the same file (`FROM builder`) is not
# an image and is skipped.
set -euo pipefail

cd "$(dirname "$0")/.."

bad=""
for f in images/*/Containerfile; do
  stages=$(sed -nE 's/^FROM[[:space:]]+.*[[:space:]]+[Aa][Ss][[:space:]]+([^[:space:]]+).*$/\1/p' "$f")
  while IFS= read -r line; do
    ref=$(awk '{for (i = 2; i <= NF; i++) if ($i !~ /^--/) {print $i; exit}}' <<<"${line#*:}")
    if grep -qxF -- "$ref" <<<"$stages"; then
      continue
    fi
    if ! [[ "$ref" =~ ^[^@[:space:]]+:[^@[:space:]]+@sha256:[0-9a-f]{64}$ ]]; then
      bad+="  ${f}:${line%%:*}: ${ref}"$'\n'
    fi
  done < <(grep -nE '^FROM[[:space:]]' "$f")
done

if [ -n "$bad" ]; then
  echo "verify-base-image-pins: base images must be pinned as <image>:<tag>@sha256:<digest>:" >&2
  printf '%s' "$bad" >&2
  exit 1
fi
count=$(grep -hcE '^FROM[[:space:]]' images/*/Containerfile | awk '{s+=$1} END{print s+0}')
echo "verify-base-image-pins: $count FROM lines, every base image digest-pinned"
