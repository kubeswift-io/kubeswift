# Troubleshooting

Common issues when running KubeSwift and how to resolve them.

## Controller-manager CrashLoopBackOff (exit code 2)

**Symptom:** controller-manager pod restarts repeatedly with `Exit Code: 2`.

**Causes:**
- `flag provided but not defined: -leader-elect` — controller-manager binary did not support `--leader-elect` (fixed in recent builds; rebuild and redeploy)
- `failed to wait for swiftimage caches to sync kind source: *v1.Job` — missing RBAC for Jobs (add `batch` API group `jobs` to ClusterRole)
- `v1.ListOptions is not suitable for converting to "image.kubeswift.io/v1alpha1"` — scheme missing metav1 for custom types (add `metav1.AddToGroupVersion` per API group)
- Missing RBAC for leader election — controller uses `--leader-elect` and needs `coordination.k8s.io/leases`
- Missing CRDs (SwiftGuest, SwiftImage, SwiftSeedProfile, SwiftGuestClass)
- RBAC not applied or outdated

**Actions:**
```bash
kubectl logs -n kubeswift-system deployment/controller-manager --previous
```

- If logs mention `leases` or `coordination.k8s.io`: ensure the controller-manager ClusterRole includes:
  ```yaml
  - apiGroups: ["coordination.k8s.io"]
    resources: ["leases"]
    verbs: ["create", "get", "update", "patch", "list", "watch"]
  ```
- Reapply RBAC and redeploy:
  ```bash
  kubectl apply -f config/manager/controller-manager-rbac.yaml
  # or, with Helm: helm upgrade kubeswift ... (chart includes RBAC)
  ```
- Verify CRDs exist: `kubectl get crd | grep kubeswift`

## swiftletd DaemonSet not deployed

**Symptom:** No swiftletd pods in the cluster.

**Cause:** The swiftletd DaemonSet is **disabled by default** (`swiftletd.daemonset.enabled: false`). swiftletd is designed to run only inside SwiftGuest pods as the launcher container. A standalone DaemonSet has no runtime intent and would crash immediately.

**Fix:** No action needed. swiftletd runs as a sidecar when you create SwiftGuest resources. Enable the DaemonSet only if you have a custom setup that provides runtime intent to each node.

## ImagePullBackOff (controller-manager, swiftletd)

**Symptom:** Pods fail with `ErrImagePull` or `ImagePullBackOff` for `ghcr.io/kubeswift-io/kubeswift/controller-manager:latest` or `swiftletd:latest`.

**Cause:** CI does not publish images with `latest`. Published tags: `vX.Y.Z` (stable/RC), `sha-<shortsha>` (dev).

**Fix:**

- **OCI install:** Use a chart version whose images exist. The chart (as of the fix) defaults image tags to match the chart version. Reinstall or upgrade:
  ```bash
  helm upgrade kubeswift oci://ghcr.io/kubeswift-io/charts/kubeswift --version 0.13.1 -n kubeswift-system \
    --set controllerManager.image.tag=v0.13.1 \
    --set swiftletd.image.tag=v0.13.1
  ```
  For dev: use `--version 0.0.0-dev.<sha>` and `--set controllerManager.image.tag=sha-<sha> --set swiftletd.image.tag=sha-<sha>`.

- **Local cluster:** Build and load images, then deploy with kustomize (not Helm):
  ```bash
  make build-images
  make load-images
  make deploy
  ```

## SwiftImage

### SwiftImage stuck in Importing or Failed

**Symptom:** `status.phase` never reaches Ready.

**Causes:**
- Image URL unreachable (network, firewall, invalid URL)
- Insufficient PVC storage
- Format mismatch (e.g. `format: raw` but image is qcow2)
- SwiftImage controller not running

**Actions:**
```bash
kubectl describe swiftimage <name> -n <namespace>
kubectl get pods -n <namespace> -l image.kubeswift.io/swiftimage=<name>
```

- Verify URL is accessible from the cluster (e.g. `curl -I <url>` from a pod)
- Ensure StorageClass exists and PVC has sufficient capacity (≥10Gi for default)
- For Ubuntu cloud images (`.img`), try `format: qcow2`
- Check controller-manager logs

