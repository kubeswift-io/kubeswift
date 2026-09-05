# Hugepage-backed guest memory

`SwiftGuestClass.spec.hugepages` backs a guest's RAM with 2MiB or 1GiB pages
instead of the default 4K, rendered as Cloud Hypervisor
`--memory hugepages=on,hugepage_size=`.

| Value | Effect |
|---|---|
| *(unset)* | ordinary 4K pages — unchanged behaviour |
| `2Mi` | guest RAM from 2MiB hugepages |
| `1Gi` | guest RAM from 1GiB hugepages |

Hugepages cut TLB misses and page-table walks for memory-heavy guests, keep
guest RAM permanently resident (hugepages are never swapped), and are a
prerequisite for DPDK-style workloads inside the guest.

## Reserve the pages on the node first

The kubelet only advertises hugepages already reserved on the host, and it
reads them **at startup**. A class asking for a size the node has not reserved
will not schedule — which is the intended failure mode: it is visible, and it
happens before any VM starts.

### 2Mi — runtime, no reboot

```
sysctl -w vm.nr_hugepages=4096                                    # 8GiB
echo "vm.nr_hugepages = 4096" > /etc/sysctl.d/90-kubeswift-hugepages.conf
```

### 1Gi — boot time

1Gi pages need physically contiguous memory and generally cannot be reserved
reliably on a host that has been up for a while. Reserve at boot:

```
GRUB_CMDLINE_LINUX="... default_hugepagesz=1G hugepagesz=1G hugepages=8"
```

Then `update-grub` and reboot.

### Make the kubelet notice

Restarting the kubelet is required either way. Restarting the whole node agent
usually also restarts the container runtime, which bounces every pod on the
node — check how your distribution supervises the two before doing it on a node
with running guests.

Confirm the node advertises the resource:

```
kubectl get node <node> -o jsonpath='{.status.allocatable}' | tr ',' '\n' | grep hugepages
```

## How the launcher accounts for it

Guest RAM **moves** to the hugepages resource rather than being requested
twice. With `hugepages: 2Mi` on a 4Gi class the launcher pod requests:

- `hugepages-2Mi: 4Gi` — the guest's RAM
- `memory:` the launcher overhead only

The kubelet subtracts reserved hugepages from the node's allocatable `memory`,
so a pod booking both would consume twice the guest's RAM from the node and
stop scheduling long before the hugepages ran out.

The container gets a size-qualified `emptyDir` (`medium: HugePages-2Mi`)
mounted at `/dev/hugepages`, which is where Cloud Hypervisor allocates from.

## Units

The CRD uses Kubernetes units (`2Mi`, `1Gi`) because that is what the matching
node resource is called. Cloud Hypervisor rejects those —
`ParseMemory(Conversion("hugepage_size", "2Mi"))` — so the controller
translates to `2M` / `1G` when writing the runtime intent.

## `reserve=on` is load-bearing here

KubeSwift always emits `reserve=on` on `--memory`. With hugepages that is not
merely a fail-fast nicety:

| Config, on a node with no pages reserved | Result |
|---|---|
| `hugepages=on` **with** `reserve=on` | clean `GuestMemoryRegion(Mmap(OutOfMemory))` |
| `hugepages=on` **without** `reserve=on` | **SIGBUS** — the VMM dies (exit 135) |

Verified against Cloud Hypervisor v53.0. Do not drop `reserve=on`.

## Related

`hugepages` composes with `cpuPinning` and `smtPolicy`
(see [cpu-pinning.md](cpu-pinning.md)) — a latency-sensitive guest usually
wants both. It is independent of `coreScheduling`.
