# Smoke Verification

Reproducible smoke test for KubeSwift: boot a Linux cloud guest end-to-end. Use the script (`make smoke-test`) or run the steps manually.

## Quick walkthrough

**1. Install KubeSwift** — [Helm OCI](../install/helm-oci.md) or [local cluster](../install/local-cluster.md).

**2. Run the smoke test:**

```bash
make smoke-test
```

The script applies samples, waits for SwiftImage Ready (up to 15 min) and SwiftGuest Running (up to 5 min), asserts conditions, and cleans up.

**3. Or run manually:**

```bash
kubectl apply -f config/samples/shared/swiftguestclass-default.yaml
kubectl apply -f config/samples/disk-boot/swiftimage-ubuntu-focal.yaml
kubectl apply -f config/samples/shared/swiftseedprofile-minimal.yaml
kubectl apply -k config/rbac -n default
kubectl apply -f config/samples/disk-boot/swiftguest-sample.yaml

# Wait for SwiftImage Ready (5–15 min)
kubectl get swiftimage ubuntu-cloud -w

# Wait for SwiftGuest Running (1–3 min after image Ready)
kubectl get swiftguest sample -w
```

## Smoke test script

```bash
make smoke-test
# or
./test/smoke/boot-test.sh [--timeout-image MIN] [--timeout-guest MIN] [--no-cleanup]
```

| Option | Default | Description |
|--------|---------|-------------|
| `--timeout-image` | 15 | Minutes to wait for SwiftImage Ready |
| `--timeout-guest` | 5 | Minutes to wait for SwiftGuest Running |
| `--no-cleanup` | — | Leave resources for inspection |

**Environment:** `NAMESPACE=my-ns` to use a different namespace.

## Prerequisites

| Check | Verify |
|-------|--------|
| CRDs installed | `kubectl get crd swiftguests.swift.kubeswift.io swiftguestclasses.swift.kubeswift.io swiftimages.image.kubeswift.io swiftseedprofiles.seed.kubeswift.io` |
| Controllers running | `kubectl get pods -n kubeswift-system` |
| swiftletd image available | Local: `docker images`; or pull from registry |
| RBAC in namespace | Script applies it; for manual: `kubectl apply -k config/rbac -n default` |
| Node capacity | Default SwiftGuestClass: 2 CPU, 2Gi; at least one node must have capacity |
| Image URL reachable | `config/samples/disk-boot/swiftimage-ubuntu-focal.yaml` URL must be reachable from cluster |
| KVM on workers | Run [preflight](worker-node-preflight.md) before smoke test |

**Realistic expectations:**
- SwiftImage import: 5–15 min (Ubuntu cloud image ~600MB)
- SwiftGuest boot: 1–3 min after image Ready
- Default SwiftGuestClass: 2 CPU, 2Gi — nodes need capacity
- If SwiftImage import fails: sample uses `format: raw` but Ubuntu `.img` is qcow2; try `format: qcow2` in `config/samples/disk-boot/swiftimage-ubuntu-focal.yaml`

---

## Verification stages

### Stage 1: SwiftImage Ready

**Expected:** `status.phase=Ready` within timeout (default 15m).

**On failure:**
```bash
kubectl describe swiftimage ubuntu-cloud -n <namespace>
```

**Common causes:**
- Image URL unreachable (network, firewall, invalid URL)
- Insufficient PVC storage
- SwiftImage controller not running

**Remediation:** Verify URL accessibility; check controller logs; ensure sufficient cluster storage.

### Stage 2: SwiftGuest scheduling

**Expected:** Pod created for SwiftGuest, scheduled to a node, not Pending indefinitely.

**On failure:**
```bash
kubectl get pods -n <namespace> -l swift.kubeswift.io/guest=sample
kubectl describe pod <pod-name> -n <namespace>
```

Check Events in the describe output for scheduling failures.

**Common causes:**
- No nodes with sufficient resources
- Node lacks Cloud Hypervisor or swiftletd image
- Resource requests exceed node capacity

