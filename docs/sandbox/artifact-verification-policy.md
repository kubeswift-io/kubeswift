# Artifact verification policy

A `SwiftArtifactVerificationPolicy` is a cluster-scoped rule an administrator
writes for the OCI artifacts that sandboxes mount (`spec.artifacts`) and that
placements prepare (`SwiftArtifactPlacement`). In the namespaces it selects,
an artifact from a repository it governs is materialized only if it carries a
signature by one of the policy's keys, with the policy's signed claims.

A workload's own `verifyKeySecretRef` lets a tenant check what it mounts.
A policy is for content the platform itself vouches for: native code,
compiled output, anything a platform component or a user relies on because
"the platform checked it". The tenant cannot choose, skip or weaken it.

```yaml
apiVersion: sandbox.kubeswift.io/v1alpha1
kind: SwiftArtifactVerificationPolicy
metadata:
  name: platform-caches
spec:
  namespaceSelector:
    matchLabels: {platform.example.com/trusted-artifacts: "true"}
  repositories:
    - registry.example.com/caches/**
  otherRepositories: Allow      # or Deny
  digestPinning: Required       # default
  authorities:
    - name: release-2026
      key:
        publicKey: |
          -----BEGIN PUBLIC KEY-----
          MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE...
          -----END PUBLIC KEY-----
  claims:
    - key: example.com/kind
      value: compiled-cache
    - key: example.com/runtime
      values: [rt-1.4.2, rt-1.4.3]
    - key: example.com/consumer
      fromNamespace: true
    - key: example.com/source
      fromArtifact: source
```

## Which artifacts it governs

A policy governs an artifact when its `namespaceSelector` matches the labels
of the artifact's namespace and the artifact's repository matches one of its
`repositories`. Every policy that governs an artifact must be satisfied.
Nothing in a SwiftSandbox or a placement names a policy, so a tenant cannot
pick one, leave one out, or swap one for another.

- `repositories` are globs over the canonical repository: registry host and
  path, lowercase, as `registry.example.com/caches/app` or
  `index.docker.io/library/busybox`. `*` matches within one path segment,
  `**` any number of segments.
- `otherRepositories: Deny` refuses, in the selected namespaces, any artifact
  that no policy governs. Without it (`Allow`), such artifacts mount as
  before, checked only by their own `verifyKeySecretRef`. With `Allow`, a
  tenant can still mount a copy of an artifact from a repository no policy
  governs; it then carries no platform trust. Use `Deny` where the namespace
  must only mount platform-verified artifacts, or let consumers ask for
  `requirePlatformVerification`.
- Select on labels that only administrators can set. Namespace admins bound
  to the built-in `admin` role cannot change Namespace labels;
  `kubernetes.io/metadata.name` cannot be changed at all.
- A namespace no policy selects behaves exactly as before. Installing or
  upgrading KubeSwift creates no policy.

## What is verified

KubeSwift runs `cosign verify --key` for each of a policy's keys, in order,
until one passes, and reads the payloads cosign returns for that key. A
policy is satisfied when one such payload:

- is for the artifact's manifest digest, checked again by KubeSwift
  (`critical.image.docker-manifest-digest`); and
- carries every required claim with an accepted value.

Claims are the signature's annotations, written by the signer:

```bash
cosign sign --key platform.key \
  -a example.com/kind=compiled-cache \
  -a example.com/runtime=rt-1.4.3 \
  -a example.com/consumer=team-a \
  -a example.com/source=sha256:9f86d0... \
  registry.example.com/caches/app@sha256:...
```

With cosign 3 the annotations are subject annotations of the in-toto
statement inside the signed DSSE envelope; with cosign 2 they are the
`optional` map of the signed simple-signing payload. Either way they are
inside the signed bytes, and the statement or payload names the artifact's
digest. Annotations on the OCI manifest itself are never read. All claims
must come from one payload: claims from two signatures never combine, and a
signature by any key other than the policy's is ignored.

A claim's expected value is:

| Source | Expected value |
|---|---|
| `value` | that string |
| `values` | any one of them |
| `fromNamespace: true` | the name of the namespace the artifact is mounted in |
| `fromArtifact: <name>` | the artifact's own `claims[<name>]`; an artifact without it is refused |

Keys and values are plain strings (keys `[A-Za-z0-9._/-]`, values
`[A-Za-z0-9._:/@+-]`); KubeSwift attaches no meaning to them.

