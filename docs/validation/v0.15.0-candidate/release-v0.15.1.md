# Release v0.15.1

Run 2026-10-01, 09:30–09:53 UTC, per PLAN.md "Release: tag v0.15.1".

**Verdict: RELEASED.** The signed tag `v0.15.1` is on `70947a1`. Release
Stable and Verify Release both succeeded, and every published artifact checks
out.

| Step | Result |
|---|---|
| 1. Tag and push | **Done.** Tag object `b18b8e7`, which points at `70947a1`; signature verified; pushed 09:31:02 |
| 2. Release run | **Release Stable: success**, 2 jobs, 20 min 5 s. **Verify Release: success** (dispatched by hand, as the plan says), 27 s |
| 3. Published artifacts | **All pass:** GitHub release, assets, chart, 9/9 image signatures, binary signature and checksums, standalone manifests |

The clusters were **not** upgraded to the stable chart, as the plan says.
They run `0.0.0-dev.5968241`, which has the same code.

## 1. Tag and push

```text
git fetch origin
origin/main = 70947a12aab3c586aa63454f967afd77120e8cb9   (exactly; not moved)
5968241 (validated) is an ancestor; 5968241..70947a1 = #701 release: v0.15.1, #702 CHANGELOG date
  git diff 5968241 70947a1 -- '*.go' rust/ charts/kubeswift/templates/ charts/kubeswift/crds/  -> empty (15 files changed, all docs/pins)
at 70947a1: Chart.yaml version 0.15.1 / appVersion "0.15.1"; CHANGELOG "## [v0.15.1] — 2026-10-01";
            config/dra-driver/dra-driver.yaml and config/daemonset/gpu-discovery.yaml pin :v0.15.1
v0.15.1 before tagging: absent locally and on origin (checked again right before the push)

git tag -s v0.15.1 -m "KubeSwift v0.15.1" 70947a12aab3c586aa63454f967afd77120e8cb9
git -c gpg.ssh.allowedSignersFile=<temporary file with William's key> tag -v v0.15.1
  Good "git" signature for william.rizzo@gmail.com with ED25519 key SHA256:ALS0VrMI2tXe5rUJX6PSHkkl432pIt1shD8+axLi/Zs
  object 70947a12aab3c586aa63454f967afd77120e8cb9  type commit  tag v0.15.1  tagger wrkode
git push origin v0.15.1   (09:31:02)
  * [new tag] v0.15.1 -> v0.15.1
git ls-remote: b18b8e7ff98d8bac574e66e3d069910065b0fa9d refs/tags/v0.15.1 ; 70947a12… refs/tags/v0.15.1^{}
```

The tag is signed with William's SSH key, as for v0.15.0. No git config was
changed.

## 2. The release run

**Release Stable:** https://github.com/kubeswift-io/kubeswift/actions/runs/36843251637
(started by the tag push)

| Job | Result | Time |
|---|---|---|
| Build and push images + chart | success | 09:31:09 → 09:46:59 (15 m 50 s) |
| Create GitHub Release | success | 09:47:03 → 09:51:10 (4 m 7 s) |
| **Run total** | **success** | 09:31:06 → 09:51:11 (**20 m 5 s**) |

**Verify Release:** https://github.com/kubeswift-io/kubeswift/actions/runs/36845425565.
It was dispatched by hand once Release Stable had finished:
`gh workflow run verify-release.yaml --ref main -f tag=v0.15.1`.

| Job | Result | Time |
|---|---|---|
| Verify v0.15.1 (Install cosign; Verify signatures + attestations) | success: "all 9 images verified for v0.15.1" | 09:51:36 → 09:51:58 |
| **Run total** | **success** | 09:51:32 → 09:51:59 (**27 s**) |

## 3. What was published

| What | Result |
|---|---|
| **GitHub release** | https://github.com/kubeswift-io/kubeswift/releases/tag/v0.15.1: "KubeSwift v0.15.1", **not a draft, not a pre-release**, published 09:51:06. The body **starts with** `## [v0.15.1] — 2026-10-01` and contains the whole CHANGELOG section verbatim (112 lines: intro, Upgrade, Fixed, Dependencies, CI, Docs). Then come **Install**, **Upgrade**, **Images**, **swiftctl** and **Supply chain**, then GitHub's "What's Changed" |
| **Release assets** | `swiftctl-darwin-amd64`, `swiftctl-darwin-arm64`, `swiftctl-linux-amd64`, `SHA256SUMS`, `SHA256SUMS.sig`, `SHA256SUMS.pem`: the same names as v0.15.0 |
| **Chart** | `helm show chart oci://ghcr.io/kubeswift-io/charts/kubeswift --version 0.15.1`: `name: kubeswift`, `version: 0.15.1`, `appVersion: 0.15.1` (digest `sha256:41992cd7…`) |
| **Images** | The plan's `cosign verify` loop (identity `release-(stable\|rc).yaml@refs/tags/v.*`): **OK × 9**, for `controller-manager`, `swiftletd`, `gpu-discovery`, `migration-stunnel`, `kubeswift-gateway`, `kubeswift-dra-driver`, `sandbox-materialize`, `snapshot-s3` and `snapshot-oras` |
| **Binaries** | `cosign verify-blob --signature SHA256SUMS.sig --certificate SHA256SUMS.pem`, with the identity pinned to `release-stable.yaml@refs/tags/v0.15.1`: **Verified OK**. Then `sha256sum -c SHA256SUMS`: **OK** for all three. `swiftctl-linux-amd64 --help` runs |
| **Standalone manifests** | At the tag, `config/dra-driver/dra-driver.yaml` pins `kubeswift-dra-driver:v0.15.1`, and `config/daemonset/gpu-discovery.yaml` pins `gpu-discovery:v0.15.1`. **Both image tags exist** (`oras manifest fetch`) |

```text
SHA256SUMS
42fab20b6d3ec8b2f2bebf9748c4611aa0cbf6d832ddfeb0de0a1520daf14c5f  swiftctl-darwin-amd64
da98473a9ed149402eb9229cc7600c112f2905d4040637f87842b69091e18095  swiftctl-darwin-arm64
2f642af848dcb00d599b672beafaaefc8cae03a4a4dd2b3291d4d51e2a26a8dc  swiftctl-linux-amd64
```
