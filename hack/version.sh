#!/usr/bin/env bash
# KubeSwift version helper.
# Usage: source hack/version.sh  OR  eval $(hack/version.sh)
# Exports: VERSION, VERSION_TAG, GIT_COMMIT, GIT_COMMIT_SHORT, IMAGE_TAG, CHART_VERSION

set -e

GIT_COMMIT=$(git rev-parse HEAD 2>/dev/null || echo "unknown")
GIT_COMMIT_SHORT=$(echo "$GIT_COMMIT" | cut -c1-7)
GIT_TAG=$(git describe --tags --exact-match 2>/dev/null || true)

if [[ -n "$GIT_TAG" ]]; then
  if [[ "$GIT_TAG" == *"-rc"* ]]; then
    # Release candidate: v0.1.0-rc.1
    VERSION="${GIT_TAG#v}"
    VERSION_TAG="$GIT_TAG"
    IMAGE_TAG="$GIT_TAG"
    CHART_VERSION="$VERSION"
  else
    # Stable: v0.1.0
    VERSION="${GIT_TAG#v}"
    VERSION_TAG="$GIT_TAG"
    IMAGE_TAG="$GIT_TAG"
    CHART_VERSION="$VERSION"
  fi
else
  # Dev (no tag): 0.0.0-dev.<shortsha>
  # SemVer §9 leading-zero guard: only ~0.4% of commits hit this
  # (all-digit hash starting with 0). Mirror the same conditional
  # the release-dev workflow applies; otherwise local computation
  # would diverge from published chart versions.
  if [[ "$GIT_COMMIT_SHORT" =~ ^0[0-9]{6}$ ]]; then
    VERSION="0.0.0-dev.g${GIT_COMMIT_SHORT}"
  else
    VERSION="0.0.0-dev.${GIT_COMMIT_SHORT}"
  fi
  VERSION_TAG="v${VERSION}"
  IMAGE_TAG="sha-${GIT_COMMIT_SHORT}"
  CHART_VERSION="$VERSION"
fi

export VERSION VERSION_TAG GIT_COMMIT GIT_COMMIT_SHORT IMAGE_TAG CHART_VERSION

# Emit for eval or export
echo "export VERSION=\"$VERSION\""
echo "export VERSION_TAG=\"$VERSION_TAG\""
echo "export GIT_COMMIT=\"$GIT_COMMIT\""
echo "export GIT_COMMIT_SHORT=\"$GIT_COMMIT_SHORT\""
echo "export IMAGE_TAG=\"$IMAGE_TAG\""
echo "export CHART_VERSION=\"$CHART_VERSION\""
