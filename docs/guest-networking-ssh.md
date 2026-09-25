# Guest Networking and SSH Access

KubeSwift attaches the guest VM to the pod network so the guest receives an IP and is reachable for SSH.

## Pod Network Model

- **Bridge + TAP:** An init container creates an internal Linux bridge (`br0`) with its own subnet and a TAP device (`tap0`) for the VM. The pod's primary interface (`eth0`) is never touched—it retains the pod IP so swiftletd can reach the Kubernetes API.
- **DHCP:** The launcher starts dnsmasq on the bridge and hands out an IP from the bridge subnet (e.g. 192.168.99.10–20) to the VM. VM traffic is NATted out via eth0.
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

## Guest IP, Pod IP and IP Scope

`status.network.primaryIP` is the guest's own address, and
`status.network.primaryIPScope` says where it can be reached from. On a nat
guest (the default) it is `Pod`: the address is on the launcher's private
bridge, every nat launcher hands out the same range, so different guests
routinely report the same `primaryIP`, and it is reachable only from inside
that launcher pod. Identify a guest by its name, and reach it on
`status.network.podIP`, the IP of the launcher pod that currently runs it: the
ports in `spec.network.ports` are DNAT'd from it to the guest. `podIP` changes
when the guest moves to a new launcher (a restart or a migration). The scope is
`Network` when the primary interface rides a multi-node NAD (see
[multi-node L2](networking/multi-node-l2.md)) or an OVN-Kubernetes primary UDN:
`primaryIP` is then an address on that network, reachable there.
`kubectl get swiftguest` shows `Guest IP` and `Pod IP`; `-o wide` adds
`IP Scope`.

## Operator Workflow

1. **Create SwiftSeedProfile** with `ssh_authorized_keys` in userData (or use `swiftseedprofile-ssh.yaml`).

2. **Create SwiftGuest** referencing SwiftImage, SwiftGuestClass, and the SwiftSeedProfile:
   ```bash
   kubectl apply -f config/samples/disk-boot/swiftguest-sample.yaml
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
   Use the private key matching the `ssh_authorized_keys` you provided. When
   `primaryIPScope` is `Pod`, run this from inside the launcher pod, or use
   swiftctl ssh below, which does that for you.

## Using swiftctl ssh (recommended)

Instead of discovering the IP and SSH-ing manually, use swiftctl ssh
which proxies through the launcher pod automatically:

```bash
swiftctl ssh sample -i ~/.ssh/your-key
swiftctl ssh sample -u ubuntu -i ~/.ssh/your-key
```

swiftctl ssh reads status.network.primaryIP and execs an SSH session
through the launcher pod. No direct network access to the guest IP is
required from your workstation.

See [swiftctl](swiftctl.md) for full flag reference.

## Prerequisites

- RBAC: Apply `config/rbac/` in the namespace so swiftletd can patch pods and SwiftGuest status.
- SwiftSeedProfile with seed (networking is enabled when seed is present).
