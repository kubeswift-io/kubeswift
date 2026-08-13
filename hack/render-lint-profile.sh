#!/usr/bin/env bash
# render-lint-profile — the ONE canonical render of the chart for policy checks.
#
# Everything that scans rendered manifests (verify-render-coverage, kubeconform,
# kube-linter) must render through here. The reason is drift: when two callers
# each spell out their own `helm template` line, one can gain a flag the other
# lacks and an object silently drops out of the scanned set while every check
# still reports green. That is the same failure mode as a hand-maintained image
# sign list (#497/#512) — the fix is to derive, not to repeat.
#
# --api-versions is why this exists. `helm template` does not talk to a cluster,
# so `.Capabilities.APIVersions.Has` is evaluated against a built-in default set
# that does NOT include ValidatingAdmissionPolicy. The launcher-SA admission gate
# (#443) is guarded on that capability so the chart stays installable on clusters
# older than k8s 1.30 — which means that without the flag below it renders to
# nothing here, and CI would lint a security control it cannot see.
#
# Add an entry whenever a template becomes capability-guarded.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"

API_VERSIONS=(
  "admissionregistration.k8s.io/v1/ValidatingAdmissionPolicy"
  "admissionregistration.k8s.io/v1/ValidatingAdmissionPolicyBinding"
)

args=(template kubeswift "$ROOT/charts/kubeswift" -f "$ROOT/hack/lint-values/full.yaml")
for v in "${API_VERSIONS[@]}"; do
  args+=(--api-versions "$v")
done

helm "${args[@]}"
