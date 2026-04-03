# Console Implementation: Learnings from virtctl and Cloud Hypervisor

This document summarizes findings from studying [virtctl](https://github.com/kubevirt/kubevirt/tree/main/pkg/virtctl) and [Cloud Hypervisor](https://github.com/cloud-hypervisor/cloud-hypervisor) to improve `swiftctl console` and serial console reliability.

---

## 1. virtctl (KubeVirt)

### Architecture

- **Transport:** WebSocket API, not exec. virtctl calls `client.VirtualMachineInstance(namespace).SerialConsole(vmi, options)` which opens a WebSocket to the KubeVirt API server; the server proxies to the VMI's serial console.
- **No pod exec:** KubeVirt does not exec into a pod. The serial stream is exposed via a custom API endpoint.

### Console Flow

```
User TTY <-> virtctl <-> WebSocket <-> KubeVirt API <-> VMI serial
```

### Key Implementation Details

| Aspect | virtctl | swiftctl (current) |
|--------|---------|---------------------|
| **Transport** | WebSocket | exec + socat |
| **Raw terminal** | `term.MakeRaw()` | `term.MakeRaw()` ✓ |
| **Escape to detach** | Ctrl+] (byte 29) | Ctrl+O (byte 15) |
| **SIGINT (Ctrl+C)** | Sends to guest (raw mode) | Sends to guest ✓ |
| **Exit on SIGINT** | `signal.Notify(waitInterrupt, os.Interrupt)` – user can Ctrl+C to exit before connection | Same pattern ✓ |
| **Buffer size** | 1024 bytes | N/A (socat handles) |
| **Disconnect message** | Explains VM off, another user, network | Could add similar |

### virtctl Escape Sequence

```go
const escapeSequenceCode = 29  // 0x1D = Ctrl+]
```

- In the stdin read loop, if `buf[0] == 29`, virtctl stops forwarding and exits.
- **Ctrl+]** is a common telnet/SSH escape; virtctl uses it for consistency.
- **Ctrl+5** is the same byte (0x1D) on some keyboards.

### virtctl Attach Pattern

1. Create `io.Pipe()` pairs for stdin/stdout.
2. Goroutine: connect WebSocket, stream In/Out to pipes.
3. Main goroutine: `Attach()` – raw terminal, copy stdin→pipe, pipe→stdout, detect escape byte in stdin.
4. SIGINT handler: close stopChan to exit.

### Takeaways for swiftctl

1. **Escape key:** Consider switching from Ctrl+O to **Ctrl+]** for familiarity (virtctl, telnet, many CLIs).
2. **Disconnect message:** Add a friendly message when disconnected (VM off, another user, etc.).
3. **Timeout:** virtctl has `--timeout` (default 5 min) for waiting for VMI; swiftctl could add `--wait` for socket readiness (currently 15s hardcoded).

---

## 2. Cloud Hypervisor

### Serial Console Options

CH supports `--serial` with several modes:

```
--serial off|null|pty|tty|file=<path>|socket=<path>
```

Default: `null`.

### Socket Mode

- **Format:** `--serial socket=/path/to/serial.sock`
- **Behavior:** CH creates a Unix domain socket. When a client connects, the serial port (ttyS0) is bidirectionally connected.
- **Parsing:** `ConsoleConfig::parse()` in `vmm/src/config.rs`:
  - `socket=` → `ConsoleOutputMode::Socket`, `socket: Some(PathBuf::from(path))`
- **Usage:** Connect with `socat -,raw,echo=0 UNIX-CONNECT:/path/to/serial.sock`

### Serial vs Console

CH has two separate concepts:

| Option | Purpose |
|--------|---------|
| `--serial` | Legacy serial port (ttyS0) – used for boot logs, getty, kernel console |
| `--console` | Virtio console – higher performance, different device |

For disk boot with firmware (e.g. CLOUDHV.fd), the **serial** device is created when the VM is properly initialized. The kernel/OS must have `console=ttyS0,115200n8` in its cmdline for output to appear.

### Firmware Boot and Cmdline

- With `--kernel <firmware>` (CLOUDHV.fd), CH boots from disk. The kernel cmdline comes from **GRUB on the disk**, not from CH's `--cmdline`.
- KubeSwift already patches GRUB during import to add `console=ttyS0,115200n8`.

### Socket Creation

- CH creates the socket when the VM starts (after API socket is ready).
- KubeSwift uses `--serial socket=<runtime-dir>/serial.sock` and `--console off`.
- The socket path: `/var/lib/kubeswift/run/<namespace>-<name>/serial.sock`.

### Takeaways for swiftctl

1. **socat flags:** `-,raw,echo=0` is correct – raw mode, no echo (guest handles echo).
2. **Socket timing:** CH creates the socket when the VM is up; the 15s wait loop in swiftctl is appropriate.
3. **GRUB patch:** Confirmed – firmware boot needs GRUB patching; CH `--cmdline` is not used for disk boot.

---

## 3. Recommendations for KubeSwift

### High Value

1. **Escape key:** Change from Ctrl+O to **Ctrl+]** – matches virtctl and common practice.
2. **Disconnect message:** When exec stream ends, print a short message (e.g. "Disconnected. VM may have stopped or another user connected.").
3. **Documentation:** Ensure `swiftctl console --help` and docs clearly state the escape key.

### Medium Value

4. **Configurable wait:** Add `--wait=30` (or similar) for socket readiness.
5. **Consistent messaging:** Print "Connected to <guest>. Press Ctrl+] to detach." on connect (like virtctl).

### Already Correct

- Raw terminal mode
- exec + socat (appropriate for KubeSwift; no WebSocket backend)
- socat `-,raw,echo=0`
- GRUB patch for `console=ttyS0`
- `--console off` and `--serial socket=` in CH args

---

## 4. References

- [virtctl console](https://github.com/kubevirt/kubevirt/blob/main/pkg/virtctl/console/console.go)
- [Cloud Hypervisor main.rs](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/cloud-hypervisor/src/main.rs) – `--serial` help: `off|null|pty|tty|file=|socket=`
- [CH config.rs ConsoleConfig](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/vmm/src/config.rs) – `ConsoleConfig::parse()`, `socket=` → `ConsoleOutputMode::Socket`
- KubeSwift: `cmd/swiftctl/console.go`, `rust/swift-ch-client/src/config.rs`, `internal/controller/swiftimage/import.go`
