#!/usr/bin/env bash
# Chart version for a given release type.
# Usage: hack/chart-version.sh [dev|rc|stable]
# Reads GIT_COMMIT_SHORT (dev) or GIT_TAG (rc/stable) from env or git.

set -e

TYPE="${1:-dev}"

case "$TYPE" in
  dev)
    GIT_COMMIT_SHORT="${GIT_COMMIT_SHORT:-$(git rev-parse HEAD | cut -c1-7)}"
    # SemVer §9 leading-zero guard: only ~0.4% of commits hit this
    # (all-digit hash starting with 0). Mirror the same conditional
    # the release-dev workflow applies; otherwise local computation
    # would diverge from published chart versions.
    if [[ "$GIT_COMMIT_SHORT" =~ ^0[0-9]{6}$ ]]; then
      echo "0.0.0-dev.g${GIT_COMMIT_SHORT}"
    else
      echo "0.0.0-dev.${GIT_COMMIT_SHORT}"
    fi
    ;;
  rc|stable)
    GIT_TAG="${GIT_TAG:-$(git describe --tags --exact-match 2>/dev/null)}"
    if [[ -z "$GIT_TAG" ]]; then
      echo "ERROR: not on a git tag" >&2
      exit 1
    fi
    echo "${GIT_TAG#v}"
    ;;
  *)
    echo "Usage: $0 [dev|rc|stable]" >&2
    exit 1
    ;;
esac
