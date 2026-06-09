# Secrets — seed user-data in Git

SwiftSeedProfile `userData` is cloud-init config and frequently carries
credentials (password hashes, tokens, SSH private keys for agents). **Never
commit those in plaintext.**

## Pattern: userDataFrom + SOPS-encrypted Secret

SwiftSeedProfile supports `userDataFrom` (and `metaDataFrom`/
`networkDataFrom`) referencing a Secret/ConfigMap instead of inlining:

```yaml
apiVersion: seed.kubeswift.io/v1alpha1
kind: SwiftSeedProfile
metadata: { name: app-seed }
spec:
  datasource: NoCloud
  userDataFrom:
    secretKeyRef: { name: app-seed-userdata, key: user-data }
```

Encrypt the Secret in Git with [SOPS](https://fluxcd.io/flux/guides/mozilla-sops/)
(Flux decrypts at apply time via `spec.decryption` on the Kustomization) or use
[sealed-secrets](https://github.com/bitnami-labs/sealed-secrets). The reference
example's `default` seed inlines only a public SSH key — public keys are fine
in Git; everything else goes through the Secret path.
