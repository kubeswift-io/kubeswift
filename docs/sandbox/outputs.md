# Sandbox outputs

> **Status: new in v0.19.** Needs the `kernels/sandbox:6.6.17` or
> `kernels/gpu-sandbox:6.6.6` kernel (bridge feature `outputs`); an older
> kernel fails the sandbox with `KernelUnsupported`.

A workload in a SwiftSandbox can write files to a directory it declares in
`spec.outputs`. When the workload exits 0, KubeSwift collects that directory,
packages it, and pushes it as an OCI artifact. The guest never sees the push
credentials. The result is reported by digest in `status.outputs`, and can be
mounted later by `spec.artifacts` or prepared by a SwiftArtifactPlacement.

Typical uses: build outputs, compiled caches, generated indexes or
configuration bundles.

## What export does not do

Export moves bytes and creates an immutable artifact. It does not make the
content trustworthy: the workload decides what it writes. To consume an
exported artifact as trusted, have a separate signer check it and sign its
digest, then require that signature with a
[SwiftArtifactVerificationPolicy](artifact-verification-policy.md). KubeSwift
never signs exported artifacts.

## Declaring an output

```yaml
apiVersion: sandbox.kubeswift.io/v1alpha1
kind: SwiftSandbox
metadata:
  name: build
  namespace: team-a
spec:
  image: registry.example.com/tools/builder@sha256:...
  command: ["/bin/build", "--out", "/output"]
  outputs:
  - name: compiled
    path: /output
    # repository: registry.example.com/platform/outputs/team-a/compiled
    maxSize: 512Mi
```

| Field | Meaning |
|---|---|
| `name` | DNS label, at most 40 characters, unique. |
| `path` | Absolute guest directory. Not `/` or `/proc`, `/sys`, `/dev`; must not overlap another output, an artifact, the model or a secret file. The workload finds it empty and writable. |
| `repository` | Optional. Registry host and path, no tag or digest, in canonical form. Unset: the policy's `defaultRepository` + `/<name>`. |
| `maxSize` | Bytes of files. Default and ceiling come from the policy. |
| `maxEntries` | Files and directories. Default and ceiling come from the policy. |
| `artifactType` | Media type for the manifest. Default `application/vnd.kubeswift.sandbox-output.v1`. |
| `annotations` | Up to 8, copied onto the manifest. They are the author's statements; do not trust them in a signer. `org.opencontainers.image.*` and `io.kubeswift.*` are reserved. |

At most 4 outputs. `spec.outputs` cannot be changed after creation. Outputs
are required: if an export fails, the sandbox fails (`OutputExportFailed`).

A sandbox with outputs boots cold even with `poolRef`: warm slots have no
output share.

## Where outputs may go: SwiftArtifactExportPolicy

The destination is an administrator's decision. A cluster-scoped
SwiftArtifactExportPolicy names the namespaces that may export, the
repositories they may use, and the credential used to push:

```yaml
apiVersion: sandbox.kubeswift.io/v1alpha1
kind: SwiftArtifactExportPolicy
metadata:
  name: platform-outputs
spec:
  namespaceSelector:
    matchLabels:
      platform.example.com/exports: "true"
  repositories:
  - registry.example.com/platform/outputs/{namespace}/**
  defaultRepository: registry.example.com/platform/outputs/{namespace}
  credentialsSecretRef:
    name: platform-output-push
  maxSize: 1Gi
```

- A path segment that is exactly `{namespace}` stands for the sandbox's
  namespace, so one policy gives each namespace its own path and no namespace
  can push into another's.
- The registry host must be literal. Globs: `*` within a path segment, `**`
  across segments.
- A repository is used only if a policy selecting the namespace allows it.
  If policies with different credentials allow it, the output is refused as
  ambiguous rather than guessing.
- No policy selects the namespace: outputs are refused.
- Use namespace labels only administrators can set.

## Credentials

