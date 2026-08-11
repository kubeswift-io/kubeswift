#!/usr/bin/env bash
# verify-render-coverage — fail loudly when the lint profile stops rendering a
# workload we expect to be scanning.
#
# THE FAILURE MODE THIS GUARDS. Nearly every component in this chart is behind
# an `enabled: false` toggle. Rendering with defaults produces one pod-bearing
# workload out of five. So a manifest check that renders the chart and finds
# nothing wrong may simply not be looking at anything — and it reports green
# either way. Renaming a values key, or adding a new required value that makes a
# template fail-closed, silently shrinks coverage without failing anything.
#
# This asserts the set of pod-bearing workloads the lint profile renders is
# EXACTLY the expected set. Adding a component is meant to fail here: that is
# the reminder to decide whether it should be scanned.
#
# Run via:
#   make verify-render-coverage     (developer/CI)
#   ./hack/verify-render-coverage.sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CHART="$ROOT/charts/kubeswift"
VALUES="$ROOT/hack/lint-values/full.yaml"

# The workloads the full lint profile must render. Sorted; compared verbatim.
EXPECTED="DaemonSet/gpu-discovery
DaemonSet/kubeswift-dra-driver
Deployment/controller-manager
Deployment/kubeswift-gateway
Deployment/kubeswift-ui"

command -v helm >/dev/null || { echo "verify-render-coverage: helm not found" >&2; exit 1; }

rendered="$(helm template kubeswift "$CHART" -f "$VALUES")"

# Pod-bearing kinds only — those are what the security policy checks apply to.
actual="$(printf '%s' "$rendered" | awk '
  /^---$/            { kind=""; name=""; next }
  /^kind: /          { kind=$2; next }
  # first metadata.name at indent 2 after the kind line
  /^  name: / && kind != "" && name == "" { name=$2 }
  END {}
  { if (kind != "" && name != "") { print kind "/" name; kind=""; name="" } }
' | grep -E '^(Deployment|DaemonSet|StatefulSet)/' | sort -u)"

if [[ "$actual" != "$EXPECTED" ]]; then
  echo "verify-render-coverage: the lint profile does not render the expected workloads." >&2
  echo >&2
  diff <(printf '%s\n' "$EXPECTED") <(printf '%s\n' "$actual") >&2 || true
  echo >&2
  echo "If a component was added or renamed, update EXPECTED above AND confirm the" >&2
  echo "new workload is covered by hack/lint-values/full.yaml." >&2
  exit 1
fi

echo "verify-render-coverage: OK — $(printf '%s\n' "$actual" | wc -l) workloads rendered and accounted for."
