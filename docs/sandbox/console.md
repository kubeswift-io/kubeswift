# SwiftSandbox console

A SwiftSandbox's console is read with the Kubernetes pod logs API:

```bash
kubectl logs <pod> -c console [-f] [--timestamps]
swiftctl sandbox logs <name> [-f] [--timestamps]
```

`<pod>` is the sandbox's `status.podRef`: its own launcher pod, or the warm-pool
slot it checked out. New in v0.20.0. Before, the console was read by an exec into
the launcher pod.

## What it shows

| Sandbox | Console |
|---|---|
| Cold | The guest console: the kernel and bridge boot lines, then the workload's stdout and stderr |
| Warm checkout | Only the checked-out workload's stdout and stderr, from its dispatch on, as it runs. The slot's boot console is not in it |

A warm checkout's output arrives as it runs on `kernels/sandbox` 6.6.18 and
`kernels/gpu-sandbox` 6.6.7 or later (bridge feature `workload-stream`). On an
older kernel the sandbox runs as before and the output appears in the console
when the workload exits; nothing fails.

## How it works

Every sandbox launcher pod has two containers: `launcher` (privileged: swiftletd
and the hypervisor) and `console`. The launcher writes the console into a
`console` emptyDir that holds nothing else; the `console` container copies that
file to its stdout, byte for byte, and the kubelet keeps it as that container's
log.

- **Cold:** Cloud Hypervisor writes the guest's serial console to
  `<run>/serial.sock.log`, which the launcher links into the console volume
  before the VM starts.
- **Warm:** the checkout's workload runs over vsock. With `workload-stream`, the
  guest agent streams its stdout and stderr to swiftletd, which appends them to
  the console as they arrive; the slot's own boot console stays out of the
  volume. A slot serves one checkout and is deleted after it, so nothing from
  an earlier workload can be in it.
- The `console` container starts with the launcher (not as an init-ordered
  sidecar, which would add a kubelet step to every cold start) and ends with
  it: the launcher holds a FIFO in the volume open for as long as it runs, and
  the console container stops when the last writer is gone, however the
  launcher ended. It always exits 0, so the launcher alone decides the pod's
  phase.

## Security boundary

The `console` container:

- runs as uid 65532, non-root, with no capabilities, no privilege escalation, a
  read-only root filesystem and the RuntimeDefault seccomp profile;
- has no service account token (an empty volume masks it);
- mounts only the console volume, read-only: no launcher file, socket, device,
  vsock or run directory;
- has no stdin, and runs a fixed copy loop that opens no connection. It shares
  the pod's network namespace, like every container of a pod.

Reading the console gives no way to write to the guest or to reach the
launcher. The guest's bytes are not interpreted on the way; they are printed to
the reader's terminal raw, escape sequences included, as with `kubectl logs` of
any container.

## Who can read it

Reading takes `get` on `pods/log` in the sandbox's namespace, plus `get` on the
SwiftSandbox to find the pod and `get` on pods for `kubectl logs` (the API's log
endpoint itself needs only `pods/log`). No `pods/exec`, `pods/attach` or
`pods/portforward`, no Secrets.

`pods/log` is in Kubernetes' built-in `view`, `edit` and `admin` roles. **Anyone
who can read pod logs in a namespace can read the consoles of its sandboxes,
live.** Treat a workload's output as visible to them: a workload should not print
secrets. An administrator who does not want this sets the chart value
`sandboxMaterialize.console: false` (controller flag `--sandbox-console=false`):
pods created after that have no console container. There is no per-sandbox
switch.

A narrow role for a controller that reads consoles:

```yaml
rules:
- {apiGroups: [sandbox.kubeswift.io], resources: [swiftsandboxes], verbs: [get, list, watch]}
- {apiGroups: [""], resources: [pods], verbs: [get]}
- {apiGroups: [""], resources: [pods/log], verbs: [get]}
```

`swiftctl sandbox logs` reads `pods/log`. It execs into the launcher only for a
pod that has no console container (created before v0.20.0), and that needs
`pods/exec`, as before.

The kubeswift-ui **Logs** button goes through the gateway's `/sandbox-logs`,
which still authorizes the user for `get swiftsandboxes/log` and then reads the
console container's logs with its own credential (`get pods/log`, granted by the
chart). For a pod without a console container, or a member whose gateway
credential lacks `pods/log` (RBAC set up by hand before v0.20.0), it falls back
to its exec bridge.

## Bounds

- A warm workload's console keeps 16 MiB (`KUBESWIFT_CONSOLE_MAX_BYTES` on the
  launcher). Past it one line says so and the rest is dropped; the launcher keeps
  reading the guest's output, so the guest never waits on the console and the
  workload's exit code is unaffected. The launcher logs the totals
  (`workload_console ... bytes= dropped=`).
- The kubelet rotates the console container's log like any container log
  (`containerLogMaxSize`, `containerLogMaxFiles`).
- The console container copies through a fixed buffer; it uses a few MiB of
  memory (limit 64 MiB) and under 1 mCPU.
- A cold sandbox's serial file is not bounded: Cloud Hypervisor writes it, as
  before v0.20.0 (#855).

## Lifecycle

| | |
|---|---|
| Cold sandbox | The console is readable while the pod exists; a cold sandbox's pod stays until the sandbox is deleted |
| Warm checkout | Readable from the checkout's dispatch while the workload runs. The slot pod is deleted when the workload ends, and its console with it: follow it (`-f`) from the checkout |
| Sandbox deleted | The pod goes; a follower's stream ends |
| Launcher exits (any way) | The console container copies what is left and exits; the pod ends |
| Controller restart | No effect |
| Console container restarted by the kubelet | The new instance reads the console from the start (it is bounded) |
| Migration | Sandboxes do not migrate |

Nothing is kept anywhere else: once the pod is gone, so is the console.

## Timestamps

`--timestamps` prefixes each line with the time the node received it. On a warm
checkout a line reaches the node within a few milliseconds of the guest writing
it.

Do not compare the guest's own clock with host times. It is set at boot
(kvm-clock, no RTC, no NTP inside) and drifts from the host's: a warm slot's
guest clock was measured 150 to 180 ms behind its node's. For stage timings,
print the guest's monotonic time and take differences, or use the receive
times.

## Turning it off

```yaml
sandboxMaterialize:
  console: false
```

Pods created afterwards have no console container; running ones keep theirs.
`swiftctl sandbox logs` and the gateway fall back to the exec for them.