Governed artifacts must be referenced by digest unless the policy sets
`digestPinning: Optional`; a tag is refused (`TagReference`).

## Workload fields

```yaml
spec:
  artifacts:
    - name: cache
      ref: registry.example.com/caches/app@sha256:...
      mountPath: /run/cache
      optional: true
      claims:
        source: sha256:9f86d0...       # fills fromArtifact: source
      requirePlatformVerification: true
```

- `claims` supplies the values a policy reads with `fromArtifact` (the entry
  named in `fromArtifact`). Every other entry is a claim the platform-verified
  signature must also carry, under that key with that value; one that
  contradicts a policy's own value is refused (`ClaimConflict`). An artifact
  with `claims` that no policy governs is refused (`NoMatchingPolicy`):
  nothing would check them.
- `requirePlatformVerification: true` refuses the artifact when no policy
  governs it in this namespace, so a consumer that relies on platform trust
  is told when there is none.
- `verifyKeySecretRef` keeps working and is checked as well: an artifact
  must pass every governing policy and its own key.

These fields can only add requirements. The same two fields exist on
`SwiftArtifactPlacement` artifacts.

## Optional artifacts

Optional never means unverified. An optional artifact that a policy refuses,
whose signature does not verify, or whose claims do not match is skipped:
the workload starts and nothing is mounted at `mountPath`. A required
artifact fails the sandbox instead.

## Warm checkouts and placements

Verification is recorded in the node's artifact cache per digest,
repository, policy, key and claims. A warm checkout mounts an entry only if
the record for the current policy exists, which the launcher checks on the
node; it does no network or cryptographic work. A record made under another
policy, another key no longer trusted, other claims, or another namespace's
`fromNamespace` value is never reused.

A `SwiftArtifactPlacement` verifies under the same policies as a checkout in
its namespace, and reports a node `Prepared` only once every governing policy
has passed there. Prepared checkouts keep their speed.

Warm slots started by a KubeSwift release before v0.18 cannot check policy
records; a governed artifact is never dispatched to one. A checkout of a
required governed artifact boots cold instead, and an optional one starts
without it (`LauncherUnsupported`), until the pool has replaced those slots.
Required artifacts that fail report `ArtifactPolicyDenied`,
`ArtifactVerificationFailed` or `ArtifactClaimMismatch` as the sandbox's
reason.

## Rotating keys

1. Add the new key as a second authority. Existing records stay valid;
   nothing is verified again.
2. Sign artifacts with the new key, alone or as well as the old one.
3. Remove the old key. Entries verified only with it are verified again on
   their next use, and placements prepare their nodes again in the
   background. An artifact signed only by the old key is now refused.

Changing `claims` re-verifies the artifacts the policy governs. Changing
`namespaceSelector`, `repositories` or `otherRepositories` re-verifies
nothing; the next checkout follows the new binding. Running sandboxes are
never touched: their artifacts were verified when they were mounted.

## Status and failures

`kubectl get savp` shows whether each policy is `Ready`, and how many
namespaces it selects. `status.authorities` lists each key's `keyID`, the
sha256 of its SubjectPublicKeyInfo:
`openssl pkey -pubin -in key.pub -outform der | sha256sum` gives the same. A policy that is
not Ready (a key that does not parse, for example) refuses the artifacts it
governs; it never lets them through unchecked.

An artifact that fails reports it in `status.artifacts[].reason`, with an
Event:

| Reason | Meaning |
|---|---|
| `PolicyDenied` | refused before any fetch; the message says why: `NoMatchingPolicy`, `RepositoryNotAllowed`, `TagReference`, `PolicyInvalid`, `MissingClaimValue`, `ClaimConflict` |
| `VerificationFailed` | no signature by a policy key for this digest |
| `ClaimMismatch` | a policy key signed it, without the required claims |

Registry refusals and fetch failures keep their own reasons
(`ArtifactResolveFailed`, `ArtifactMaterializeFailed`).
`status.artifacts[].policies` names the policies that governed the artifact,
with their generations. Metric:
`kubeswift_artifact_policy_decisions_total{path,result}`.

## What it does not defend against

- Principals who can create privileged or hostPath pods, or modify launcher
  pods, are trusted with the node and can write its cache directly.
- A policy protects artifacts, not root filesystem images (`spec.image`) or
  `spec.model`.
- Keyless (Fulcio/OIDC) signatures are not supported yet; authorities are
  public keys. Verification never contacts a transparency log.
