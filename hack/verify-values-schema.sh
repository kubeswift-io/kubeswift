#!/usr/bin/env bash
# verify-values-schema — prove charts/kubeswift/values.schema.json still rejects
# unknown values keys, and still matches values.yaml.
#
# THE FAILURE MODE. Helm ignores a values key that no template reads. One
# mistyped `--set snapshotOras.image.tag=...` (the real key is snapshotORAS)
# left a decoy that `helm get values` carried into every later upgrade, so the
# real tag froze for four releases while the values looked current. The schema
# turns that into an install/upgrade error.
#
# Rendering valid values cannot tell whether the schema works: a schema that is
# deleted, renamed, excluded by .helmignore, or has lost an
# `additionalProperties: false` renders them just as cleanly. So the cases
# below must FAIL, not just pass.
#
# Run via:
#   make verify-values-schema
#   ./hack/verify-values-schema.sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CHART="$ROOT/charts/kubeswift"

command -v helm >/dev/null || { echo "verify-values-schema: helm not found" >&2; exit 1; }
command -v jq >/dev/null || { echo "verify-values-schema: jq not found" >&2; exit 1; }
[[ -f "$CHART/values.schema.json" ]] || { echo "verify-values-schema: $CHART/values.schema.json is missing" >&2; exit 1; }

fail=0

# expect_reject KEY ARGS... — rendering must fail on a schema error naming KEY.
expect_reject() {
  local key="$1"; shift
  local out errs
  if out="$(helm template kubeswift "$CHART" "$@" 2>&1)"; then
    echo "verify-values-schema: ACCEPTED $*" >&2
    echo "  expected a schema error for '$key'" >&2
    fail=1
    return
  fi
  # Helm < 3.18:          Additional property KEY is not allowed
  # Helm >= 3.18 and 4.x: additional properties 'KEY' not allowed
  errs="$(grep -E 'dditional propert(y|ies)' <<<"$out" || true)"
  if ! grep -qw -- "$key" <<<"$errs"; then
    echo "verify-values-schema: $* failed, but not on a schema error for '$key':" >&2
    echo "$out" >&2
    fail=1
  fi
}

# expect_accept ARGS... — rendering must succeed.
expect_accept() {
  local out
  if ! out="$(helm template kubeswift "$CHART" "$@" 2>&1)"; then
    echo "verify-values-schema: REJECTED $*" >&2
    echo "$out" >&2
    fail=1
  fi
}

# The incident, and the correct spelling.
expect_reject snapshotOras --set snapshotOras.image.tag=v1
expect_accept --set snapshotORAS.image.tag=v1

# Below the top level. gateway.oidc spells it clientID but ui.oidc spells it
# clientId, which makes that one an easy slip.
expect_reject tga --set controllerManager.image.tga=v1
expect_reject clientID --set ui.oidc.clientID=kubeswift
expect_reject clusterissuer --set gateway.ingress.tlsAuto.clusterissuer=letsencrypt

# Free-form maps must stay open. So must global: Helm adds it to a chart's
# values whenever the chart is used as a dependency, so rejecting it would make
# the chart unusable as one.
expect_accept \
  --set global.imageRegistry=registry.example.com \
  --set 'gateway.ingress.annotations.example\.com/key=value' \
  --set monitoring.serviceMonitor.additionalLabels.release=kube-prometheus-stack \
  --set controllerManager.resources.limits.memory=1Gi \
  --set 'swiftletd.imagePullSecrets[0].name=regcred'

# Drift. A key added to values.yaml but not to the schema fails every render
# already. The reverse does not: a key removed from values.yaml but left in the
# schema stays silently accepted. So require every key the schema declares
# (global aside) and lint values.yaml against that.
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
cp -r "$CHART" "$tmp/kubeswift"
jq 'walk(if type == "object" and .additionalProperties == false
         then .required = (((.properties // {}) | keys) - ["global"])
         else . end)' \
  "$CHART/values.schema.json" > "$tmp/kubeswift/values.schema.json"
if ! out="$(helm lint "$tmp/kubeswift" 2>&1)"; then
  echo "verify-values-schema: values.yaml and values.schema.json disagree:" >&2
  grep -F '[ERROR]' <<<"$out" >&2 || echo "$out" >&2
  echo >&2
  echo "  A key in values.yaml needs an entry in the schema, or a key the schema" >&2
  echo "  declares is no longer in values.yaml and should be removed from it." >&2
  fail=1
fi

if [[ $fail -ne 0 ]]; then
  exit 1
fi
echo "verify-values-schema: OK — unknown keys rejected, free-form maps and global accepted, schema matches values.yaml."
