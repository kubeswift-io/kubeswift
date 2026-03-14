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

### Known flakiness

- **Image import** – First run can be slow; increase `--timeout-image` if needed
- **Pod scheduling** – Depends on node availability; ensure at least one node can run the guest
- **Network** – Image download from Ubuntu cloud images may be slow or intermittent
