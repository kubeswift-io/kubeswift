# KubeSwift Sample Manifests

Sample manifests organized by scenario. Each directory is self-contained with a README
explaining prerequisites, apply order, and expected results.

All seed profiles include `ssh_authorized_keys` — replace with your own key for production.

## Shared Resources

Resources used across multiple scenarios. Apply these first.

```bash
kubectl apply -f config/samples/shared/swiftguestclass-default.yaml
kubectl apply -f config/samples/shared/swiftseedprofile-minimal.yaml
```

## Scenarios

| Directory | Description | Hypervisor | GPU Required |
|-----------|-------------|------------|--------------|
| [disk-boot/](disk-boot/) | Ubuntu Noble 24.04 cloud image boot | Cloud Hypervisor | No |
| [kernel-boot/](kernel-boot/) | faas-minimal direct kernel boot | Cloud Hypervisor | No |
| [qemu-boot/](qemu-boot/) | Ubuntu Noble via QEMU/OVMF | QEMU | No |
| [gpu-pcie/](gpu-pcie/) | Tier 1 PCIe GPU passthrough | Cloud Hypervisor | Yes |
| [gpu-hgx/](gpu-hgx/) | Tier 2 HGX SXM shared NVSwitch | QEMU | Yes |
| [datadisk/](datadisk/) | Secondary data disk attachment | Cloud Hypervisor | No |
| [rocky/](rocky/) | Rocky Linux 9 alternative distro | Cloud Hypervisor | No |

## Quick Start (Disk Boot)

```bash
kubectl apply -k config/rbac
kubectl apply -f config/samples/shared/swiftguestclass-default.yaml
kubectl apply -f config/samples/shared/swiftseedprofile-minimal.yaml
kubectl apply -f config/samples/disk-boot/swiftimage-ubuntu-noble.yaml
kubectl get swiftimage ubuntu-noble -w  # wait for Ready (5-15 min)
kubectl apply -f config/samples/disk-boot/swiftguest-sample.yaml
kubectl get swiftguest sample -w        # wait for Running + IP
swiftctl console sample                 # serial console
swiftctl ssh sample -u kubeswift        # SSH access
```

## Advanced Examples

The [advanced/](advanced/) directory contains additional resource examples:

- `swiftimage-pvc-clone.yaml` — clone an existing PVC as image source
- `swiftimage-upload-placeholder.yaml` — placeholder for future upload support
- `swiftseedprofile-ssh.yaml` — SSH-focused seed profile
- `swiftseedprofile-with-secret.yaml` — cloud-init user-data from a Kubernetes Secret
