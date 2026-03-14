# Troubleshooting

Common issues when running KubeSwift and how to resolve them.

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

**Actions:**
```bash
kubectl apply -k config/rbac/ -n <namespace>
kubectl logs <pod-name> -n <namespace> -c launcher
kubectl describe swiftguest <name> -n <namespace>
```

- Apply RBAC in the namespace
- Check launcher logs for swiftletd/Cloud Hypervisor errors
- Verify runtime intent ConfigMap is mounted at `/var/lib/kubeswift/intent`

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
