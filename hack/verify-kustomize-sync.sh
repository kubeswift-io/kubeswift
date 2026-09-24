#!/usr/bin/env bash
# verify-kustomize-sync — fail when the kustomize copies of chart manifests
# (see hack/sync-kustomize.sh) differ from what the chart renders to.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

"$ROOT/hack/sync-kustomize.sh" "$TMP"
status=0
for f in config/manager/controller-manager-rbac.yaml config/admission/launcher-sa-policy.yaml; do
  if ! diff -u "$ROOT/$f" "$TMP/$f"; then
    echo "ERROR: $f is out of sync with the Helm chart. Run: make generate" >&2
    status=1
  fi
done
exit "$status"
