# Guest Networking and SSH Access

KubeSwift attaches the guest VM to the pod network so the guest receives an IP and is reachable for SSH.

## Pod Network Model

- **Bridge + TAP:** An init container creates a Linux bridge (`br0`), moves the pod's primary interface (`eth0`) onto the bridge, and creates a TAP device (`tap0`) for the VM.
- **DHCP:** The launcher starts dnsmasq on the bridge and hands out an IP from the pod subnet to the VM.
- **Cloud-init:** The seed includes network-config (default: DHCP on first Ethernet interface) so the guest configures its interface on first boot.

## SSH Key Injection

Add `ssh_authorized_keys` to the cloud-init user in SwiftSeedProfile userData:

```yaml
users:
  - name: kubeswift
    passwd: $6$...
    sudo: ALL=(ALL) NOPASSWD:ALL
    ssh_authorized_keys:
      - "ssh-ed25519 AAAA... user@host"
```

Use `config/samples/swiftseedprofile-ssh.yaml` as a template. Samples include a default key; replace with your own for production.

## IP Discovery

swiftletd polls the dnsmasq lease file and patches the pod annotation `kubeswift.io/guest-ip` when the VM obtains an IP. The controller copies this to SwiftGuest status.

## Operator Workflow

1. **Create SwiftSeedProfile** with `ssh_authorized_keys` in userData (or use `swiftseedprofile-ssh.yaml`).

2. **Create SwiftGuest** referencing SwiftImage, SwiftGuestClass, and the SwiftSeedProfile:
   ```bash
   kubectl apply -f config/samples/swiftguest-sample.yaml
   ```
   (Ensure the SwiftGuest's `seedProfileRef` points to your SSH-enabled profile.)

3. **Wait for Running and network ready:**
   ```bash
   kubectl get swiftguest sample -w
   ```
   When `status.phase` is `Running` and `status.network.ready` is true, the guest has an IP.

4. **Discover the guest IP:**
   ```bash
   kubectl get swiftguest sample -o jsonpath='{.status.network.primaryIP}'
   ```

5. **SSH into the guest:**
   ```bash
   ssh kubeswift@<primaryIP>
   ```
   Use the private key matching the `ssh_authorized_keys` you provided.

## Prerequisites

- RBAC: Apply `config/rbac/` in the namespace so swiftletd can patch pods and SwiftGuest status.
- SwiftSeedProfile with seed (networking is enabled when seed is present).
