#!/usr/bin/env bash
# Drive the signature-interop harness in internal/oci/interop_test.go (#486).
#
# Provisions everything that test needs and then runs it:
#   - the cosign binaries under comparison, checksum-verified against upstream
#   - a TLS registry (cosign verify will not speak plain HTTP, so a plaintext
#     registry cannot test the verify half at all)
#   - a signing keypair and a small artifact to sign
#
# Everything is torn down on exit. Nothing here touches a real registry, a real
# cluster, or the operator's cosign install.
#
# The artifact it signs is a synthetic throwaway built fresh each run, and that
# is deliberate rather than incidental: if the offline dialect is ever broken,
# cosign uploads the signed digest to the PUBLIC Rekor — which is exactly the
# failure this harness detects, and the reason it must never be pointed at a
# real private artifact. Do not add a flag to sign an existing image with it.
#
# Usage:
#   hack/cosign-interop.sh                            # baseline — must be green
#   COSIGN_VERSIONS="v2.6.5 v3.1.3" hack/cosign-interop.sh   # evaluate a candidate
#
# Exit status is the test's: non-zero means a signature did not verify.
#
# Both majors are in the default matrix and both must pass. That was not always
# true: cosign 3.x rejects `--tlog-upload=false`, so it could not sign under our
# contract at all until SignArgs learned to pass a no-transparency-log
# `--signing-config` instead. The trap it avoids is worth stating, because the
# obvious shortcut is worse than the blocker — simply DROPPING the flag on 3.x
# signs happily and uploads the artifact digest to the PUBLIC Rekor. The harness
# checks for that directly (assertNoTransparencyLogEntry), so the shortcut fails
# a test instead of silently disclosing private artifact digests.

set -euo pipefail

# The versions to cross-check. v2.6.5 is what every existing signature in users'
# registries was made with, so it is always in the set: it is the baseline every
# other implementation is judged against, and it generates the keypair.
BASELINE="v2.6.5"
COSIGN_VERSIONS="${COSIGN_VERSIONS:-$BASELINE v3.1.3}"
case " $COSIGN_VERSIONS " in
  *" $BASELINE "*) ;;
  *) COSIGN_VERSIONS="$BASELINE $COSIGN_VERSIONS" ;;
esac
REGISTRY_PORT="${REGISTRY_PORT:-5443}"
WORKDIR="${WORKDIR:-$(mktemp -d -t cosign-interop-XXXXXX)}"
BIN_CACHE="${BIN_CACHE:-${TMPDIR:-/tmp}/kubeswift-cosign-bins}"
REGISTRY_NAME="kubeswift-interop-registry"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

log()  { printf '  %s\n' "$*"; }
step() { printf '\n== %s\n' "$*"; }

cleanup() {
  docker rm -f "$REGISTRY_NAME" >/dev/null 2>&1 || true
  [ -n "${KEEP_WORKDIR:-}" ] || rm -rf "$WORKDIR"
}
trap cleanup EXIT

for tool in docker openssl curl go jq; do
  command -v "$tool" >/dev/null || { echo "missing required tool: $tool" >&2; exit 1; }
done

step "cosign binaries"
mkdir -p "$BIN_CACHE"
IMPLS=""
for v in $COSIGN_VERSIONS; do
  bin="$BIN_CACHE/cosign-$v"
  if [ ! -x "$bin" ]; then
    log "downloading cosign $v"
    curl -sSfL "https://github.com/sigstore/cosign/releases/download/${v}/cosign-linux-amd64" -o "$bin"
    chmod +x "$bin"
  fi
  # These binaries sign artifacts, so verify them rather than trusting the
  # download. A tampered signer would make the whole matrix meaningless.
  sums="$BIN_CACHE/sums-$v.txt"
  [ -f "$sums" ] || curl -sSfL "https://github.com/sigstore/cosign/releases/download/${v}/cosign_checksums.txt" -o "$sums"
  want=$(awk '/ cosign-linux-amd64$/ {print $1}' "$sums")
  got=$(sha256sum "$bin" | awk '{print $1}')
  [ "$want" = "$got" ] || { echo "checksum mismatch for cosign $v" >&2; exit 1; }
  log "$v  checksum ok  ($($bin version 2>/dev/null | awk '/GitVersion/{print $2}'))"
  IMPLS="${IMPLS:+$IMPLS,}$v=$bin"
done

step "TLS registry on 127.0.0.1:$REGISTRY_PORT"
certs="$WORKDIR/certs"; mkdir -p "$certs"
openssl req -x509 -newkey rsa:2048 -nodes -days 1 \
  -subj "/CN=127.0.0.1" -addext "subjectAltName=IP:127.0.0.1,DNS:localhost" \
  -keyout "$certs/registry.key" -out "$certs/registry.crt" >/dev/null 2>&1
docker rm -f "$REGISTRY_NAME" >/dev/null 2>&1 || true
docker run -d --name "$REGISTRY_NAME" -p "$REGISTRY_PORT:5000" \
  -v "$certs:/certs:ro" \
  -e REGISTRY_HTTP_ADDR=0.0.0.0:5000 \
  -e REGISTRY_HTTP_TLS_CERTIFICATE=/certs/registry.crt \
  -e REGISTRY_HTTP_TLS_KEY=/certs/registry.key \
  registry:2 >/dev/null

