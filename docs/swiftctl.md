# swiftctl

swiftctl is the KubeSwift operator CLI for SwiftGuest lifecycle management and operability. It provides console access, lifecycle control, SSH, log tailing, rich status display, and debugging.

## Installation

**From source:**

```bash
go install github.com/kubeswift-io/kubeswift/cmd/swiftctl@latest
```

**From build:**

```bash
make build-go
# Binary: ./swiftctl (in repo root)
```

**From release:** Download the `swiftctl` binary from the GitHub Release page for your platform.

## Global flags

All commands accept these flags:

| Flag | Default | Description |
|------|---------|-------------|
| `-n`, `--namespace` | `default` | Kubernetes namespace |
| `--kubeconfig` | `KUBECONFIG` or `~/.kube/config` | Path to kubeconfig |
| `--context` | (current context) | Kubernetes context to use |
| `-v`, `--version` | — | Print version and exit |

## Commands

### console

Attach to the VM serial console for interactive keyboard access.

```
swiftctl console [guest-name]
```

**How it works:** Execs into the launcher pod and runs `socat -,raw,echo=0 UNIX-CONNECT:<serial.sock>` for bidirectional access. Puts the local terminal in raw mode so keystrokes reach the guest immediately.

**Detach:** Press **Ctrl+O** to detach. In raw mode, Ctrl+C is sent to the guest, not swiftctl.

**Requirements:**
- Guest must be `phase=Running`
- Must run from an interactive terminal (not piped)
- swiftletd image includes `socat`

**Examples:**

```bash
swiftctl console sample
swiftctl -n myns console my-guest
```

**Troubleshooting:**
- Blank console: wait 30–60s for Ubuntu to fully boot, then retry
- No prompt: ensure `SwiftSeedProfile` includes `runcmd: [systemctl enable --now getty@ttyS0.service]`
- Socket not found: check `swiftctl debug sample` for serial socket status

---

### ssh

SSH into the guest VM via the launcher pod.

```
swiftctl ssh [guest-name]
```

**How it works:** Reads `status.network.primaryIP`, execs into the launcher pod, writes the SSH key to a temp file, and runs `ssh -o StrictHostKeyChecking=no -i <key> <user>@<ip>`.

**Flags:**

| Flag | Default | Description |
|------|---------|-------------|
| `-u`, `--user` | `kubeswift` | SSH username |
| `-i`, `--identity` | `~/.ssh/id_rsa` | Path to SSH private key |

**Requirements:**
- Guest must be `phase=Running`
- `status.network.primaryIP` must be populated
- SSH public key must be injected via SwiftSeedProfile `ssh_authorized_keys`
- Must run from an interactive terminal

**Examples:**

```bash
swiftctl ssh sample
swiftctl ssh sample -u ubuntu -i ~/.ssh/mykey
swiftctl -n myns ssh my-guest -u fedora
```

---

### start

Set `spec.runPolicy=Running` and delete the existing pod so the controller recreates it.

```
swiftctl start [guest-name]
```

**What it does:**
1. Patches `spec.runPolicy=Running` on the SwiftGuest
2. Deletes the existing pod (if any) so the controller recreates it with the current intent

**Examples:**

```bash
swiftctl start sample
swiftctl -n myns start my-guest
```

---

### stop

Set `spec.runPolicy=Stopped` and gracefully terminate the VM.

```
swiftctl stop [guest-name]
```

**What it does:**
1. Patches `spec.runPolicy=Stopped`
2. Sends `SIGTERM` to the hypervisor PID via pod exec
3. Waits up to 30 seconds for the pod to terminate
4. If still running after 30s: force-deletes the pod

The controller sees `runPolicy=Stopped` and will not recreate the pod.

**Examples:**

```bash
swiftctl stop sample
swiftctl -n myns stop my-guest
```

---

### restart

Delete the launcher pod. The controller recreates it, which restarts the VM.

```
swiftctl restart [guest-name]
```

**Requirements:** `spec.runPolicy` must not be `Stopped`. Use `swiftctl start` first if the guest is stopped.

**Examples:**

```bash
swiftctl restart sample
swiftctl -n myns restart my-guest
```

---

### describe

Print a rich human-readable summary of SwiftGuest status.

```
swiftctl describe [guest-name]
```

