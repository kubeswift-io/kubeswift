# KubeSwift Smoke Tests

Minimal smoke tests to validate the boot-first guest flow.

## Prerequisites

- **KubeSwift cluster** – CRDs, controllers, and swiftletd deployed
- **kubectl** – Configured to talk to the cluster
- **Cloud Hypervisor** – On nodes or in swiftletd container

## boot-test.sh

Applies sample manifests, waits for SwiftImage Ready and SwiftGuest Running, asserts conditions, and cleans up.

```bash
./test/smoke/boot-test.sh
```

Or via Makefile:

```bash
make smoke-test
```

### Options

- `--timeout-image MIN` – Timeout for SwiftImage Ready (default: 15)
- `--timeout-guest MIN` – Timeout for SwiftGuest Running (default: 5)
- `--no-cleanup` – Skip cleanup; leave resources for inspection
- `--cleanup-only` – Only delete what earlier runs created (`make smoke-test-cleanup`)
- `--scenario NAME` – Run one scenario: disk-boot, kernel-boot, qemu-boot, gpu-alloc, multi-nic

`NAMESPACE=my-ns` runs the test in another namespace; the samples'
`namespace: default` is dropped when they are created.

### What cleanup deletes

Every object the script creates is labelled
`kubeswift.io/smoke-test=<namespace>`, and cleanup deletes only objects with
that label. An object that already exists, such as a shared `ubuntu-noble`
SwiftImage or the cluster's `default` SwiftGuestClass, is used as it is: the
test neither updates nor deletes it.

### gpu-alloc

Checks GPU allocation without GPU hardware. The mock SwiftGPUNode is named
after a real Node that is not cordoned, has no SwiftGPUNode and is not
labelled `kubeswift.io/gpu-node=true`, because allocation accepts only a
VFIO-ready SwiftGPUNode whose Node exists and is schedulable. The test guest
is `runPolicy: Stopped` and pinned to that Node, so it gets its allocation
but never a launcher. The scenario is skipped when no Node qualifies, or when
another SwiftGuest in the cluster waits for a GPU and could be given the mock
one.

### Known flakiness

- **Image import** – First run can be slow; increase `--timeout-image` if needed
- **Pod scheduling** – Depends on node availability; ensure at least one node can run the guest
- **Network** – Image download from Ubuntu cloud images may be slow or intermittent