REG="127.0.0.1:$REGISTRY_PORT"
CURL=(curl -sS --cacert "$certs/registry.crt")
for _ in $(seq 30); do
  [ "$("${CURL[@]}" -o /dev/null -w '%{http_code}' "https://$REG/v2/" 2>/dev/null)" = "200" ] && break
  sleep 1
done
[ "$("${CURL[@]}" -o /dev/null -w '%{http_code}' "https://$REG/v2/")" = "200" ] || {
  echo "registry did not come up" >&2; docker logs "$REGISTRY_NAME" 2>&1 | tail -20 >&2; exit 1; }
log "up (self-signed CA, trusted via SSL_CERT_FILE)"

step "test artifact"
# Pushed with the registry API directly rather than `docker push`: that would
# need the CA installed into the docker daemon's trust store, which needs root
# and a daemon restart. The artifact content is irrelevant — cosign signs a
# digest — so a minimal OCI image is enough.
push_blob() {
  local repo="$1" data="$2" dgst
  dgst="sha256:$(printf '%s' "$data" | sha256sum | awk '{print $1}')"
  local loc
  loc=$("${CURL[@]}" -X POST -D- -o /dev/null "https://$REG/v2/$repo/blobs/uploads/" \
        | awk 'tolower($1)=="location:"{print $2}' | tr -d '\r')
  case "$loc" in /*) loc="https://$REG$loc" ;; esac
  local sep='?'; case "$loc" in *\?*) sep='&' ;; esac
  printf '%s' "$data" | "${CURL[@]}" -X PUT -H 'Content-Type: application/octet-stream' \
    --data-binary @- -o /dev/null "${loc}${sep}digest=$dgst"
  printf '%s' "$dgst"
}

CFG_DATA='{}'
LAYER_DATA='kubeswift signature interop harness'

# The subject must exist in EVERY repository a signer will sign into, not just
# one. Each signer gets its own repo so their signatures cannot be confused, and
# cosign 3.x HEADs the subject manifest before signing — it refuses to sign a
# digest the repository does not hold. cosign 2.x does not check, so an earlier
# version of this script signed into empty repos and only looked correct.
push_subject() {
  local repo="$1" cfg layer manifest
  cfg=$(push_blob "$repo" "$CFG_DATA")
  layer=$(push_blob "$repo" "$LAYER_DATA")
  manifest=$(jq -nc \
    --arg cd "$cfg" --argjson cs "${#CFG_DATA}" \
    --arg ld "$layer" --argjson ls "${#LAYER_DATA}" '{
      schemaVersion: 2,
      mediaType: "application/vnd.oci.image.manifest.v1+json",
      config: {mediaType: "application/vnd.oci.image.config.v1+json", digest: $cd, size: $cs},
      layers: [{mediaType: "application/vnd.oci.image.layer.v1.tar", digest: $ld, size: $ls}]
    }')
  printf '%s' "$manifest" | "${CURL[@]}" -X PUT \
    -H 'Content-Type: application/vnd.oci.image.manifest.v1+json' \
    --data-binary @- -o /dev/null "https://$REG/v2/$repo/manifests/v1"
  printf 'sha256:%s' "$(printf '%s' "$manifest" | sha256sum | awk '{print $1}')"
}

DIGEST=$(push_subject "interop/subject")
# Mirror it into each signer's repo. Must match the naming in interop_test.go.
for v in $COSIGN_VERSIONS; do
  push_subject "interop-$(printf '%s' "$v" | tr '[:upper:]' '[:lower:]')/test" >/dev/null
done
log "$REG/interop/subject@$DIGEST (mirrored into each signer's repo)"

step "signing key"
# Generated by the baseline on purpose: the key format is part of what is under
# test, and every existing signature was made with a key this version produced.
export COSIGN_PASSWORD=""
( cd "$WORKDIR" && "$BIN_CACHE/cosign-$BASELINE" generate-key-pair >/dev/null )
log "$WORKDIR/cosign.key (+ cosign.pub), generated by $BASELINE"

# A second, unrelated keypair for the test's negative control — verification
# against it must fail, which is what proves the matrix means anything.
mkdir -p "$WORKDIR/wrong"
( cd "$WORKDIR/wrong" && "$BIN_CACHE/cosign-$BASELINE" generate-key-pair >/dev/null )
log "$WORKDIR/wrong/cosign.pub (negative control)"

step "interop matrix"
# SSL_CERT_FILE is how cosign is told to trust the throwaway CA. It is set here
# rather than passed as a flag on purpose: the test must run the argv that
# SignArgs/VerifyArgs produce in production, unmodified.
export SSL_CERT_FILE="$certs/registry.crt"
export KUBESWIFT_INTEROP_REGISTRY="$REG"
export KUBESWIFT_INTEROP_DIGEST="$DIGEST"
export KUBESWIFT_INTEROP_KEY="$WORKDIR/cosign.key"
export KUBESWIFT_INTEROP_PUBKEY="$WORKDIR/cosign.pub"
export KUBESWIFT_INTEROP_WRONG_PUBKEY="$WORKDIR/wrong/cosign.pub"
export KUBESWIFT_INTEROP_COSIGNS="$IMPLS"

cd "$REPO_ROOT"
go test -tags=interop -count=1 -v -timeout 15m ./internal/oci/ -run TestSignatureInterop
