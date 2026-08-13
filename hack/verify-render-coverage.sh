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

# Render through the shared script, NOT a local `helm template` line: it carries
# the --api-versions the capability-guarded templates need, and sharing it with
# the workflow is what stops the two renders drifting apart.
rendered="$("$ROOT/hack/render-lint-profile.sh")"

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

# Capability-guarded objects render to NOTHING unless the shared render script
# passes --api-versions for them, and a security control CI cannot see is worse
# than one that does not exist — it reads as covered. Assert the launcher-SA
# admission gate (#443) is actually in the scanned output.
GUARDED="ValidatingAdmissionPolicy/kubeswift-launcher-sa-gate
ValidatingAdmissionPolicyBinding/kubeswift-launcher-sa-gate"

for want in $GUARDED; do
  kind="${want%%/*}"; name="${want##*/}"
  if ! printf '%s' "$rendered" | grep -q "^kind: ${kind}$"; then
    echo "verify-render-coverage: ${kind} is missing from the render." >&2
    echo "It is capability-guarded — check hack/render-lint-profile.sh passes" >&2
    echo "--api-versions for it, or the policy check is scanning nothing." >&2
    exit 1
  fi
  printf '%s' "$rendered" | grep -q "name: ${name}$" || {
    echo "verify-render-coverage: ${kind} rendered but not named ${name}." >&2
    exit 1
  }
done

echo "verify-render-coverage: OK — $(printf '%s\n' "$actual" | wc -l) workloads + $(printf '%s\n' "$GUARDED" | wc -l) capability-guarded objects rendered and accounted for."
