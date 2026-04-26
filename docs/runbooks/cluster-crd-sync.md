# Cluster CRD Sync — Runbook

## What this runbook covers

Symptoms that `make deploy` silently skipped a CRD, and how to detect
and recover. Specifically: the **Phase 1 deploy gap** that happened on
2026-04-26, where SwiftSnapshot/SwiftRestore CRDs lived in
`config/crd/bases/` and `charts/kubeswift/crds/` but never reached the
cluster, leaving the controller-manager in CrashLoopBackOff for 76
restarts because it was watching kinds the apiserver didn't know about.

## How the gap happened

`make deploy` runs `kubectl apply -k config/crd`, which uses
`config/crd/kustomization.yaml`. Kustomization resource lists are
hand-maintained — Kustomize does **not** auto-discover files in
`bases/`. When a new CRD lands in `config/crd/bases/` (via
`make generate`) but the contributor forgets to add it to the
kustomization's `resources:` list, the deploy silently no-ops on that
CRD. The chart's `crds/` directory was synced (`cp config/crd/bases/
*.yaml charts/kubeswift/crds/`) but Helm v3 only applies CRDs from a
chart's `crds/` directory on `helm install`, not on `helm upgrade`,
so a chart upgrade against a cluster that was already running an
older chart version would also skip them.

We fixed this in [PR title]:

1. Added the missing CRDs to `config/crd/kustomization.yaml`.
2. Made `make generate` automatically sync to `charts/kubeswift/crds/`
   (was previously a manual step that contributors forgot).
3. Made `make generate` and `make deploy` run `make verify-crd-sync`
   (a script that diffs `config/crd/bases/` against the kustomization's
   resource list and fails loudly on drift).
4. CI runs `make verify-crd-sync` so a future drift fails the PR.

## Symptom — recognize the gap

```
$ kubectl get pods -n kubeswift-system
NAME                                  READY   STATUS             RESTARTS
controller-manager-XXXXXXXXX-XXXXX    0/1     CrashLoopBackOff   76 (4m ago)
gpu-discovery-XXXXX                   1/1     Running            0
```

```
$ kubectl logs -n kubeswift-system controller-manager-XXXXXXXXX-XXXXX --tail=20
E... kind.go:75 "msg"="if kind is a CRD, it should be installed before calling Start"
     "error"="no matches for kind \"SwiftSnapshot\" in version \"snapshot.kubeswift.io/v1alpha1\""
E... kind.go:75 "msg"="if kind is a CRD, it should be installed before calling Start"
     "error"="no matches for kind \"SwiftRestore\" in version \"snapshot.kubeswift.io/v1alpha1\""
E... controller.go:383 "msg"="Could not wait for Cache to sync"
     "error"="failed to wait for swiftsnapshot caches to sync ..."
```

## Triage — confirm the gap

```
# 1. List the CRDs the cluster knows about, filtered to kubeswift.
kubectl get crd | grep kubeswift

# 2. List the CRD bases shipped with the current code.
ls config/crd/bases/

# Diff the two sets. Anything in bases/ but missing from the cluster
# is a CRD the controller-manager expects but can't watch.
```

## Fix — apply the missing CRDs

```bash
# Apply ALL kubeswift CRDs (idempotent; existing ones are unchanged).
kubectl apply -k config/crd

# Wait for them to be Established before continuing.
kubectl wait --for=condition=Established --timeout=30s \
  crd/swiftsnapshots.snapshot.kubeswift.io \
  crd/swiftrestores.snapshot.kubeswift.io
# (Add others as needed.)

# Force the controller-manager to restart now that the CRDs exist.
kubectl rollout restart deployment/controller-manager -n kubeswift-system

# Confirm it comes up clean.
kubectl rollout status deployment/controller-manager -n kubeswift-system --timeout=2m
kubectl get pods -n kubeswift-system
```

The CrashLoopBackOff should clear within ~30s (controller-runtime's
cache-sync timeout) once the CRDs are present.

## Prevent recurrence

Run `make verify-crd-sync` locally before pushing. CI runs it on every
PR. The check fails the build with a list of missing entries:

```
$ make verify-crd-sync
ERROR: config/crd/kustomization.yaml is out of sync with config/crd/bases/

Files in bases/ but missing from kustomization.yaml:
  - bases/snapshot.kubeswift.io_swiftrestores.yaml
  - bases/snapshot.kubeswift.io_swiftsnapshots.yaml

How to fix:
  1. Make sure 'make generate' has been run (regenerates bases/).
  2. Edit config/crd/kustomization.yaml so the resources: list
     matches the contents of config/crd/bases/.
  3. Run 'cp config/crd/bases/*.yaml charts/kubeswift/crds/' to
     keep the Helm chart in lockstep.
  4. Re-run 'make verify-crd-sync' to confirm.
```

`make generate` also calls verify-crd-sync now and fails the codegen
target on drift, so contributors who run `make generate` and forget
the kustomization update get an immediate error instead of a silent
skip at deploy time.

## Helm v3 lifecycle note

Helm v3 only installs CRDs from a chart's `crds/` directory on
`helm install`. On `helm upgrade`, CRDs in that directory are
**ignored**. This is by design (Helm avoids destructive CRD changes
on upgrade), but it means:

- A fresh `helm install` of the chart picks up new CRDs.
- A `helm upgrade` against an existing release does **not**.

Until kubeswift ships CRD-management as Helm templates (with
appropriate hooks to control destructive changes), the operator must
either:

1. Run `kubectl apply -k config/crd` (or
   `kubectl apply -f charts/kubeswift/crds/`) before
   `helm upgrade` on every release that adds or changes a CRD; or
2. Use a CD pipeline that applies the chart's CRDs separately from
   the Helm release.

`make deploy` does the right thing because it explicitly
`kubectl apply -k config/crd` before deploying the chart contents.
This is documented in `docs/development.md`.

## Audit trail — what landed when

This runbook was added on 2026-04-26 alongside the kustomization fix.
The Phase 1 SwiftSnapshot/SwiftRestore CRDs were applied to the
kubeswift-cluster on the same date.
