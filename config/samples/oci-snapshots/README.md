# OCI-backend snapshots

`SwiftSnapshot.spec.backend.type: oci` packages a memory+disk snapshot as an OCI
artifact and pushes it to a registry via ORAS. The registry is an external
dependency — bring your own (Harbor / Zot / distribution / a cloud registry).
For an in-cluster test registry, see `config/samples/golden-image/` and
`config/samples/edge-zot/`.

Restore pulls the artifact (pinned by digest) into a node-local cache and boots
from it, so a clone can land on any node. This is the mechanism behind
`swiftctl guest export`/`import` — see `docs/snapshots/cold-migration.md`.

Prerequisites: the `ubuntu-noble` SwiftImage (Ready) and the
`snapshot-local-test-seed` SwiftSeedProfile
(`config/samples/local-snapshots/01-seed-profile.yaml`).

## Apply order

```bash
kubectl apply -f config/samples/local-snapshots/01-seed-profile.yaml
kubectl apply -f config/samples/oci-snapshots/02-source-guest.yaml
kubectl get swiftguest snapshot-oci-source -w      # wait for Running + IP
kubectl apply -f config/samples/oci-snapshots/03-snapshot.yaml
kubectl get swiftsnapshot snapshot-oci-mem -w      # Pending -> Capturing -> Uploading -> Ready
kubectl apply -f config/samples/oci-snapshots/04-restore-clone.yaml
kubectl get swiftrestore snapshot-oci-clone -w     # Downloading -> Restoring -> Resuming -> Ready
```

`insecure: true` in the sample targets a plaintext in-cluster registry — drop it
(and supply a `credentialsSecretRef`) for a real TLS registry. Set
`signingKeySecretRef` to cosign-sign the pushed artifact.