### Format mismatch

Ubuntu cloud images (`noble-server-cloudimg-amd64.img`) are typically qcow2. If `swiftimage-http.yaml` uses `format: raw`, import may fail. Use `format: qcow2` in the SwiftImage spec.

---

## SwiftGuest

### Pod not scheduled (Pending)

**Symptom:** SwiftGuest pod stays Pending; `PodScheduled=False`.

**Causes:**
- No nodes with sufficient resources
- Node lacks Cloud Hypervisor or swiftletd image
- Resource requests exceed node capacity
- `/dev/kvm` not available to pods (see [operator checklist](operator-checklist-ubuntu-x86_64.md))

**Actions:**
```bash
kubectl describe pod <pod-name> -n <namespace>
kubectl get nodes
kubectl describe node <node-name>
```

- Ensure at least one node can run the guest (CPU, memory)
- Verify swiftletd image is available on nodes
- Run [worker-node preflight](worker-node-preflight.md) on worker nodes

### GuestRunning never True

**Symptom:** Pod runs but `GuestRunning` condition stays False.

**Causes:**
- RBAC missing — swiftletd cannot patch SwiftGuest status
- swiftletd or Cloud Hypervisor crashed
- VM failed to boot
- Runtime intent or mount path mismatch
- **Kube API unreachable** — `failed to report running: ServiceError: client error (Connect)` when `KUBERNETES_SERVICE_HOST` is set but the cluster IP is unreachable (e.g. external API server, custom networking). swiftletd now falls back to `Config::incluster_dns()` (uses `https://kubernetes.default.svc`). Ensure you run a swiftletd image that includes this fix.

**Actions:**
```bash
kubectl apply -k config/rbac/ -n <namespace>
kubectl logs <pod-name> -n <namespace> -c launcher
kubectl describe swiftguest <name> -n <namespace>
```

- Apply RBAC in the namespace
- Check launcher logs for swiftletd/Cloud Hypervisor errors
- Verify runtime intent ConfigMap is mounted at `/var/lib/kubeswift/intent`
- If logs show `client error (Connect)`: rebuild and redeploy swiftletd image; use latest from `ghcr.io/kubeswift-io/kubeswift/swiftletd` or a release tag

### swiftletd errors in logs

**Symptom:** Launcher container exits or logs show errors.

**Causes:**
- Missing runtime intent ConfigMap or mount
- Cloud Hypervisor binary not found
- Intent parse error
- Mount path mismatch
- `/dev/kvm` not available (KVM required)

**Actions:**
```bash
kubectl logs <pod-name> -n <namespace> -c launcher
kubectl get configmap <guest>-runtime-intent -n <namespace>
kubectl get pod <pod-name> -n <namespace> -o yaml
```

- Verify runtime intent ConfigMap exists and is mounted
- Ensure Cloud Hypervisor is in the swiftletd container image
- Check pod spec includes `/dev/kvm` device (see [operator checklist](operator-checklist-ubuntu-x86_64.md))

---

## Webhooks

### Create/update blocked by webhook

**Symptom:** Create or update of SwiftGuest/SwiftImage/SwiftSeedProfile fails with webhook error.

**Causes:**
- Webhook unreachable (cert-manager not ready, TLS issues)
- Webhook validation failure

**Actions:**
- If webhook is unreachable, temporarily remove webhook configs:
  ```bash
  kubectl delete validatingwebhookconfiguration kubeswift-validating-webhook --ignore-not-found
  kubectl delete mutatingwebhookconfiguration kubeswift-mutating-webhook --ignore-not-found
  kubectl apply -k config/default
  ```
- Check cert-manager Certificate and webhook Service
- Review validation error message for spec issues

---

## Related docs

- [Smoke verification](smoke-verification.md) — Prerequisites and failure checks
- [Operator checklist](operator-checklist-ubuntu-x86_64.md) — Host prerequisites
- [Worker-node preflight](worker-node-preflight.md) — Pre-install validation
