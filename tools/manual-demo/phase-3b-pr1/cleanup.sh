#!/usr/bin/env bash
# Phase 3b PR 1 manual demo — tear down everything launch-pods.sh
# created. Safe to re-run; idempotent.
set -euo pipefail

NS="${NS:-phase-3b-pr1-demo}"
GUEST_NAME="${GUEST_NAME:-pr1-guest}"
DST_POD_NAME="${DST_POD_NAME:-${GUEST_NAME}-dst}"

step() { printf '\n[%s] %s\n' "$1" "$2"; }

# ─── 1/3 dst pod ──────────────────────────────────────────────────
# Delete dst pod FIRST so it doesn't try to talk to a half-deleted
# src on the controller's reconcile cycle.
step 1/3 "delete destination pod ${DST_POD_NAME}"
kubectl -n "${NS}" delete pod "${DST_POD_NAME}" --ignore-not-found --wait=false

# ─── 2/3 SwiftGuest + image + seed + class ─────────────────────────
step 2/3 "delete SwiftGuest ${GUEST_NAME} and supporting resources"
kubectl -n "${NS}" delete sg "${GUEST_NAME}" --ignore-not-found --wait=false
kubectl -n "${NS}" delete swiftimage ubuntu-noble --ignore-not-found --wait=false
kubectl -n "${NS}" delete ssp default --ignore-not-found --wait=false
kubectl -n "${NS}" delete sgc pr1-class --ignore-not-found --wait=false

# ─── 3/3 namespace ────────────────────────────────────────────────
step 3/3 "delete namespace ${NS}"
kubectl delete ns "${NS}" --ignore-not-found --wait=false

echo
echo "Cleanup dispatched (deletions run in background). Verify with:"
echo "  kubectl get ns | grep ${NS}"
echo "  kubectl get pods -A | grep ${GUEST_NAME}"
