## Context

KubeSwift has a first-boot flow: SwiftImage import → SwiftGuest resolution → pod creation → swiftletd launch → Cloud Hypervisor runs the VM. The existing `test/smoke/boot-test.sh` applies samples and waits for Ready/Running but does not verify each stage explicitly. `docs/first-boot.md` describes the flow and troubleshooting but lacks exact commands and failure checks. Developers need a reproducible verification path with documented prerequisites, manifests, commands, expected conditions, and failure checks.

## Goals / Non-Goals

**Goals:**

- Provide a verification-focused smoke test that validates each stage of the first-boot flow
- Document exact local cluster prerequisites (CRDs, controllers, swiftletd image, RBAC, node requirements)
- Define verification steps with exact manifests, commands, expected conditions, and failure checks
- Keep the verification path reproducible for a developer working locally
- Align all docs and scripts with github.com/projectbeskar/kubeswift

**Non-Goals:**

- Production CI/CD integration
- Performance benchmarks
- Migration, snapshots, or Windows support

## Decisions

### 1. Single script with staged verification

**Decision:** Extend `test/smoke/boot-test.sh` to perform staged verification (SwiftImage → SwiftGuest scheduling → seed → swiftletd → status conditions) rather than only waiting for final Running.

**Rationale:** One script keeps the path simple; staged checks make failures easier to diagnose. Alternative: separate scripts per stage—rejected as more complex for local use.

### 2. Prerequisites document as canonical source

**Decision:** Add `docs/smoke-verification.md` (or equivalent) as the canonical document for local cluster prerequisites, exact commands, expected conditions, and failure checks. Update `docs/first-boot.md` to reference it where appropriate.

**Rationale:** Centralizing prerequisites and commands avoids drift between script and docs. `first-boot.md` remains the user-facing walkthrough; smoke-verification doc is the verification reference.

### 3. Use existing config/samples manifests

**Decision:** Use `config/samples/` manifests (swiftguestclass-default, swiftimage-http, swiftseedprofile-minimal, swiftguest-sample) as the source of truth for the smoke test. No new sample manifests unless needed for verification-specific cases.

**Rationale:** Same manifests users apply manually; keeps verification aligned with real usage.

### 4. Failure checks via kubectl describe and logs

**Decision:** On failure, the script MUST output `kubectl describe` and relevant `kubectl logs` for the failing resource. Docs MUST document these checks and common failure causes.

**Rationale:** Developers need actionable diagnostic output; describe and logs are the standard Kubernetes debugging path.

### 5. RBAC and namespace

**Decision:** Smoke test MUST apply `config/rbac/` in the test namespace before creating SwiftGuest. Document this in prerequisites.

**Rationale:** swiftletd requires RBAC for status reporting; omitting it causes silent failures.

## Risks / Trade-offs

| Risk | Mitigation |
|------|------------|
| Image URL unreachable or slow | Document timeout and optional `--timeout-image`; suggest local mirror if needed |
| Node lacks Cloud Hypervisor | Document node requirement; script can optionally check node readiness |
| kind/minikube storage limits | Document PVC size and node disk requirements |
| Network policy blocking image download | Document as out-of-scope; assume default cluster network |