The push credential is a `kubernetes.io/dockerconfigjson` Secret in the
KubeSwift namespace, labelled `kubeswift.io/export-credentials=true` (a
policy can only name a Secret set aside for this). Only the export Job, which
runs in the KubeSwift namespace with no service account token, mounts it.
Tenants, the launcher pod and the guest never see it. Prefer a credential
that can push only under the export repositories.

## How it works

1. **Admission.** Before anything is created, each output's repository is
   resolved against the policies, its limits are clamped to the policy's, and
   the credential Secret is checked. `status.outputs[]` records the repository
   and the policy (name and UID). A refusal fails the sandbox with no launcher.
2. **Run.** The launcher gives each output an empty, size-capped filesystem
   on the node, shared into the guest at `path`. The guest writes there and
   nowhere else counts.
3. **Collection.** Only after the workload exits 0 and the microVM is off,
   the launcher walks the output and packages it as a canonical tar
   (regular files and directories only; symlinks, devices, FIFOs, sockets and
   hard links refuse the output). `spec.timeout` and liveness no longer apply.
4. **Export.** A Job in the KubeSwift namespace, on the same node, re-checks
   the package, compresses it deterministically and pushes it over HTTPS. The
   destination and limits come from the controller, re-authorized against the
   recorded policy UID just before the Job is created: a policy deleted or
   recreated in between fails the output (`OutputDestinationRevoked`), it is
   never redirected.
5. **Cleanup.** The package on the node is removed, then the sandbox is
   `Completed` or `Failed`.

The artifact is an OCI image manifest with an empty config, one
`tar+gzip` layer, the output's `artifactType`, its annotations plus
`io.kubeswift.export.namespace` and `io.kubeswift.export.output`, and no
timestamp. The same files give the same digest. It is tagged
`<sandbox UID>.<output name>`; refer to it by digest.

It mounts with `spec.artifacts` (`layout: unpacked`) and can be prepared by a
SwiftArtifactPlacement.

## Status

`status.phase` is `Exporting` between the workload's exit and the end of the
export, then `Completed` (every output exported) or `Failed`.

Each `status.outputs[]` entry has a `state`: `Pending`, `Collecting`,
`Packaged`, `Uploading`, `Exported`, `Skipped` (the workload did not exit 0)
or `Failed` (with `reason`). An exported output reports `reference`
(`repository@sha256:...`), `digest`, `contentDigest`, `size`,
`contentBytes` and `entries`.

Reasons on a failed output:

| Reason | Meaning |
|---|---|
| `OutputDestinationDenied` / `OutputDestinationAmbiguous` | No policy allows the repository, or several with different credentials do. |
| `OutputLimitExceedsPolicy` | `maxSize` or `maxEntries` is above the policy's. |
| `OutputCredentialsMissing` | The policy's Secret is absent or not labelled. |
| `OutputDestinationRevoked` | The policy changed between admission and push. |
| `OutputInvalidEntry` | A symlink, device, special file or bad path in the output. |
| `OutputTooLarge` / `OutputTooManyEntries` | Over the limits. |
| `OutputCollectionFailed` | The launcher could not package the output, or the exit code was not reported. |
| `RegistryUnauthorized` | The registry refused the credential (not retried). |
| `RegistryUnavailable` | The push failed after retries. |
| `UploadTimeout` | The push took longer than the policy's `uploadTimeout`. |
| `OutputLost` | The node holding the package is gone. |

Deleting a sandbox while it exports stops the push and removes the package.

## Operating

- Export Jobs and spool cleanup pods run in the KubeSwift namespace, named
  after the sandbox UID. `--max-concurrent-exports` (default 4) caps the Jobs
  running at once.
- Packages wait under `/var/lib/kubeswift/sandbox-outputs/<sandbox UID>/` on
  the node until pushed.
- Metric: `kubeswift_sandbox_output_exports_total{result,reason}`.
