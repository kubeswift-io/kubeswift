#!/usr/bin/env bash
# sync-kustomize — render the security-relevant manifests the kustomize install
# (make deploy, quickstart, e2e) shares with the Helm chart FROM the chart, so
# the two cannot drift.
#
# They had: the kustomize controller role kept grants the chart had removed
# (swiftgpuprofiles writes, swiftgpunodes create/delete, write verbs on
# pods/log), its swiftletd reporter role had extra verbs, the sandbox reporter
# role and the launcher-ServiceAccount admission gate were missing entirely.
#
# The chart templates rendered here use only plain value placeholders, so sed
# renders them without helm. Values are the chart defaults.
#
# Run by `make generate`; CI fails if the committed files differ from its
# output. Do not edit the generated files by hand.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUT="${1:-$ROOT}"
VALUES="$ROOT/charts/kubeswift/values.yaml"

# value <top-level key> <child key>: a scalar from values.yaml.
value() {
  awk -v top="$1:" -v key="$2:" '
    $0 ~ "^"top { in_top = 1; next }
    in_top && /^[^ #]/ { in_top = 0 }
    in_top && $1 == key { print $2; exit }
  ' "$VALUES"
}

NAMESPACE="$(awk '/^namespace:/ { print $2; exit }' "$VALUES")"
GUEST_SA="$(value launcherSAGate guestServiceAccountName)"
SANDBOX_SA="$(value launcherSAGate sandboxServiceAccountName)"
for v in NAMESPACE GUEST_SA SANDBOX_SA; do
  if [[ -z "${!v}" ]]; then
    echo "sync-kustomize: could not read $v from $VALUES" >&2
    exit 2
  fi
done

render() { # render <chart template> <output> <description>
  local src="$ROOT/charts/kubeswift/templates/$1" dst="$OUT/$2"
  mkdir -p "$(dirname "$dst")"
  {
    echo "# GENERATED from charts/kubeswift/templates/$1 by hack/sync-kustomize.sh"
    echo "# ($3). Edit the chart template and run make generate."
    grep -Ev '^\{\{-? (if|end)' "$src" |
      sed -e "s/{{ \.Values\.namespace }}/$NAMESPACE/g" \
          -e "s/{{ \.Values\.launcherSAGate\.guestServiceAccountName }}/$GUEST_SA/g" \
          -e "s/{{ \.Values\.launcherSAGate\.sandboxServiceAccountName }}/$SANDBOX_SA/g"
  } >"$dst"
  if grep -n '{{' "$dst" >&2; then
    echo "sync-kustomize: unrendered template syntax left in $2" >&2
    exit 1
  fi
}

render controller-manager/rbac.yaml config/manager/controller-manager-rbac.yaml \
  "controller-manager and launcher reporter RBAC"
render webhook/launcher-sa-policy.yaml config/admission/launcher-sa-policy.yaml \
  "launcher ServiceAccount admission gate; needs Kubernetes 1.30+"