**Remediation:** Ensure at least one node can run the guest; verify swiftletd image is available.

### Stage 3: Seed rendering and mount path

**Expected:** When SwiftGuest has seedProfileRef, seed ConfigMap exists and pod has seed volume mounted at `/var/lib/kubeswift/seed`.

**Verify:**
```bash
kubectl get configmap sample-seed -n <namespace>
kubectl get pod <pod-name> -n <namespace> -o jsonpath='{.spec.containers[0].volumeMounts}' | jq .
```

**On failure:** Check SwiftSeedProfile exists; verify controller created seed ConfigMap; ensure pod spec includes seed volume mount.

### Stage 4: swiftletd launches Cloud Hypervisor

**Expected:** Launcher container (swiftletd) runs and does not exit with error.

**On failure:**
```bash
kubectl logs <pod-name> -n <namespace> -c launcher
```

**Common causes:**
- Missing runtime intent ConfigMap or mount
- Cloud Hypervisor binary not found
- Intent parse error; mount path mismatch

**Remediation:** Verify runtime intent ConfigMap is mounted at `/var/lib/kubeswift/intent`; ensure Cloud Hypervisor is in container or on node.

### Stage 5: SwiftGuest Running with status conditions

**Expected:** `status.phase=Running`, GuestRunning condition True. Resolved, PodScheduled conditions True.

**On failure:**
```bash
kubectl describe swiftguest sample -n <namespace>
kubectl logs <pod-name> -n <namespace> -c launcher
```

**Common causes:**
- RBAC missing (swiftletd cannot patch status)
- swiftletd or Cloud Hypervisor crashed
- VM failed to boot

**Remediation:** Apply RBAC; check launcher logs; verify GuestRunning is reported.

### Stage 6: Networking (status.network.primaryIP)

**Expected:** When SwiftGuest has seedProfileRef (enabling networking), `status.network.primaryIP` is populated within ~2 min of Running. The guest receives an IP via DHCP on the pod network.

**On failure:**
```bash
kubectl get swiftguest sample -n <namespace> -o jsonpath='{.status.network}'
kubectl logs <pod-name> -n <namespace> -c launcher | grep -E "lease|guest IP|timeout"
```

**Remediation:** See [guest-networking-troubleshooting.md](../guest-networking-troubleshooting.md).

---

## Failure Check Commands Summary

| Failure mode | Commands |
|--------------|----------|
| SwiftImage not Ready | `kubectl describe swiftimage ubuntu-cloud -n <namespace>` |
| Pod not scheduled | `kubectl describe pod <pod-name> -n <namespace>` |
| Seed missing | `kubectl get configmap sample-seed -n <namespace>`; `kubectl get pod <pod> -o yaml` |
| swiftletd error | `kubectl logs <pod-name> -n <namespace> -c launcher` |
| SwiftGuest not Running | `kubectl describe swiftguest sample -n <namespace>`; `kubectl logs <pod-name> -n <namespace> -c launcher` |
| primaryIP not populated | `kubectl get swiftguest sample -o jsonpath='{.status.network}'`; launcher logs |

---

## Common Failure Causes and Remediation

| Cause | Symptom | Remediation |
|------|---------|-------------|
| Image URL unreachable | SwiftImage Importing/Failed | Verify URL; check network from cluster |
| RBAC missing | GuestRunning never True; swiftletd logs permission denied | `kubectl apply -k config/rbac/ -n <namespace>` |
| Node without Cloud Hypervisor | Pod Pending; events mention image or runtime | Use swiftletd image with CH; or install CH on node |
| Insufficient storage | SwiftImage Failed; PVC pending | Increase cluster storage; check PVC size |
| Controller not running | No reconciliation; resources stuck | Deploy/restart KubeSwift controllers |

---

## Related docs

- [Worker-node preflight](worker-node-preflight.md)
- [Operator checklist](operator-checklist-ubuntu-x86_64.md)
- [Troubleshooting](troubleshooting.md)
- [Local cluster install](../install/local-cluster.md)
- [Helm OCI install](../install/helm-oci.md)
