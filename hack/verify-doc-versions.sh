#!/usr/bin/env bash
# Fail when a docs install command pins a version that is not the chart's.
#
# README.md and eight docs pages sat on `--version 0.13.10` for three releases
# (v0.13.11, .12, .13) because bumping them is a manual step nobody owns, and
# nothing checked. A reader following the quickstart installed a chart three
# releases old and got a CRD without the fields the page described.
#
# WHAT IS CHECKED: pins in commands a reader would copy and run --
#   --version X.Y.Z          the chart version
#   image.tag=vX.Y.Z         an image tag override
#
# WHAT IS NOT: prose stating when something appeared, e.g. "a v0.13.11 field"
# or "spec.importStorageClassName (v0.13.11)". Those are history and are
# CORRECT at their original version -- rewriting them each release would be a
# lie. Neither pattern above matches prose, which is why the check is written
# against the command forms rather than "any version-shaped string".
#
# Placeholders (0.0.0-dev.<sha>, <version>) are ignored: they are illustrative
# by construction and have no single right value.
set -euo pipefail

cd "$(dirname "$0")/.."

chart=$(sed -n 's/^version: \(.*\)$/\1/p' charts/kubeswift/Chart.yaml | head -1)
if [ -z "$chart" ]; then
  echo "verify-doc-versions: could not read version from charts/kubeswift/Chart.yaml" >&2
  exit 1
fi

# Chart pins (--version X.Y.Z) and image pins (image.tag=vX.Y.Z), minus the
# dev-channel placeholder which is deliberately not a real version.
bad=$(grep -rnE -- '--version [0-9]+\.[0-9]+\.[0-9]+|image\.tag=v[0-9]+\.[0-9]+\.[0-9]+' \
        README.md docs/ 2>/dev/null \
      | grep -v '0\.0\.0-dev' \
      | grep -vE -- "--version ${chart//./\\.}([^0-9]|\$)|image\.tag=v${chart//./\\.}([^0-9]|\$)" \
      || true)

if [ -n "$bad" ]; then
  echo "verify-doc-versions: docs pin a version other than the chart's ($chart):" >&2
  echo "$bad" | sed 's/^/  /' >&2
  echo >&2
  echo "Update the install commands to $chart. Do NOT change prose that records" >&2
  echo "when a field appeared (\"a v0.13.11 field\") -- that is history, not a pin." >&2
  exit 1
fi

count=$(grep -rcE -- '--version [0-9]+\.[0-9]+\.[0-9]+|image\.tag=v[0-9]+\.[0-9]+\.[0-9]+' \
          README.md docs/ 2>/dev/null | awk -F: '{s+=$2} END{print s+0}')
echo "verify-doc-versions: $count install pins, all at $chart"
