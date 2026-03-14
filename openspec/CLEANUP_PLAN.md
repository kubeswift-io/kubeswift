# OpenSpec Changes Cleanup Plan

**Context:** Compare active changes against archived `boot-first-kubeswift-guest`, current implementation, and synced specs.

**Archived change:** `2026-03-10-boot-first-kubeswift-guest`  
**Synced specs:** `openspec/specs/boot-first-guest/spec.md` (9 requirements)

---

## 1. create-kubeswift-architecture-foundation

| Criterion | Assessment |
|-----------|------------|
| **Superseded by boot-first?** | No. boot-first was integration-focused; it did not create architecture docs. |
| **Unique requirements not implemented** | Architecture documentation in `docs/architecture/`; recorded decisions (language split, API groups, runtime model). `docs/repo-layout.md` exists but `docs/architecture/` does not. |
| **Recommendation** | **Archive** – Architecture is captured in `openspec/config.yaml` context and embedded in subsequent designs. Creating `docs/architecture/` can be a small follow-up if needed. |
| **Evidence** | No `docs/architecture/`; `docs/repo-layout.md` describes layout only; config.yaml has full domain/architecture context. |

---

## 2. bootstrap-kubeswift-monorepo

| Criterion | Assessment |
|-----------|------------|
| **Superseded by boot-first?** | Yes. boot-first apply implemented bootstrap as a prerequisite. |
| **Unique requirements not implemented** | None. All bootstrap deliverables exist. |
| **Recommendation** | **Archive** – Fully implemented. |
| **Evidence** | `api/`, `cmd/`, `internal/`, `config/`, `rust/`, `docs/` exist; `go.mod`, `Cargo.toml` workspace, placeholder binaries (controller-manager, webhook-server, swiftctl, swiftletd); `Makefile` with build targets; `docs/repo-layout.md`. |

---

## 3. add-core-kubeswift-api-types

| Criterion | Assessment |
|-----------|------------|
| **Superseded by boot-first?** | Yes. boot-first apply implemented core API types as a prerequisite. |
| **Unique requirements not implemented** | None. API types and CRDs are complete. |
| **Recommendation** | **Archive** – Fully implemented. |
| **Evidence** | `api/swift/v1alpha1/` (SwiftGuest, SwiftGuestClass), `api/image/v1alpha1/` (SwiftImage), `api/seed/v1alpha1/` (SwiftSeedProfile); `config/crd/bases/` with CRDs; `config/samples/` with YAML; `make generate` works. |

---

## 4. add-api-validation-and-defaulting

| Criterion | Assessment |
|-----------|------------|
| **Superseded by boot-first?** | No. boot-first did not implement webhooks. |
| **Unique requirements not implemented** | Admission webhook validation and defaulting; `internal/webhook/` handlers; ValidatingWebhookConfiguration, MutatingWebhookConfiguration. |
| **Recommendation** | **Keep active** – Not implemented; required for production but not for first boot. |
| **Evidence** | `internal/webhook/` exists but is empty (no .go files). boot-first-guest spec does not require webhooks. |

---

## 5. add-swiftguest-resolver

| Criterion | Assessment |
|-----------|------------|
| **Superseded by boot-first?** | No. boot-first did not implement the resolver. |
| **Unique requirements not implemented** | `internal/resolved/` package; ResolvedGuest types; merge logic; compatibility checks. |
| **Recommendation** | **Keep active** – Not implemented; prerequisite for SwiftGuest controller and boot-first end-to-end flow. |
| **Evidence** | `internal/resolved/` exists but is empty. boot-first-guest spec requires "resolver produces ResolvedGuest"; runtimeintent.Build takes a ResolvedGuest interface with no implementation. |

---

## 6. implement-swiftimage-controller

| Criterion | Assessment |
|-----------|------------|
| **Superseded by boot-first?** | No. boot-first did not implement the SwiftImage controller. |
| **Unique requirements not implemented** | `internal/controller/swiftimage/`; lifecycle phases; http/pvcClone import; preparedArtifact in status. |
| **Recommendation** | **Keep active** – Not implemented; prerequisite for SwiftImage Ready and boot-first end-to-end flow. |
| **Evidence** | No `internal/controller/swiftimage/` directory. boot-first-guest spec requires "SwiftImage reaches Ready with preparedArtifact". |

---

