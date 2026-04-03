# First Boot: Linux Cloud Guest with KubeSwift

This guide walks through booting your first Linux cloud guest with KubeSwift.

## Prerequisites

- **Kubernetes cluster** – Any conformant cluster (kind, minikube, k3s, or cloud provider)
- **KubeSwift installed** – CRDs, controllers, and swiftletd container image deployed
- **Cloud Hypervisor on nodes** – The `cloud-hypervisor` binary must be available on nodes where guest pods run (or in the swiftletd container image)
- **Sample image URL** – The SwiftImage http source must point to a valid, accessible Linux cloud image (e.g., Ubuntu cloud image)

For exact prerequisites, verification commands, and failure checks, see [docs/smoke-verification.md](smoke-verification.md).

## Sample Image

The default sample uses Ubuntu 24.04 (Noble) cloud image:

- **URL:** `https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img`
- **Format:** qcow2 (NOT raw — Ubuntu .img files are qcow2)
- **Size:** ~600MB (download time varies by network)
- **Note:** All modern Linux distributions are supported (Ubuntu 22.04+, Rocky 9, Fedora, Debian 12) thanks to CLOUDHV.fd UEFI firmware

You can substitute another Linux cloud image (e.g., Fedora, Debian) by editing `config/samples/disk-boot/swiftimage-ubuntu-noble.yaml`.

## Apply Steps

1. Apply resources in order:

   ```bash
   kubectl apply -f config/samples/shared/swiftguestclass-default.yaml
   kubectl apply -f config/samples/disk-boot/swiftimage-ubuntu-noble.yaml
   kubectl apply -f config/samples/shared/swiftseedprofile-minimal.yaml
   kubectl apply -f config/samples/disk-boot/swiftguest-sample.yaml
   ```

2. Wait for SwiftImage to reach Ready (import can take 5–15 minutes depending on image size and network):

   ```bash
   kubectl get swiftimage ubuntu-noble -w
   ```

   When `status.phase` is `Ready`, the image is prepared.

3. Wait for SwiftGuest to reach Running:

   ```bash
   kubectl get swiftguest sample -w
   ```

   The guest progresses: Pending → Scheduling → Running. When `status.phase` is `Running` and `GuestRunning` condition is True, the VM is up.

## Expected Timeline

| Step              | Typical duration |
|-------------------|------------------|
| SwiftImage Ready  | 5–15 minutes     |
| SwiftGuest Running| 1–3 minutes after image Ready |

## Verification

- **Status conditions:**

  ```bash
  kubectl describe swiftguest sample
  ```

  Check `status.conditions` for `Resolved`, `ImageReady`, `PodScheduled`, `GuestRunning`.

- **SSH access:** When using a SwiftSeedProfile with `ssh_authorized_keys`, the guest gets an IP on the pod network. See [guest-networking-ssh.md](guest-networking-ssh.md) for the full workflow (discover IP via `kubectl get swiftguest <name> -o jsonpath='{.status.network.primaryIP}'`, then `ssh kubeswift@<IP>`).

  Or use swiftctl ssh for direct SSH access via the launcher pod:

  ```bash
  swiftctl ssh sample -i ~/.ssh/your-key
  ```

- **Pod logs:**

  ```bash
  kubectl get pods -l <swiftguest-label>
  kubectl logs <pod-name> -c <container>
  ```

  Logs show swiftletd and Cloud Hypervisor output.

## Troubleshooting

### Image import failure

- **Symptom:** SwiftImage stays in `Importing` or transitions to `Failed`
- **Causes:** Invalid URL, network unreachable, disk space
- **Actions:** Check `kubectl describe swiftimage` for condition reason; verify URL is accessible; ensure cluster has sufficient PVC storage

### Pod scheduling failure

- **Symptom:** SwiftGuest pod stays `Pending`; `PodScheduled=False`
- **Causes:** No nodes with Cloud Hypervisor; resource requests exceed capacity; node selector mismatch
- **Actions:** Check `kubectl describe pod` for events; ensure at least one node can run the guest; verify node has Cloud Hypervisor binary or swiftletd image includes it

### swiftletd errors

- **Symptom:** Pod runs but VM never starts; logs show swiftletd exit or error
- **Causes:** Missing runtime intent; mount path mismatch; Cloud Hypervisor not found; intent parse error
- **Actions:** Check `kubectl logs` for swiftletd; verify runtime intent ConfigMap is mounted; ensure Cloud Hypervisor binary is in container or on node
