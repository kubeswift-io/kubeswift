# The registry as the artifact store

If you are already running Flux, you are already pulling KubeSwift from an OCI
registry: `oci://ghcr.io/kubeswift-io/charts/kubeswift` is an OCI artifact, and
the reconciler resolves it, optionally verifies its signature, and applies it.

KubeSwift puts three more artifact types through that same channel. The payoff
is not novelty, it is that one registry, one credential and one signing key
cover the platform and everything it runs.

| Artifact | Kind / field | Direction |
|---|---|---|
| Helm chart | `OCIRepository` (Flux) | pull |
| Golden VM disk | `SwiftImage.spec.source.oci` | pull |
| Kernel + initramfs | `SwiftKernel.spec.ociRef.image` | pull |
| VM snapshot | `SwiftSnapshot.spec.backend.type: oci` | **push** |

Working manifests for all four:
[`examples/gitops-flux/`](../../examples/gitops-flux/). Producer-side detail
lives in [`docs/registry/golden-images.md`](../registry/golden-images.md) and
[`docs/snapshots/cold-migration.md`](../snapshots/cold-migration.md); this page
is only about what changes when a reconciler owns the manifests.

## Pin by digest, for the reason you already pin the chart

A tag is mutable. Someone re-pushing `noble-24.04` changes what every future
guest boots, with no commit in the fleet repository to show for it, and the
change lands only on the clusters that happen to import after the re-push. Two
clusters tracking identical Git state diverge, which is the one thing GitOps is
supposed to make impossible.

```yaml
spec:
  source:
    oci:
      repository: ghcr.io/example-org/vm-images
      digest: sha256:...        # not: tag: noble-24.04
```

`swiftctl image publish` prints the digest. Treat a new disk as a new commit,
the same way you treat a chart bump.

The same argument applies to a `SwiftSandboxPool.spec.image` and to a sandbox
`spec.model`: an unpinned tag makes slot churn nondeterministic, because slots
re-materialize when the resolved digest changes.

## Verify before you trust the bytes

`verifyKeySecretRef` points at a Secret holding a cosign **public** key. The
import verifies the artifact's signature before the bytes become a root disk,
and fails closed if it does not verify.

```yaml
      verifyKeySecretRef:
        name: vm-image-cosign-pub
```

A public key is not a secret, so that Secret belongs in Git as-is. The
*signing* key used by an OCI snapshot export (`signingKeySecretRef`, a cosign
private key plus password) is genuinely secret and needs SOPS or sealed-secrets
like any other, see [secrets.md](secrets.md).

Two constraints worth knowing before you plan around it: cosign cannot verify
over a plaintext registry, so `verifyKeySecretRef` together with `insecure: true`
is rejected at admission; and `insecure` exists for an in-cluster test registry
and nothing else.

## Snapshots are the direction that flows outward

Everything else here is a pull. An OCI snapshot backend is a push, and that is
what makes it useful in a fleet: a CSI snapshot is trapped in the cluster that
took it, while an OCI artifact is portable. The registry your reconciler already
pulls the chart from becomes the transport for moving VM state to another
cluster, to an edge site, or into cold storage, with nothing bespoke in between.

Commit the **schedule**, never the snapshots. A `SwiftSnapshot` is a one-shot
verb that goes terminal; a `SwiftSnapshotSchedule` is desired state:

```yaml
spec:
  schedule: "0 4 * * 0"
  retention: { keepLast: 4 }
  template:
    spec:
      guestRef: { name: db }
      backend:
        type: oci
        oci: { repository: ghcr.io/example-org/vm-snapshots }
```

Restoring is a `SwiftRestore` pinned to the pushed digest, which
`SwiftSnapshot.status.oci` records. It is imperative and one-shot, so it does not
belong in the workloads directory either.

## Credentials

One `kubernetes.io/dockerconfigjson` Secret per namespace covers images,
kernels and snapshot pushes, referenced as `credentialsSecretRef`. It must live
in the same namespace as the resource using it, so a fleet repo spanning
namespaces needs one per namespace, and each one SOPS-encrypted.

This is the same registry credential your `OCIRepository` uses for the chart,
which is worth saying out loud: if the reconciler can pull the platform, the
only thing standing between it and pulling VM disks is that KubeSwift resources
read a Secret rather than the reconciler's own auth.

## What this does not do

- **No cache.** Every cluster imports independently. A registry near your nodes
  is the fix, not a KubeSwift feature.
- **The import is still asynchronous.** OCI skips the download-and-convert of
  the http path, but materializing the disk still takes minutes, so `wait: false`
  on the infra Kustomization still applies. See
  [infrastructure.md](infrastructure.md).
- **Registry outage is a boot-time failure, not a running-VM failure.** Guests
  already running are unaffected; new imports and cold sandbox starts are not.
