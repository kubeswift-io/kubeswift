# First-Boot Cluster Smoke Verification

This document provides exact prerequisites, verification commands, expected conditions, and failure checks for reproducible first-boot smoke testing of KubeSwift. Use it to validate that a local cluster can boot a Linux cloud guest end-to-end.

## Prerequisites

Before running the smoke test, ensure all of the following are satisfied.

### 1. CRDs installed

KubeSwift CRDs for swift.kubeswift.io, image.kubeswift.io, and seed.kubeswift.io must be installed.

**Verify:**
```bash
kubectl get crd swiftguests.swift.kubeswift.io swiftguestclasses.swift.kubeswift.io swiftimages.image.kubeswift.io swiftseedprofiles.seed.kubeswift.io
```

All four CRDs should be listed. If any are missing, apply CRDs from `config/crd/`.

### 2. KubeSwift controllers deployed

Controllers (SwiftImage, SwiftGuest, etc.) must be running.

**Verify:**
```bash
kubectl get pods -A | grep -E "kubeswift|controller"
```

Controllers should be Running. The exact namespace depends on your deployment (e.g., `kube-system`, `kubeswift-system`, or a custom namespace).

### 3. swiftletd container image available

The swiftletd image (with Cloud Hypervisor) must be available on nodes where guest pods run. Build locally with `make build-swiftletd-image` or pull from a registry.

**Verify:** Ensure the image referenced by the launcher (see `internal/controller/swiftguest/constants.go` LauncherImage) exists. For local builds:
```bash
docker images | grep swiftletd
```

### 4. RBAC for swiftletd

swiftletd needs RBAC to patch SwiftGuest status. Apply in each namespace where SwiftGuests run.

**Apply:**
```bash
kubectl apply -k config/rbac/ -n <namespace>
```

For non-default namespaces, patch the RoleBinding subject:
```bash
kubectl patch rolebinding swiftletd-reporter -n <namespace> --type=json \
  -p='[{"op":"replace","path":"/subjects/0/namespace","value":"<namespace>"}]'
```

**Verify:**
```bash
kubectl get role swiftletd-reporter -n <namespace>
kubectl get rolebinding swiftletd-reporter -n <namespace>
```

### 4a. SSH validation (optional follow-up)

When using a SwiftSeedProfile with `ssh_authorized_keys`, the guest receives an IP on the pod network. To validate networking and SSH:

1. Create a SwiftGuest with an SSH-enabled SwiftSeedProfile (see `config/samples/swiftseedprofile-ssh.yaml`).
2. Wait for `status.network.ready=true`.
3. Get the IP: `kubectl get swiftguest <name> -o jsonpath='{.status.network.primaryIP}'`
4. SSH: `ssh -o StrictHostKeyChecking=no -o ConnectTimeout=10 kubeswift@<IP> echo ok`

See [guest-networking-ssh.md](guest-networking-ssh.md) for the full workflow.

### 5. Node requirements

At least one node must be able to run guest pods. The node needs:
- Cloud Hypervisor binary (or swiftletd image that includes it)
- Sufficient CPU/memory for the guest (default sample: 1 CPU, 128Mi)
- Writable storage for PVCs

**Verify:** Schedule a test pod or check node capacity.

### 6. Accessible image URL

The SwiftImage http source must point to a valid, accessible Linux cloud image. Default sample uses Ubuntu cloud image.

**Verify:** Ensure the URL in `config/samples/swiftimage-http.yaml` is reachable from the cluster (e.g., `curl -I <url>` from a pod).

---

## Smoke Test Script

Run the smoke test:
```bash
# Set KUBECONFIG if using a non-default cluster
export KUBECONFIG=/path/to/kubeconfig

make smoke-test
# or
./test/smoke/boot-test.sh [--timeout-image MIN] [--timeout-guest MIN] [--no-cleanup]
```

Default: `--timeout-image=15`, `--timeout-guest=5`. Use `NAMESPACE=my-ns` to override the default namespace.

**After applying fixes:** Rebuild and redeploy controller + swiftletd images before re-running the smoke test. For remote clusters, push images to the registry; for kind/minikube, use `make load-images`.

---

## Verification Stages

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

**Common causes:**
- Interface name mismatch (fixed: default network-config uses `match: name: en*` and `eth*`)
- Lease poll timeout (VM boot + DHCP took too long)
- dnsmasq or bridge setup failed

**Remediation:** See [guest-networking-troubleshooting.md](guest-networking-troubleshooting.md).

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