## 7. implement-seed-rendering-for-nocloud

| Criterion | Assessment |
|-----------|------------|
| **Superseded by boot-first?** | No. boot-first did not implement seed rendering. |
| **Unique requirements not implemented** | Control-plane ConfigMap creation (user-data, meta-data, network-config); Secret/ConfigMap ref resolution. |
| **Recommendation** | **Keep active** – Not implemented; prerequisite for seed-attached guests. |
| **Evidence** | No seed rendering logic in `internal/controller/swiftguest/` or `internal/seed/`. boot-first-guest spec requires "seed ConfigMap created and mounted". |

---

## 8. add-launcher-pod-and-runtime-intent

| Criterion | Assessment |
|-----------|------------|
| **Superseded by boot-first?** | Partially. boot-first implemented runtime intent and mount path alignment. |
| **Unique requirements not implemented** | SwiftGuest controller reconcile loop; pod creation; status mapping (PodScheduled, phase); intent ConfigMap creation and mount. |
| **Recommendation** | **Replace with smaller follow-up** – "complete-swiftguest-controller" or "swiftguest-controller-reconcile": pod creation, status mapping, intent ConfigMap. Runtime intent types and mount paths are done. |
| **Evidence** | `internal/runtimeintent/` complete (types, build, serialize, tests); `internal/controller/swiftguest/constants.go` and `pod.go` (AddVolumeMounts) exist. No controller.go, no reconcile, no pod creation. |

---

## 9. implement-swiftletd-mvp

| Criterion | Assessment |
|-----------|------------|
| **Superseded by boot-first?** | Partially. boot-first implemented intent path, parsing, and error handling. |
| **Unique requirements not implemented** | swift-seed NoCloud builder; swift-ch-client CH API; swift-runtime dir setup; CH process spawn; process monitoring; status reporting (patch SwiftGuest). |
| **Recommendation** | **Replace with smaller follow-up** – "complete-swiftletd-mvp": NoCloud generation, CH launch, monitoring, status reporting. Intent parsing and paths are done. |
| **Evidence** | `rust/swiftletd/src/intent.rs` has load_intent, INTENT_PATH, parsing; `main.rs` reads intent and exits. `rust/swift-seed`, `rust/swift-ch-client`, `rust/swift-runtime` are placeholder stubs only. |

---

## Summary Table

| Change | Superseded? | Unique requirements | Recommendation |
|--------|-------------|---------------------|----------------|
| create-kubeswift-architecture-foundation | No | docs/architecture/ | Archive |
| bootstrap-kubeswift-monorepo | Yes | None | Archive |
| add-core-kubeswift-api-types | Yes | None | Archive |
| add-api-validation-and-defaulting | No | Webhooks | Keep active |
| add-swiftguest-resolver | No | internal/resolved | Keep active |
| implement-swiftimage-controller | No | SwiftImage controller | Keep active |
| implement-seed-rendering-for-nocloud | No | Seed ConfigMap | Keep active |
| add-launcher-pod-and-runtime-intent | Partially | Pod creation, status | Replace with follow-up |
| implement-swiftletd-mvp | Partially | NoCloud, CH, status | Replace with follow-up |

---

## Recommended Actions

1. **Archive (3):** create-kubeswift-architecture-foundation, bootstrap-kubeswift-monorepo, add-core-kubeswift-api-types  
2. **Keep active (4):** add-api-validation-and-defaulting, add-swiftguest-resolver, implement-swiftimage-controller, implement-seed-rendering-for-nocloud  
3. **Replace with follow-ups (2):**
   - Archive add-launcher-pod-and-runtime-intent; create `complete-swiftguest-controller` (pod creation, status mapping, intent ConfigMap)
   - Archive implement-swiftletd-mvp; create `complete-swiftletd-mvp` (NoCloud, CH launch, monitoring, status reporting)

---

## Dependency Order for Remaining Work

To satisfy boot-first-guest spec end-to-end:

1. add-swiftguest-resolver (ResolvedGuest)
2. implement-swiftimage-controller (SwiftImage Ready, preparedArtifact)
3. implement-seed-rendering-for-nocloud (seed ConfigMap)
4. complete-swiftguest-controller (pod, intent, status)
5. complete-swiftletd-mvp (NoCloud, CH, status)

add-api-validation-and-defaulting can be done in parallel or after first boot works.
