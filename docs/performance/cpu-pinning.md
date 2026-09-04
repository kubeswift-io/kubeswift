# CPU pinning and SMT placement

Pin a guest's vCPUs to dedicated host CPUs, and choose how they land on
hyper-thread siblings. Set on `SwiftGuestClass`, so every guest of the class
inherits it.

```yaml
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuestClass
metadata:
  name: latency-sensitive
spec:
  cpu: 2
  memory: 4Gi
  rootDisk: { format: raw, size: 20Gi }
  cpuPinning: static     # none (default) | static
  smtPolicy: spread      # spread (default) | pack
```

| Field | Values | Meaning |
|---|---|---|
| `cpuPinning` | `none` (default), `static` | `static` pins vCPU *N* to one host CPU. `none` lets the host scheduler place vCPU threads. |
| `smtPolicy` | `spread` (default), `pack` | Which SMT siblings to use. Ignored unless `cpuPinning: static`. |

`spread` uses one thread per physical core before touching any core's second
thread, so a guest smaller than the core count gets a whole core's pipeline and
L1/L2 per vCPU. `pack` fills both siblings of a core before moving on, touching
fewer cores and leaving whole cores free for other work.

## Which CPUs get used

The candidate CPUs are **the launcher pod's own effective cpuset**, never the
node's full CPU list. This is the important detail:

- With the kubelet CPU Manager policy at **`static`**, the pod's exclusive CPUs
  are assigned at admission — *after* the controller has written the guest's
  runtime intent. A CPU chosen ahead of that assignment is not honoured: the
  kernel clamps or rejects it, and the guest would run unpinned while still
  reporting as pinned.
- With the policy at **`none`** (the default), the effective cpuset is simply
  every host CPU, so the result is what you would expect.

Because the set is read at launch time, the same configuration stays correct if
a node's policy changes later. Nothing in the class needs to know which policy a
node runs.

The launcher pod already requests equal CPU requests and limits with a whole
number of CPUs, which is what the CPU Manager `static` policy requires before it
will grant a pod exclusive CPUs. No extra configuration is needed on the
KubeSwift side to benefit from it.

## It fails loudly, not quietly

`cpuPinning: static` needs at least as many CPUs in the launcher pod's cpuset as
the class requests vCPUs. If it does not, **the launcher refuses to start** and
the guest reports the failure:

```
cpuPinning=static needs at least 4 CPUs in the launcher pod's cpuset, found 2 ([0, 1]).
Reduce the class's cpu, or give the pod more CPUs.
```

Pinning two vCPUs onto one host CPU would report success while delivering worse
latency than not pinning at all, so it is refused rather than approximated.

## Relationship to `coreScheduling`

They solve different problems and compose:

- `cpuPinning` / `smtPolicy` are **placement** — *which* CPUs this guest runs on.
- [`coreScheduling`](../security-hardening-notes.md) is **isolation** — *who else*
  may run on a core's sibling thread, a defence against cross-thread SMT side
  channels.

A latency-sensitive multi-tenant class typically wants both:

```yaml
  cpuPinning: static
  smtPolicy: spread
  coreScheduling: vm
```

## Verifying

The guest's vCPU threads live in the launcher pod. Check what they are actually
allowed to run on:

```bash
kubectl exec -n <ns> <guest> -- sh -c 'grep Cpus_allowed_list /proc/self/status'
```

The launcher also logs the plan it computed at start-up:

```bash
kubectl logs -n <ns> <guest> | grep cpuPinning
```

```
cpuPinning=static (Spread): vcpu0->cpu[0] vcpu1->cpu[1]
```

## Limits

- **NUMA placement is not covered here.** Aligning vCPUs, memory and passthrough
  devices on one NUMA node is separate work and needs a multi-socket host to be
  meaningful.
- **Hugepage-backed guest memory is not covered here.**
- vCPU pinning applies to the Cloud Hypervisor path. The QEMU (HGX GPU) path has
  its own pinning, computed from GPU NUMA locality.
