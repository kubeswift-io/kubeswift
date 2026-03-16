# Guest Networking and Status Reporting – Validation Summary

This document summarizes the root causes, fixes, and validation steps for the smoke test failures around guest networking and status reporting.

## Root Causes

### 1. Guest networking (DHCP lease empty)

**Cause:** Ubuntu cloud images use predictable interface names (`ens3`, `enp0s3`), not `eth0`. The old default network-config targeted only `eth0`, so cloud-init configured a non-existent interface and the VM never sent DHCP. The dnsmasq lease file stayed empty.

**Fix:** `internal/controller/swiftguest/controller.go` – `defaultNetworkConfig` now uses:
- Top-level `network:` wrapper (required by cloud-init/netplan)
- `match: name: en*` for predictable naming (Ubuntu, Debian)
- `match: name: eth*` for legacy (Rocky/RHEL with net.ifnames=0)

### 2. Kube API client (Connect error)

**Cause:** swiftletd used `kube::Client::try_default()` which uses `KUBERNETES_SERVICE_HOST` + `KUBERNETES_SERVICE_PORT`. In clusters with an external API server (e.g. `https://frida.labk8s.io:6443`), the cluster IP may be unreachable from pods. The patch to SwiftGuest status fails with `ServiceError: client error (Connect)`.

**Target:** `api.patch_status()` in `report.rs` → `PATCH /apis/swift.kubeswift.io/v1alpha1/namespaces/{ns}/swiftguests/{name}/status` → Kubernetes API server at `https://{KUBERNETES_SERVICE_HOST}:{PORT}` or `https://kubernetes.default.svc`.

**Fix:** `rust/swiftletd/src/kube_client.rs` – `create_client()` tries `Config::incluster_dns()` first (uses `https://kubernetes.default.svc`), then falls back to `Client::try_default()`. Both report-running and lease poller use this client.

### 3. status.network.primaryIP

**Cause:** primaryIP is populated when the lease poller patches `kubeswift.io/guest-ip` on the pod and the controller copies it to status. The lease file was empty (no DHCP), so the poller never patched. Fixing (1) and (2) restores the full flow.

## Files Changed

| File | Change |
|------|--------|
| `internal/controller/swiftguest/controller.go` | `defaultNetworkConfig` with `network:` wrapper and `en*`/`eth*` match |
| `rust/swiftletd/src/kube_client.rs` | New module: `create_client()` with incluster_dns fallback |
| `rust/swiftletd/src/main.rs` | Report-running uses `kube_client::create_client()` |
| `rust/swiftletd/src/lease.rs` | Lease poller uses `kube_client::create_client()` |

## Validation Commands

### Prerequisites

1. **Build images:**
   ```bash
   make build-controller-image build-swiftletd-image
   ```

2. **Deploy to cluster:**
   - **Remote cluster (ghcr.io):** Push images, then redeploy:
     ```bash
     docker push ghcr.io/projectbeskar/kubeswift/controller-manager:latest
     docker push ghcr.io/projectbeskar/kubeswift/swiftletd:latest
     KUBECONFIG=/path/to/kubeconfig make deploy
     ```
   - **kind/minikube:** Load images locally:
     ```bash
     make load-images
     make deploy
     ```

3. **Restart SwiftGuest pods** so they pull the new swiftletd image (or delete and recreate via smoke test).

### Run smoke test

```bash
export KUBECONFIG=/home/wrkode/code/vmm-kubeswift/dev-tests/kubeswift/kubeswift-cluster.yaml

make smoke-test-cleanup
make smoke-test
```

## Expected Smoke Test Output After Fix

```
=== KubeSwift boot smoke test ===
Namespace: default
...
SwiftImage Ready
Pod sample created and scheduled
Seed ConfigMap and mount OK
SwiftGuest Running
Checking status conditions...
Conditions OK
Waiting for status.network.primaryIP (timeout 2m)...
status.network.primaryIP=10.244.125.10

=== Smoke test PASSED ===
```

- **GuestRunning:** True
- **status.network.primaryIP:** Populated (e.g. `10.244.125.10`)
- **Lease poll:** No timeout; swiftletd discovers IP and patches pod
- **Tap warning:** `Tap tap0 already exists` is expected and harmless (init container creates tap0 before cloud-hypervisor)

## Phase vs GuestRunning

`phase=Running` can appear before `GuestRunning=True` because:
- **phase** comes from pod phase (controller observes pod status)
- **GuestRunning** is set by swiftletd when it reports VM state to the API

This is intentional. The smoke test checks both; GuestRunning becomes True once swiftletd successfully reports.

## Doc Updates

- `docs/operator/troubleshooting.md` – Added Kube API Connect error cause and fix for GuestRunning
- `docs/smoke-verification.md` – Added KUBECONFIG note and rebuild/redeploy reminder
- `docs/guest-networking-troubleshooting.md` – Already documents interface mismatch and data flow
