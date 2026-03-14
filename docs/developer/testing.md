# Testing

## Smoke test

End-to-end: apply samples, wait for SwiftImage Ready and SwiftGuest Running, assert conditions.

```bash
make smoke-test
# or
./test/smoke/boot-test.sh [--timeout-image 15] [--timeout-guest 5] [--no-cleanup]
```

| Option | Default | Description |
|--------|---------|-------------|
| `--timeout-image` | 15 | Minutes for SwiftImage Ready |
| `--timeout-guest` | 5 | Minutes for SwiftGuest Running |
| `--no-cleanup` | — | Leave resources for inspection |

`NAMESPACE=my-ns` to override namespace.

**Prerequisites:** KubeSwift deployed, worker nodes with KVM, [preflight](../operator/worker-node-preflight.md) passing. First run: image import can take 5–15 min.

[Smoke verification](../operator/smoke-verification.md)

## Unit tests

```bash
go test ./...
go test ./internal/controller/swiftguest/...
go test ./internal/seed/...
```

Tests: `internal/controller/swiftguest/status_test.go`, `internal/controller/swiftimage/*_test.go`, `internal/seed/*_test.go`, `internal/runtimeintent/build_test.go`, `internal/resolved/*_test.go`.
