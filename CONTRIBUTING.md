# Contributing to KubeSwift

Thanks for your interest in KubeSwift. This document covers how to get a change
merged: how to build and test, the conventions the project follows, and the few
repo-specific traps that are easy to hit.

KubeSwift is pre-1.0 and the `v1alpha1` APIs may change between releases.

## Code of Conduct

Be respectful and constructive. Harassment, personal attacks, and demeaning
comments are not welcome in issues, pull requests, or any other project space.
Maintainers may remove comments, reject contributions, or block accounts that
do not meet that bar.

## Reporting bugs

Open a [GitHub issue](https://github.com/kubeswift-io/kubeswift/issues). The
most useful reports include:

- what you expected and what happened instead,
- the KubeSwift version (`kubectl get deploy -n kubeswift-system controller-manager -o jsonpath='{.spec.template.spec.containers[0].image}'`),
- Kubernetes version, CNI, and storage backend,
- **real output** — `kubectl describe` on the affected resource, controller
  logs, and launcher-pod logs. This project's convention is to diagnose from
  actual cluster output rather than from a description of it, so a log excerpt
  is worth more than a paragraph.

For anything with security impact, do not open a public issue — see
[Security](#security) below.

## Proposing changes

For a small fix, open a pull request directly. For anything that adds a CRD
field, changes an API, or alters runtime behaviour, open an issue first so the
design can be discussed before you write it — API surface is hard to remove
once released.

## Development setup

You will need:

- **Go** — see the `go` directive in `go.mod` for the minimum version
- **Rust** (stable) for the `rust/` workspace
- **Docker** or another OCI builder, for container images
- **A Kubernetes cluster** for anything beyond unit tests. KubeSwift launches
  real VMs, so meaningful testing needs a cluster whose nodes have `/dev/kvm`.

```bash
make build          # Go binaries + Rust crates
go test ./...       # Go unit tests
cargo test          # Rust tests (from rust/)
make help           # every available target
```

`make help` is the source of truth for targets; the e2e suites
(`make e2e-tests`, `make smoke-test`) require a live cluster.

## The one trap to know about

**After any change under `api/`, run `make generate`.**

```bash
make generate
```

This regenerates the CRDs and deepcopy functions and syncs `charts/kubeswift/crds/`.
If you skip it, the shipped CRD schema no longer matches the Go types, and the
API server **silently drops** any field it does not know about — no error, the
field simply disappears. CI enforces this, but it is much easier to catch
locally.

Related: `helm upgrade` does not upgrade CRDs (Helm treats `crds/` as
install-only), which is why release notes always tell operators to apply them
separately.

## Commit conventions

**Sign off every commit** with the Developer Certificate of Origin:

```bash
git commit -s
```

This adds a `Signed-off-by:` line certifying, under the
[Developer Certificate of Origin](https://developercertificate.org/), that you
have the right to submit the work under the project's licence (Apache-2.0). Commits must also be cryptographically
signed — the repository ruleset requires it.

Write commit messages that explain **why**, not just what. The diff already
shows what changed; the message should say what problem it solves and, where
relevant, what you ruled out. A good message is the difference between a future
reader trusting the change and re-deriving it from scratch.

## Pull requests

1. **Branch off `main`.** Do not commit to `main` directly.
2. Keep the change focused. Unrelated fixes belong in their own PR.
3. **Add tests.** For a bug fix, a test that fails without the fix. For a new
   guard or check, verify it can actually fail — a check that has never gone
   red is not known to work.
4. Update documentation in the same PR. If you add a CRD field, it belongs in
   `docs/crds.md` and the relevant `docs/api/` page, not only in a guide.
5. Make sure CI is green. The pipeline runs Go and Rust builds and tests,
   `gofmt`/`cargo fmt`, CRD generation drift, proto generation drift, container
   image builds, CodeQL, gosec, and vulnerability scanning.
6. PRs are merged by **squash merge**, so the PR title and description become
   the commit message on `main`. Write them accordingly.

Maintainers may ask for changes; that is normal and not a judgement of the
work. [MAINTAINERS.md](MAINTAINERS.md) lists who reviews and merges, and how
decisions are made.

## Code conventions

**Go** — standard `gofmt`. Controllers follow controller-runtime patterns
(reconcile loop, status conditions, status subresource). New CRD fields need
`+kubebuilder:` markers, and new API groups need scheme registration.

**Rust** — `cargo fmt`. The workspace lives at `rust/Cargo.toml`; new crates are
added as workspace members. Async work uses tokio; errors use `anyhow` in
binaries and `thiserror` in libraries.

**Both** — match the surrounding code. Comment density, naming, and structure
should look like the file you are editing rather than like a different project.

## Documentation

Documentation lives in `docs/`, indexed by `docs/index.md`. Two conventions:

- Install commands must pin the current chart version; CI fails otherwise
  (`hack/verify-doc-versions.sh`).
- Prose recording when a feature appeared — "a v0.13.11 field" — is history and
  should **not** be updated to the current version. It is correct as written.

Prefer plain, direct prose. Explain the mechanism rather than restating the
field name.

## Security

Do not report vulnerabilities through public issues. Use GitHub's
[private vulnerability reporting](https://github.com/kubeswift-io/kubeswift/security/advisories/new)
so the issue can be fixed before it is disclosed.

Note that the VM **launcher** pod runs privileged by design — it manages KVM,
tap devices, and VFIO — and is therefore a node-level trust boundary. See
[`docs/security-audit.md`](docs/security-audit.md) before filing a report about
it, as this is deliberate rather than an oversight.

## Licence

KubeSwift is licensed under the [Apache License, Version 2.0](LICENSE). By
contributing, you agree that your contributions are licensed under the same
terms (section 5 of the licence: a contribution intentionally submitted for
inclusion is under Apache-2.0 unless you state otherwise). The `Signed-off-by`
line certifies that you have the right to submit it on those terms.

Do not add code copied from another project unless its licence is compatible
with Apache-2.0 (for example MIT, BSD or Apache-2.0), and keep its copyright
and licence notices. Code under a copyleft licence such as GPL or AGPL cannot
be included.
