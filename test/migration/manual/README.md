# Phase 2 Live Migration — Manual Demo

> **Status:** Test surface — NOT a usable migration workflow.

---

## ⚠️ SECURITY BANNER ⚠️

**Phase 2 swiftletd live-migration plumbing carries unauthenticated guest state in cleartext on the cluster network. Operators MUST NOT route production traffic through this path. Phase 2 is a swiftletd-extension test surface; Phase 3 adds mTLS for production use.**

**Routing a production VM through Phase 2 is a security incident.** Full guest memory and CPU state, including any in-memory secrets (TLS private keys, application credentials, kernel keyrings, decrypted disk content held in page cache), is exposed in cleartext to anyone with read access to the cluster pod network for the duration of the migration.

The `kubeswift.io/migration-phase2-unsafe-plaintext: ack` annotation is required on both source and destination launcher pods for swiftletd to accept any migration action. The annotation must be set to the literal string `ack`.

---

## What this is

This directory contains a hand-rolled operator workflow that demonstrates Phase 2 swiftletd's send/receive primitives end-to-end without going through any controller. Phase 3 will replace this with the SwiftMigration controller's `mode: live` path.

The demo:
1. Picks an existing running SwiftGuest as the source.
2. Applies a hand-rolled destination launcher pod (receiver mode).
3. Annotates each pod to trigger `vm.receive-migration` and `vm.send-migration`.
4. Verifies the guest survived the migration via a sentinel disk file.

**Operators wanting to migrate VMs in production should use Phase 1's `SwiftMigration` CRD (offline mode) until Phase 3 ships live mode.** Phase 2's manual path is for testing the swiftletd extension in isolation.

## What this is NOT

- An automated migration controller. There is no controller integration.
- Production-ready. The S2 ack gate exists exactly so this can't be misused.
- Cancel-capable from the action loop. PR-B's cancel handler is a placeholder; to cancel an in-flight migration, `kubectl delete pod` on the destination launcher pod.
- Progress-reporting. The intermediate `precopy`/`stopcopy`/`listening` annotation values are not emitted in this version; the operator only sees `running` (accept) → `complete` (source success) / `running` (destination success) / `failed`.

## Prerequisites

- Two-node cluster with both nodes labelled `kubeswift.io/launcher-node=true` (the smoke-test cluster — miles + boba — is the validated configuration).
- The same swiftletd image deployed to all nodes. Phase 2 mandates exact-image-tag match across source and destination CH (Decision 3 — `spec.allowVersionSkew` lands in Phase 3).
- A running SwiftGuest on the SOURCE node with a sentinel marker the operator can verify post-migration. Example:
  ```
  kubectl exec swiftguest-pod -c swiftletd -- swiftctl ssh <guest> --tty=false -- \
    "echo SPIKE-PRE-MIGRATION-$(date +%s) | sudo tee /root/sentinel.txt && sudo md5sum /root/sentinel.txt"
  ```
  Record the md5 — you will check it post-migration.
- `kubectl` configured against the cluster (the deploy kubeconfig at `/home/wrkode/code/vmm-kubeswift/dev-tests/kubeswift/kubeswift-cluster.yaml`).

## SwiftGuest CR isolation invariant

**The Phase 2 manual path does NOT touch any SwiftGuest CR.** All operations are on launcher pods directly. This keeps Phase 2 fully orthogonal to Phase 1's SwiftGuest controller; Phase 1's offline-migration logic and SwiftGuest-CR-identity preservation are unaffected by this demo.

The migration leaves both launcher pods running until you delete them by hand. The SwiftGuest CR remains pointed at the SOURCE pod throughout — `kubectl get swiftguest` will continue showing the source as the canonical pod even after the guest has migrated. This is intentional in Phase 2; Phase 3's controller will swap `status.podRef` atomically.

## Files in this directory

| File | Purpose |
|---|---|
| `dst-launcher-pod.yaml.template` | Hand-rolled destination launcher pod template; envsubst expands `${...}` placeholders |
| `source.sh` | Inspects the source SwiftGuest, gathers required metadata, prepares env vars |
| `destination.sh` | Renders the dst pod template + applies it; waits for the pod to reach Ready |
| `run.sh` | Orchestrates the full kubectl annotate sequence per design §7 steps 4–10 |
| `verify.sh` | Post-migration sentinel verification |
| `README.md` | This file |

## Running the demo

```bash
# 1. Source preparation: pick an existing SwiftGuest, gather its volumes/intent
SWIFTGUEST=my-guest NAMESPACE=default ./source.sh

# 2. Destination prep: render the dst pod YAML, apply it, wait for Ready.
#    Provide the target node hostname (must be different from the src pod's node).
TARGET_NODE=boba ./destination.sh

# 3. End-to-end orchestration: write start-receive on dst, observe listening,
#    write start-send on src, observe terminal statuses on both.
./run.sh

# 4. Verify the sentinel survived. Compare md5 against the pre-migration value.
./verify.sh
```

Each script writes to `/tmp/kubeswift-migration-phase2-manual/state.env` so subsequent scripts can read prior context without arguments.

## NetworkPolicy hygiene (optional but recommended)

Operators running this demo on a non-test cluster should apply a default-deny NetworkPolicy on the test namespace to limit incidental exposure of the cleartext migration traffic to other workloads:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: deny-cross-namespace-migration-traffic
spec:
  podSelector:
    matchLabels:
      kubeswift.io/migration-test: "true"
  policyTypes: [Ingress, Egress]
  ingress:
    - from:
        - podSelector:
            matchLabels:
              kubeswift.io/migration-test: "true"
  egress:
    - to:
        - podSelector:
            matchLabels:
              kubeswift.io/migration-test: "true"
```

Phase 3's mTLS removes the need for this belt-and-braces measure. Phase 2 manual demo prerequisites do NOT enforce it; the unsafe-plaintext-ack gate is the formal gate.

## Cancel

To cancel an in-flight migration:

```
kubectl delete pod <dst-launcher-pod>
```

PR-B's `MigrationCancel` action handler ships as a placeholder; the SIGKILL-CH-from-action-loop wiring is a follow-up beyond Phase 2's must-have set. `kubectl delete pod` on the destination is the operational equivalent — kubelet sends SIGTERM to the launcher pod, which kills CH, and on the source side the in-flight `send-migration` returns `connection refused` (F2 finding from the spike). The source CH automatically resumes the guest.
