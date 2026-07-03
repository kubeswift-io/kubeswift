#!/bin/bash
# destination.sh — Phase 2 manual demo: apply destination launcher pod
# in receiver mode.
#
# Reads STATE_FILE (from source.sh), takes the captured source pod YAML
# as a starting point, modifies it for receiver mode, and applies the
# resulting Pod. Waits for the pod to reach Ready (CH spawned with
# --api-socket only, awaiting receive-migration).
#
# Required env vars (after source.sh has run):
#   TARGET_NODE   — destination node hostname (must differ from SOURCE_NODE
#                   in STATE_FILE)
#
# Optional env vars:
#   STATE_FILE    — defaults to /tmp/kubeswift-migration-phase2-manual/state.env
#
# Modifications applied to the source pod YAML:
#   * metadata.name: <source>-mig-recv
#   * metadata.uid / .resourceVersion / .ownerReferences / .creationTimestamp:
#     stripped (these would conflict with `kubectl apply`)
#   * status: stripped
#   * spec.nodeName: $TARGET_NODE (direct binding — bypasses scheduler)
#   * metadata.annotations:
#       kubeswift.io/migration-role: receiver
#       kubeswift.io/migration-phase2-unsafe-plaintext: ack
#       kubeswift.io/migration-test: "true"  (for the optional NetworkPolicy)
#   * launcher container env: KUBESWIFT_MIGRATION_ROLE=receiver
#
# After this script:
#   * Destination launcher pod is running on $TARGET_NODE.
#   * swiftletd has spawned CH with --api-socket only (receiver mode).
#   * STATE_FILE is updated with DST_POD, TARGET_NODE.

set -euo pipefail

: "${TARGET_NODE:?TARGET_NODE env var required}"
STATE_FILE="${STATE_FILE:-/tmp/kubeswift-migration-phase2-manual/state.env}"
if [[ ! -f "$STATE_FILE" ]]; then
    echo "ERROR: $STATE_FILE not found. Run source.sh first." >&2
    exit 1
fi
# shellcheck source=/dev/null
. "$STATE_FILE"

if [[ "$TARGET_NODE" == "$SOURCE_NODE" ]]; then
    echo "ERROR: TARGET_NODE ($TARGET_NODE) must differ from SOURCE_NODE ($SOURCE_NODE)" >&2
    exit 1
fi

DST_POD="${SOURCE_POD%-launcher*}-launcher-mig-recv"
# Cap pod-name length at 63 chars (k8s DNS-1123 label limit).
if [[ ${#DST_POD} -gt 63 ]]; then
    DST_POD="${DST_POD:0:63}"
fi
echo "==> Destination launcher pod: $DST_POD on $TARGET_NODE"

SRC_POD_YAML="$WORKDIR/src-pod.yaml"
DST_POD_YAML="$WORKDIR/dst-pod.yaml"
if [[ ! -f "$SRC_POD_YAML" ]]; then
    echo "ERROR: $SRC_POD_YAML missing — re-run source.sh" >&2
    exit 1
fi

echo "==> Building destination pod YAML from $SRC_POD_YAML..."

# Strip status + identity fields, rewrite name + nodeName, add receiver
# annotations + env var. Done with `kubectl run`-style YAML
# manipulation via python so we don't add a yq dependency.
python3 - "$SRC_POD_YAML" "$DST_POD_YAML" "$DST_POD" "$TARGET_NODE" <<'PY'
import sys
import yaml

src_path, dst_path, dst_name, target_node = sys.argv[1:5]
with open(src_path) as f:
    pod = yaml.safe_load(f)

meta = pod.setdefault("metadata", {})
meta["name"] = dst_name
for f in ("uid", "resourceVersion", "creationTimestamp", "selfLink",
          "managedFields", "ownerReferences"):
    meta.pop(f, None)
ann = meta.setdefault("annotations", {})
ann["kubeswift.io/migration-role"] = "receiver"
ann["kubeswift.io/migration-phase2-unsafe-plaintext"] = "ack"
labels = meta.setdefault("labels", {})
labels["kubeswift.io/migration-test"] = "true"

pod.pop("status", None)

spec = pod["spec"]
spec["nodeName"] = target_node
# Direct binding via spec.nodeName bypasses the scheduler. Same model
# Phase 1's offline-migration controller uses (Approach A from the
# Phase 1 spike). Drop spec.nodeSelector to avoid a node-affinity
# mismatch — TARGET_NODE wins.
spec.pop("nodeSelector", None)
# RestartPolicy=Never matches Phase 1's launcher pods invariant.
spec["restartPolicy"] = "Never"

# Add KUBESWIFT_MIGRATION_ROLE=receiver to the swiftletd / launcher
# container env list. The container name is "launcher" per pod.go.
for c in spec.get("containers", []):
    if c.get("name") == "launcher":
        env = c.setdefault("env", [])
        # Replace any existing entry with the same name.
        env = [e for e in env if e.get("name") != "KUBESWIFT_MIGRATION_ROLE"]
        env.append({"name": "KUBESWIFT_MIGRATION_ROLE", "value": "receiver"})
        c["env"] = env

with open(dst_path, "w") as f:
    yaml.safe_dump(pod, f)
PY

echo "==> Applying destination pod..."
kubectl apply -f "$DST_POD_YAML"

echo "==> Waiting for $DST_POD to reach Ready..."
kubectl wait pod "$DST_POD" -n "$NAMESPACE" --for=condition=Ready --timeout=120s

# Confirm CH is up by checking that the API socket exists inside the
# launcher container. CH needs ~1-3s to bind after the launcher container
# starts.
echo "==> Confirming CH api-socket inside launcher container..."
for i in $(seq 1 20); do
    if kubectl exec "$DST_POD" -n "$NAMESPACE" -c launcher -- \
        test -S /var/lib/kubeswift/runtime/ch.sock 2>/dev/null; then
        echo "    api-socket present (attempt $i)"
        break
    fi
    if [[ $i -eq 20 ]]; then
        echo "ERROR: ch.sock did not appear within 20 seconds" >&2
        kubectl logs "$DST_POD" -n "$NAMESPACE" -c launcher --tail=40 | sed 's/^/    /'
        exit 1
    fi
    sleep 1
done

echo
cat >> "$STATE_FILE" <<EOF
DST_POD="$DST_POD"
TARGET_NODE="$TARGET_NODE"
EOF
echo "==> Destination ready. State updated:"
grep -E "^(DST_POD|TARGET_NODE)=" "$STATE_FILE" | sed 's/^/    /'
echo
echo "Next: ./run.sh"
