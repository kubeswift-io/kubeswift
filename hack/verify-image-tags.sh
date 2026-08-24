#!/usr/bin/env bash
# verify-image-tags — every image reference the chart renders must carry a tag
# CI could actually have published.
#
# Why this exists: Chart.yaml's appVersion is hand-edited at release time, and
# `kubeswift.imageTag` used to prepend "v" unconditionally. When appVersion was
# written as "v0.13.11" the helper produced the tag `vv0.13.11` — an image that
# does not exist, for the controller AND for the five launcher images it hands
# on via env var. Nothing failed at render, lint, or install time: kubeconform
# validates schema, not tag plausibility, and the pull only fails later, on a
# node. Released charts were unaffected because the workflows pass
# `--app-version "${TAG#v}"`, so the bug lived exclusively on the
# install-from-a-checkout path and stayed invisible for several releases.
#
# The helper now trims the prefix, so that exact bug is unspellable. This checks
# the property instead of that one spelling: a rendered tag must look like
# something the release pipeline emits.
#
# Accepted tag shapes:
#   vX.Y.Z[-suffix]   stable / rc   (release-stable, release-rc)
#   sha-<hex>         dev builds    (release-dev)
#   latest            local builds  (values default, pre-resolution)
#
# Rendered through hack/render-lint-profile.sh, NOT an inline `helm template`
# line, for the reason that script's header gives: a second caller drifts, and
# a check that renders fewer objects than it thinks still reports green. That
# profile also supplies the OIDC placeholders the gateway fail-closes without.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"

RENDER=$("$ROOT/hack/render-lint-profile.sh")

# Both spellings matter: `image:` on containers, and `value:` on the env vars
# the controller uses to tell launcher pods which image to run. The latter is
# where five of the six bad tags lived.
mapfile -t REFS < <(
  printf '%s\n' "$RENDER" \
    | grep -oE '(image|value):[[:space:]]*"?[A-Za-z0-9.-]+/[A-Za-z0-9./_-]+:[A-Za-z0-9._-]+"?' \
    | sed -E 's/^(image|value):[[:space:]]*//; s/"//g' \
    | sort -u
)

# A vacuous pass is the failure mode this whole file exists to prevent, so an
# empty extraction is an error, not a success.
if [[ ${#REFS[@]} -eq 0 ]]; then
  echo "verify-image-tags: found no image references to check." >&2
  echo "  The extraction regex has drifted from the templates, or the render" >&2
  echo "  is empty. Failing rather than passing vacuously." >&2
  exit 1
fi

bad=0
for ref in "${REFS[@]}"; do
  tag="${ref##*:}"
  if [[ "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([-.+][A-Za-z0-9._-]+)?$ ]] \
     || [[ "$tag" =~ ^sha-[0-9a-f]{7,40}$ ]] \
     || [[ "$tag" == "latest" ]]; then
    continue
  fi
  echo "verify-image-tags: BAD TAG  $ref" >&2
  echo "    tag '$tag' is not vX.Y.Z, sha-<hex>, or latest" >&2
  bad=1
done

if [[ $bad -ne 0 ]]; then
  echo >&2
  echo "  Most likely cause: charts/kubeswift/Chart.yaml appVersion is not a" >&2
  echo "  bare X.Y.Z. It must NOT carry a leading 'v' — the imageTag helper" >&2
  echo "  adds it." >&2
  exit 1
fi

echo "verify-image-tags: ${#REFS[@]} image references, all well-formed"