**Output includes:** phase, node, run policy, image/kernel/class/seed refs, runtime (hypervisor, PID), console (serial socket path), network (primaryIP, interfaces), conditions, pod reference.

**Examples:**

```bash
swiftctl describe sample
swiftctl -n myns describe my-guest
```

**Sample output:**

```
Name:        sample
Namespace:   default
Phase:       Running
Node:        worker-1
RunPolicy:   Running

Spec:
  Image:       ubuntu-noble
  Kernel:      (none)
  GuestClass:  default
  SeedProfile: ssh

Runtime:
  Hypervisor:  cloud-hypervisor
  PID:         12345

Console:
  SerialSocket: /var/lib/kubeswift/run/default-sample/serial.sock

Network:
  PrimaryIP:   192.168.99.11
  Interfaces:
    - eth0: 192.168.99.11

Conditions:
  Resolved: True — ...
  PodScheduled: True — ...
  GuestRunning: True — ...

Pod:
  Name:      swiftguest-default-sample-xxxxx
  Namespace: default
```

---

### logs

Tail or stream the launcher pod logs (swiftletd output).

```
swiftctl logs [guest-name]
```

**Flags:**

| Flag | Default | Description |
|------|---------|-------------|
| `-f`, `--follow` | `false` | Stream logs continuously |
| `--tail` | `50` | Number of lines to show from the end |

**Examples:**

```bash
swiftctl logs sample
swiftctl logs sample -f
swiftctl logs sample --tail 200
swiftctl -n myns logs my-guest --follow
```

---

### debug

Run diagnostics on the launcher pod.

```
swiftctl debug [guest-name]
```

**Without `--shell`:** Prints:
- Runtime directory contents (`ls -la /var/lib/kubeswift/run/<guestId>/`)
- Serial socket status (exists or not found)
- Cloud Hypervisor or QEMU process command line (from `/proc/<pid>/cmdline`)
- Runtime intent JSON (`/var/lib/kubeswift/intent/runtime-intent.json`)

**With `--shell`:** Drops into an interactive shell in the **launcher container**. This is the Debian-based container running swiftletd and the hypervisor — not the VM itself. Use `swiftctl console` to connect to the VM.

**Flags:**

| Flag | Default | Description |
|------|---------|-------------|
| `--shell` | `false` | Drop into interactive shell in launcher container |

**Examples:**

```bash
swiftctl debug sample
swiftctl debug sample --shell
swiftctl -n myns debug my-guest
```

**From a debug shell**, you can manually test the serial console:

```bash
socat -,raw,echo=0 UNIX-CONNECT:/var/lib/kubeswift/run/default-sample/serial.sock
```

Press Ctrl+C or type `exit` to leave the debug shell.

## Console transport details

`swiftctl console` uses **exec + socat** for interactive serial console access. No port-forward, no websocket, no custom transport protocol.

1. swiftletd passes `--serial socket=<path>` to Cloud Hypervisor when launching
2. Cloud Hypervisor creates a Unix socket at `/var/lib/kubeswift/run/<namespace>-<name>/serial.sock`
3. swiftctl execs into the launcher pod and runs: `socat -,raw,echo=0 UNIX-CONNECT:<socket-path>`
4. swiftctl puts the local terminal in raw mode (identical to `kubectl exec -it`) so all keystrokes reach the guest

For QEMU guests, the serial socket is created the same way via `-chardev socket,id=serial0,path=<path>,server=on,wait=off`.

**Clusters that restrict `pods/exec` do not support console access.** Use SSH via cloud-init as an alternative.

## SSH transport details

`swiftctl ssh` does not require open ports, load balancers, or NodePort services. It routes through the Kubernetes exec API:

1. Reads `status.network.primaryIP` from the SwiftGuest
2. Execs into the launcher pod (same container that runs the hypervisor)
3. The launcher pod is on the same bridge network as the guest (br0/tap0)
4. SSH connects from launcher pod to guest IP, giving full bidirectional interactive access

The SSH key is written to a temp file inside the exec session and removed after SSH exits.

## Exit codes

| Code | Meaning |
|------|---------|
| 0 | Success |
| 1 | Error: guest not found, pod not found, phase mismatch, patch failed, etc. |

Errors are printed to stderr.
