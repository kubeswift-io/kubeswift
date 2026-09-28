# Release v0.15.0

Run 2026-09-28, 12:44–13:07 UTC, per PLAN.md "Release: tag v0.15.0".

**Verdict: RELEASED.** The signed tag `v0.15.0` is on `37b8c27`. Release
Stable and Verify Release both succeeded, and every published artifact checks
out.

| Step | Result |
|---|---|
| 1. Tag and push | **Done.** Tag object `b868465`, which points at `37b8c27`; signature verified; pushed 12:44:53 |
| 2. Release run | **Release Stable: success**, 2 jobs, 21 min 5 s. **Verify Release: success**, 27 s. It had to be dispatched by hand (see below) |
| 3. Published artifacts | **All pass:** GitHub release, assets, chart, 9/9 image signatures, binary signature and checksums, standalone manifests |

The clusters were **not** upgraded to the stable chart, as the plan says.

## 1. Tag and push

```text
git fetch origin
origin/main = 37b8c27e9ebc715d2f713a8302083ae870679bbf   (exactly; not moved)
8afbad3 (validated) is an ancestor; 8afbad3..37b8c27 = #695 docs sweep, #690 codeql-action bump, #693 CHANGELOG
  git diff 8afbad3 37b8c27 -- '*.go' rust/ charts/kubeswift/templates/ charts/kubeswift/crds/  -> empty
v0.15.0 before tagging: absent locally and on origin (checked again right before the push)

git tag -s v0.15.0 -m "KubeSwift v0.15.0" 37b8c27e9ebc715d2f713a8302083ae870679bbf
git tag -v v0.15.0
  Good "git" signature for william.rizzo@gmail.com with ED25519 key SHA256:ALS0VrMI2tXe5rUJX6PSHkkl432pIt1shD8+axLi/Zs
  object 37b8c27e9ebc715d2f713a8302083ae870679bbf  type commit  tag v0.15.0  tagger wrkode
git push origin v0.15.0    (12:44:53)
  * [new tag] v0.15.0 -> v0.15.0
git ls-remote: b868465bda0c7789d686e31d1b3539c151388647 refs/tags/v0.15.0 ; 37b8c27e… refs/tags/v0.15.0^{}
```

- **Signing.** The tag is signed with William's SSH key, as for v0.14.1.
- **Verifying.** This checkout has no `gpg.ssh.allowedSignersFile`, so `git
  tag -v` needs one. I passed a temporary one with `git -c
  gpg.ssh.allowedSignersFile=…`, holding that key. No git config was changed.
- **Pre-release checks at the tag:**
  - `Chart.yaml` is `version: 0.15.0`, `appVersion: "0.15.0"`;
  - `CHANGELOG.md` has `## [v0.15.0] — 2026-09-28`, which Release Stable
    requires.

## 2. The release run

**Release Stable:** https://github.com/kubeswift-io/kubeswift/actions/runs/36423826557
(started by the tag push)

| Job | Result | Time |
|---|---|---|
| Build and push images + chart | success | 12:45:00 → 13:01:46 (16 m 46 s) |
| Create GitHub Release | success | 13:01:49 → 13:06:02 (4 m 13 s) |
| **Run total** | **success** | 12:44:57 → 13:06:02 (**21 m 5 s**) |

**Verify Release:** https://github.com/kubeswift-io/kubeswift/actions/runs/36426227763

| Job | Result | Time |
|---|---|---|
| Verify v0.15.0 (Install cosign; Verify signatures + attestations) | success: "all 9 images verified for v0.15.0" | 13:06:21 → 13:06:42 |
| **Run total** | **success** | 13:06:16 → 13:06:43 (**27 s**) |

- **Verify Release is not started by the tag push.** The plan says it follows
  Release Stable, but `verify-release.yaml` is `workflow_dispatch`-only, with
  a required `tag` input. Release Stable does not dispatch it either.
- I dispatched it once Release Stable had finished: `gh workflow run
  verify-release.yaml --ref main -f tag=v0.15.0`.
- It is read-only (`contents: read`).

## 3. What was published

| What | Result |
|---|---|
| **GitHub release** | https://github.com/kubeswift-io/kubeswift/releases/tag/v0.15.0: "KubeSwift v0.15.0", **not a draft, not a pre-release**, published 13:05:59. The body **starts with** `## [v0.15.0] — 2026-09-28` and contains the whole CHANGELOG section verbatim (674 lines: intro, Upgrade with "Seven changes may need action", Security, Fixed, Changed, Docs, CI). Then come **Install**, **Upgrade**, **Images**, **swiftctl** and **Supply chain**, then GitHub's "What's Changed" |
| **Release assets** | `swiftctl-darwin-amd64`, `swiftctl-darwin-arm64`, `swiftctl-linux-amd64`, `SHA256SUMS`, `SHA256SUMS.sig`, `SHA256SUMS.pem`: the same names as v0.14.1 |
| **Chart** | `helm show chart oci://ghcr.io/kubeswift-io/charts/kubeswift --version 0.15.0`: `name: kubeswift`, `version: 0.15.0`, `appVersion: 0.15.0` (digest `sha256:c32ebe49…`) |
| **Images** | The plan's `cosign verify` loop (identity `release-(stable\|rc).yaml@refs/tags/v.*`): **OK × 9**, for `controller-manager`, `swiftletd`, `gpu-discovery`, `migration-stunnel`, `kubeswift-gateway`, `kubeswift-dra-driver`, `sandbox-materialize`, `snapshot-s3` and `snapshot-oras` |
| **Binaries** | `cosign verify-blob --signature SHA256SUMS.sig --certificate SHA256SUMS.pem`, with the identity pinned to `release-stable.yaml@refs/tags/v0.15.0`: **Verified OK**. Then `sha256sum -c SHA256SUMS`: **OK** for all three binaries. `swiftctl-linux-amd64 --help` runs |
| **Standalone manifests** | At the tag, `config/dra-driver/dra-driver.yaml` pins `kubeswift-dra-driver:v0.15.0`, and `config/daemonset/gpu-discovery.yaml` pins `gpu-discovery:v0.15.0`. **Both image tags exist** (`oras manifest fetch`) |

```text
SHA256SUMS
ccfd643e81d05c3ba954df41497861196b0765dd4d6b9ad4b5b664b1a27f62ff  swiftctl-darwin-amd64
30e52733f1e721b26ddbeeb2e8807a5e7b02b00ec9543ba475b172d04ba257b5  swiftctl-darwin-arm64
00555e6d4dbd4dceb78b7d80a251ba2322c820e5975b6d3fcbfd1cdda7578009  swiftctl-linux-amd64
```

## Notes

- **Verify Release has to be dispatched by hand.** Future release steps
  should say so, as the plan assumed it would run by itself.
- **The clusters stay on the dev chart.** dev, ntx and sov are still on
  `0.0.0-dev.8afbad3`, with the same code as `v0.15.0`. Moving them to the
  stable chart is William's call.
