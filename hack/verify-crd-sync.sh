#!/usr/bin/env bash
# verify-crd-sync — fail loudly when config/crd/bases/ has CRDs that
# config/crd/kustomization.yaml doesn't list, or vice versa.
#
# This guards against the failure mode that bit Phase 1: `make
# generate` produces a new CRD in config/crd/bases/, the chart's
# crds/ directory is updated via `cp config/crd/bases/*.yaml
# charts/kubeswift/crds/`, but config/crd/kustomization.yaml is
# hand-maintained and easily forgotten. `make deploy` runs
# `kubectl apply -k config/crd` which silently skips bases not
# listed as resources — so new CRDs never reach the cluster.
#
# Run via:
#   make verify-crd-sync     (developer/CI)
#   ./hack/verify-crd-sync.sh

set -euo pipefail

CRD_DIR="$(cd "$(dirname "$0")/.." && pwd)/config/crd"
BASES_DIR="$CRD_DIR/bases"
KUSTOMIZATION="$CRD_DIR/kustomization.yaml"

if [[ ! -d "$BASES_DIR" ]]; then
  echo "ERROR: $BASES_DIR does not exist" >&2
  exit 2
fi
if [[ ! -f "$KUSTOMIZATION" ]]; then
  echo "ERROR: $KUSTOMIZATION does not exist" >&2
  exit 2
fi

# bases/ — the source of truth (controller-gen output, sorted).
mapfile -t bases < <(cd "$BASES_DIR" && ls -1 *.yaml | sort)

# kustomization.yaml — what `kubectl apply -k` will actually apply.
# Strip leading whitespace + "- bases/" prefix, then sort.
mapfile -t listed < <(
  awk '
    /^[[:space:]]*-[[:space:]]+bases\// {
      sub(/^[[:space:]]*-[[:space:]]+bases\//, "")
      print
    }
  ' "$KUSTOMIZATION" | sort
)

# Diff the two sorted lists. Any line prefixed with < or > is drift.
drift="$(diff <(printf '%s\n' "${bases[@]}") <(printf '%s\n' "${listed[@]}") || true)"

if [[ -z "$drift" ]]; then
  echo "OK: config/crd/kustomization.yaml lists every CRD in config/crd/bases/ (${#bases[@]} CRDs)"
  exit 0
fi

echo "ERROR: config/crd/kustomization.yaml is out of sync with config/crd/bases/" >&2
echo "" >&2
echo "Files in bases/ but missing from kustomization.yaml:" >&2
diff <(printf '%s\n' "${bases[@]}") <(printf '%s\n' "${listed[@]}") | awk '/^< / {sub(/^< /,""); print "  - bases/" $0}' >&2 || true
echo "" >&2
echo "Files listed in kustomization.yaml but missing from bases/:" >&2
diff <(printf '%s\n' "${bases[@]}") <(printf '%s\n' "${listed[@]}") | awk '/^> / {sub(/^> /,""); print "  - bases/" $0}' >&2 || true
echo "" >&2
echo "How to fix:" >&2
echo "  1. Make sure 'make generate' has been run (regenerates bases/)." >&2
echo "  2. Edit config/crd/kustomization.yaml so the resources: list" >&2
echo "     matches the contents of config/crd/bases/." >&2
echo "  3. Run 'cp config/crd/bases/*.yaml charts/kubeswift/crds/' to" >&2
echo "     keep the Helm chart in lockstep." >&2
echo "  4. Re-run 'make verify-crd-sync' to confirm." >&2
exit 1
