# Sandbox outputs

> **Status: in development for v0.19.** The API below is in place, but this
> version does not export yet: a SwiftSandbox with `spec.outputs` fails with
> reason `OutputsNotSupported` before anything runs.

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

## Status

`status.phase` is `Exporting` between the workload's exit and the end of the
export, then `Completed` (every output exported) or `Failed`.

Each `status.outputs[]` entry has a `state`: `Pending`, `Collecting`,
`Packaged`, `Uploading`, `Exported`, `Skipped` (the workload did not exit 0)
or `Failed` (with `reason`). An exported output reports `reference`
(`repository@sha256:...`), `digest`, `contentDigest`, `size`,
`contentBytes` and `entries`.
