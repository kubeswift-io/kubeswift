#!/usr/bin/env bash
#
# SwiftSandbox output export (spec.outputs): round trip, security cases and
# timings, on a real cluster.
#
# Why this needs a real cluster with KVM and a registry: the output is written
# by a guest over virtio-fs, collected by swiftletd after the microVM powers
# off, and pushed by an export Job with a platform credential. Unit tests
# cover each piece; this proves they meet.
#
# Subcommands:
#   ./outputs.sh validate   # round trip + security cases + compiler fixture
#   ./outputs.sh perf       # stage timings at 256 KiB, 10 MiB and 100 MiB
#   ./outputs.sh cleanup    # delete the namespaces, policies and Secrets
#
# What "validate" checks:
#   round trip  A exports; a SwiftArtifactPlacement prepares the digest on a
#               pool's node; pooled B mounts it read-only and verifies every
#               file's sha256, and that it cannot write.
#    1 normal output exported                    2 empty output exported
#    3 exactly maxSize exported                  4 maxSize + 1 byte: OutputTooLarge
#    5 relative symlink (traversal): OutputInvalidEntry
#    6 absolute symlink escape: OutputInvalidEntry (a hard link is exported
#      as a copy: it cannot leave the output's own filesystem)
#    7 FIFO / device node: OutputInvalidEntry
#    8 repository outside the policy: refused, no launcher
#    9 refused credential: RegistryUnauthorized
#   10 unresolvable registry: RegistryUnavailable
#   11 registry that never answers: UploadTimeout
#   12 sandbox deleted while uploading: export Job, spool and tag gone
#   13 controller restarted while uploading: exported once (RESTART_CONTROLLER=1)
#   14 the guest, with open egress, finds no push credential and cannot read
#      the Secret; the launcher pod does not reference it
#   15 another namespace: refused (not selected / not its path)
#   16 under a SwiftArtifactVerificationPolicy: unsigned refused, signed by a
#      separate signer consumed (needs cosign)
#   fixture     a sandbox compiles a C program into its output; another runs
#               the exported binary (needs egress to the Alpine mirrors)
#
# Prerequisites:
#   - KubeSwift with spec.outputs, a kernel node with KVM
#   - KERNEL_IMAGE: a sandbox kernel with bridge feature "outputs"
#     (kernels/sandbox:6.6.17 or later)
#   - REPOSITORY_PREFIX: an HTTPS registry path the test may push under,
#     e.g. ghcr.io/example/outputs-test
#   - PUSH_DOCKERCONFIG: a docker config.json with a push credential for it
#   - PULL_DOCKERCONFIG: a docker config.json to pull it back (default: the
#     push one)
#
# Acceptance: exit 0 on success. Non-zero on any check failing.

set -euo pipefail

NS=${NS:-outputs-test}
OTHER_NS=${OTHER_NS:-outputs-test-other}
BAD_NS=${BAD_NS:-outputs-test-badcred}
NET_NS=${NET_NS:-outputs-test-net}
SYSTEM_NS=${SYSTEM_NS:-kubeswift-system}
IMAGE=${IMAGE:-docker.io/library/alpine:3.20}
KERNEL_IMAGE=${KERNEL_IMAGE:-}
REPOSITORY_PREFIX=${REPOSITORY_PREFIX:-}
PUSH_DOCKERCONFIG=${PUSH_DOCKERCONFIG:-}
PULL_DOCKERCONFIG=${PULL_DOCKERCONFIG:-$PUSH_DOCKERCONFIG}
RESTART_CONTROLLER=${RESTART_CONTROLLER:-0}
TIMEOUT=${TIMEOUT:-300}
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

PASS=0
FAIL=0
pass() { echo "PASS: $*"; PASS=$((PASS + 1)); }
fail() { echo "FAIL: $*"; FAIL=$((FAIL + 1)); }

need() {
  local missing=0
  for v in KERNEL_IMAGE REPOSITORY_PREFIX PUSH_DOCKERCONFIG; do
    if [ -z "${!v}" ]; then echo "set $v" >&2; missing=1; fi
  done
  [ $missing -eq 0 ] || exit 2
}

# sandbox NAME NAMESPACE SCRIPT [jq-style extras as JSON object]
# Creates a SwiftSandbox running SCRIPT with one output "out" at /output.
sandbox() {
  local name=$1 ns=$2 script=$3 extra=${4:-'{}'}
  python3 - "$name" "$ns" "$script" "$extra" "$IMAGE" <<'PY' | kubectl apply -f - >/dev/null
import json, sys
name, ns, script, extra, image = sys.argv[1:6]
extra = json.loads(extra)
out = {"name": "out", "path": "/output"}
for k in ("repository", "maxSize"):
    if k in extra:
        out[k] = extra.pop(k)
spec = {"image": image, "cpu": 1, "memory": "512Mi",
        "command": ["/bin/sh", "-c", script], "outputs": [out]}
spec.update(extra)
print(json.dumps({"apiVersion": "sandbox.kubeswift.io/v1alpha1", "kind": "SwiftSandbox",
                  "metadata": {"name": name, "namespace": ns}, "spec": spec}))
PY
}

# consumer NAME REF [extras JSON]: mounts REF at /in, verifies, cannot write.
consumer() {
  local name=$1 ref=$2 extra=${3:-'{}'}
  python3 - "$name" "$NS" "$ref" "$extra" "$IMAGE" <<'PY' | kubectl apply -f - >/dev/null
import json, sys
name, ns, ref, extra, image = sys.argv[1:6]
extra = json.loads(extra)
art = {"name": "out", "ref": ref, "mountPath": "/in", "layout": "unpacked",
       "pullSecretRef": {"name": "outputs-pull"}}
if extra.pop("requirePlatformVerification", False):
    art["requirePlatformVerification"] = True
check = extra.pop("check", "cd /in || exit 10; sha256sum -c SHA256SUMS >/dev/null 2>&1 || exit 12; "
                           "touch /in/x 2>/dev/null && exit 13; exit 0")
spec = {"image": image, "cpu": 1, "memory": "512Mi", "command": ["/bin/sh", "-c", check], "artifacts": [art]}
spec.update(extra)
print(json.dumps({"apiVersion": "sandbox.kubeswift.io/v1alpha1", "kind": "SwiftSandbox",
                  "metadata": {"name": name, "namespace": ns}, "spec": spec}))
PY
}

wait_done() { # NAME NS [timeout]
  local end=$((SECONDS + ${3:-$TIMEOUT})) p
  while [ $SECONDS -lt $end ]; do
    p=$(kubectl -n "$2" get swiftsandbox "$1" -o jsonpath='{.status.phase}' 2>/dev/null || true)
    case $p in Completed | Failed) return 0 ;; esac
    sleep 2
  done
  return 1
}

field() { kubectl -n "$2" get swiftsandbox "$1" -o jsonpath="$3"; }

# expect NAME NS PHASE OUTPUT_STATE [OUTPUT_REASON]
expect() {
  local name=$1 ns=$2 phase=$3 state=$4 reason=${5:-}
  if ! wait_done "$name" "$ns"; then fail "$name: not terminal after ${TIMEOUT}s"; return; fi
  local p s r
  p=$(field "$name" "$ns" '{.status.phase}')
  s=$(field "$name" "$ns" '{.status.outputs[0].state}')
  r=$(field "$name" "$ns" '{.status.outputs[0].reason}')
  if [ "$p" = "$phase" ] && [ "$s" = "$state" ] && [ "$r" = "$reason" ]; then
    pass "$name: $p, output $s${reason:+ ($reason)}"
  else
    fail "$name: phase $p, output $s ($r: $(field "$name" "$ns" '{.status.outputs[0].message}')); want $phase, $state${reason:+ ($reason)}"
  fi
}

no_launcher() {
  if kubectl -n "$2" get pod "$1" >/dev/null 2>&1; then fail "$1: a launcher pod was created"; else pass "$1: no launcher created"; fi
}

PRODUCER='set -e; cd /output; mkdir -p bin obj/a obj/b; printf "#!/bin/sh\necho tool\n" > bin/tool; chmod 755 bin/tool
for i in 1 2 3 4; do echo "object $i" > obj/a/m$i.o; done; head -c ${SIZE:-262144} /dev/urandom > obj/b/blob.bin
find . -type f ! -name SHA256SUMS | sort | xargs sha256sum > SHA256SUMS'

setup() {
  need
  local kernel
  for ns in "$NS" "$OTHER_NS" "$BAD_NS" "$NET_NS"; do
    kubectl create namespace "$ns" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
    kernel=$(printf '{"apiVersion":"kernel.kubeswift.io/v1alpha1","kind":"SwiftKernel","metadata":{"name":"sandbox","namespace":"%s"},"spec":{"kernelCmdline":"console=ttyS0","profile":"sandbox","ociRef":{"image":"%s"}}}' "$ns" "$KERNEL_IMAGE")
    echo "$kernel" | kubectl apply -f - >/dev/null
  done
  kubectl label namespace "$NS" outputs-test.kubeswift.io/exports=true --overwrite >/dev/null
  kubectl label namespace "$BAD_NS" outputs-test.kubeswift.io/badcred=true --overwrite >/dev/null
  kubectl label namespace "$NET_NS" outputs-test.kubeswift.io/net=true --overwrite >/dev/null

  kubectl -n "$SYSTEM_NS" create secret docker-registry outputs-test-push \
    --from-file=.dockerconfigjson="$PUSH_DOCKERCONFIG" --dry-run=client -o yaml |
    kubectl label --local -f - kubeswift.io/export-credentials=true -o yaml | kubectl apply -f - >/dev/null
  printf '{"auths":{"%s":{"auth":"%s"}}}' "${REPOSITORY_PREFIX%%/*}" "$(printf 'invalid:invalid' | base64 -w0)" > "$WORK/bad.json"
  kubectl -n "$SYSTEM_NS" create secret docker-registry outputs-test-bad \
    --from-file=.dockerconfigjson="$WORK/bad.json" --dry-run=client -o yaml |
    kubectl label --local -f - kubeswift.io/export-credentials=true -o yaml | kubectl apply -f - >/dev/null
  kubectl -n "$NS" create secret docker-registry outputs-pull \
    --from-file=.dockerconfigjson="$PULL_DOCKERCONFIG" --dry-run=client -o yaml | kubectl apply -f - >/dev/null

  kubectl apply -f - >/dev/null <<EOF
apiVersion: sandbox.kubeswift.io/v1alpha1
kind: SwiftArtifactExportPolicy
metadata: {name: outputs-test}
spec:
  namespaceSelector: {matchLabels: {outputs-test.kubeswift.io/exports: "true"}}
  repositories: ["$REPOSITORY_PREFIX/{namespace}/**"]
  defaultRepository: "$REPOSITORY_PREFIX/{namespace}"
  credentialsSecretRef: {name: outputs-test-push}
  maxSize: 200Mi
---
apiVersion: sandbox.kubeswift.io/v1alpha1
kind: SwiftArtifactExportPolicy
metadata: {name: outputs-test-badcred}
spec:
  namespaceSelector: {matchLabels: {outputs-test.kubeswift.io/badcred: "true"}}
  repositories: ["$REPOSITORY_PREFIX/{namespace}/**"]
  defaultRepository: "$REPOSITORY_PREFIX/{namespace}"
  credentialsSecretRef: {name: outputs-test-bad}
---
apiVersion: sandbox.kubeswift.io/v1alpha1
kind: SwiftArtifactExportPolicy
metadata: {name: outputs-test-unreachable}
spec:
  namespaceSelector: {matchLabels: {outputs-test.kubeswift.io/net: "true"}}
  repositories: ["unreachable.invalid/{namespace}/**"]
  credentialsSecretRef: {name: outputs-test-bad}
---
apiVersion: sandbox.kubeswift.io/v1alpha1
kind: SwiftArtifactExportPolicy
metadata: {name: outputs-test-tarpit}
spec:
  namespaceSelector: {matchLabels: {outputs-test.kubeswift.io/net: "true"}}
  repositories: ["tarpit.$NET_NS.svc.cluster.local/{namespace}/**"]
  credentialsSecretRef: {name: outputs-test-bad}
  uploadTimeout: 45s
---
apiVersion: v1
kind: Pod
metadata: {name: tarpit, namespace: $NET_NS, labels: {app: tarpit}}
spec:
  containers:
  - {name: nc, image: $IMAGE, command: ["sh", "-c", "while true; do nc -lk -p 443 >/dev/null; done"]}
---
apiVersion: v1
kind: Service
metadata: {name: tarpit, namespace: $NET_NS}
spec: {selector: {app: tarpit}, ports: [{port: 443, targetPort: 443}]}
EOF
  for ns in "$NS" "$OTHER_NS" "$BAD_NS" "$NET_NS"; do
    kubectl -n "$ns" wait --for=jsonpath='{.status.phase}'=Ready swiftkernel/sandbox --timeout=300s >/dev/null
  done
  kubectl -n "$NET_NS" wait --for=condition=Ready pod/tarpit --timeout=120s >/dev/null
}

round_trip() {
  sandbox rt-producer "$NS" "$PRODUCER"
  expect rt-producer "$NS" Completed Exported
  local ref
  ref=$(field rt-producer "$NS" '{.status.outputs[0].reference}')
  [ -n "$ref" ] || { fail "round trip: no reference"; return; }
  kubectl apply -f - >/dev/null <<EOF
apiVersion: sandbox.kubeswift.io/v1alpha1
kind: SwiftSandboxPool
metadata: {name: rt-pool, namespace: $NS}
spec: {image: $IMAGE, cpu: 1, memory: 512Mi, minWarm: 1}
---
apiVersion: sandbox.kubeswift.io/v1alpha1
kind: SwiftArtifactPlacement
metadata: {name: rt-prep, namespace: $NS}
spec:
  poolRef: {name: rt-pool}
  artifacts:
  - {name: out, ref: "$ref", layout: unpacked, pullSecretRef: {name: outputs-pull}}
EOF
  kubectl -n "$NS" wait --for=condition=Prepared sap/rt-prep --timeout=300s >/dev/null &&
    pass "round trip: placement prepared $ref" || fail "round trip: placement not prepared"
  local end=$((SECONDS + 300))
  until [ "$(kubectl -n "$NS" get swiftsandboxpool rt-pool -o jsonpath='{.status.warmReplicas}')" = "1" ] || [ $SECONDS -ge $end ]; do sleep 2; done
  consumer rt-consumer "$ref" '{"poolRef":{"name":"rt-pool"}}'
  if wait_done rt-consumer "$NS" && [ "$(field rt-consumer "$NS" '{.status.phase}/{.status.exitCode}')" = "Completed/0" ]; then
    pass "round trip: pooled consumer verified every file, read-only ($(field rt-consumer "$NS" '{.status.artifacts[0].reason}'))"
  else
    fail "round trip: consumer $(field rt-consumer "$NS" '{.status.phase} exit={.status.exitCode} {.status.message}')"
  fi
}

security() {
  sandbox c02-empty "$NS" 'true'
  sandbox c03-max "$NS" 'head -c 1048576 /dev/urandom > /output/f' '{"maxSize":"1Mi"}'
  sandbox c04-over "$NS" 'head -c 1048577 /dev/urandom > /output/f' '{"maxSize":"1Mi"}'
  sandbox c05-traversal "$NS" 'mkdir /output/d && ln -s ../../../../etc /output/d/up'
  sandbox c06-symlink "$NS" 'ln -s /etc/shadow /output/shadow'
  sandbox c06-hardlink "$NS" 'echo a > /output/a && ln /output/a /output/b'
  sandbox c07-special "$NS" 'mkfifo /output/fifo; mknod /output/null c 1 3 2>/dev/null; true'
  sandbox c08-outside "$NS" 'true' "{\"repository\":\"${REPOSITORY_PREFIX%%/*}/elsewhere/out\"}"
  sandbox c09-badcred "$BAD_NS" 'echo x > /output/f'
  sandbox c10-unreachable "$NET_NS" 'echo x > /output/f' "{\"repository\":\"unreachable.invalid/$NET_NS/out\"}"
  sandbox c11-timeout "$NET_NS" 'echo x > /output/f' "{\"repository\":\"tarpit.$NET_NS.svc.cluster.local/$NET_NS/out\"}"
  sandbox c15-unselected "$OTHER_NS" 'true'
  sandbox c15-intoother "$NS" 'true' "{\"repository\":\"$REPOSITORY_PREFIX/$OTHER_NS/out\"}"

  expect c02-empty "$NS" Completed Exported
  expect c03-max "$NS" Completed Exported
  expect c04-over "$NS" Failed Failed OutputTooLarge
  expect c05-traversal "$NS" Failed Failed OutputInvalidEntry
  expect c06-symlink "$NS" Failed Failed OutputInvalidEntry
  expect c06-hardlink "$NS" Completed Exported
  expect c07-special "$NS" Failed Failed OutputInvalidEntry
  expect c08-outside "$NS" Failed Failed OutputDestinationDenied
  no_launcher c08-outside "$NS"
  expect c09-badcred "$BAD_NS" Failed Failed RegistryUnauthorized
  TIMEOUT=600 expect c10-unreachable "$NET_NS" Failed Failed RegistryUnavailable
  expect c11-timeout "$NET_NS" Failed Failed UploadTimeout
  expect c15-unselected "$OTHER_NS" Failed Failed OutputDestinationDenied
  no_launcher c15-unselected "$OTHER_NS"
  expect c15-intoother "$NS" Failed Failed OutputDestinationDenied

  case12
  case13
  case14
  case16
}

wait_uploading() {
  local end=$((SECONDS + TIMEOUT))
  until [ "$(field "$1" "$NS" '{.status.outputs[0].state}')" = "Uploading" ] || [ $SECONDS -ge $end ]; do sleep 0.3; done
}

case12() {
  sandbox c12-delete "$NS" "SIZE=104857600; $PRODUCER"
  local uid node
  uid=$(field c12-delete "$NS" '{.metadata.uid}')
  wait_uploading c12-delete
  node=$(field c12-delete "$NS" '{.status.nodeName}')
  kubectl -n "$NS" delete swiftsandbox c12-delete --timeout=120s >/dev/null
  if [ -z "$(kubectl -n "$SYSTEM_NS" get jobs,pods -l "sandbox.kubeswift.io/export-sandbox-uid=$uid,app.kubernetes.io/component=sandbox-export" -o name)" ]; then
    pass "c12-delete: export Job gone with the sandbox (the cleanup Job removes itself after its TTL)"
  else
    fail "c12-delete: export Job left for $uid"
  fi
  kubectl -n "$NS" run c12-spool --restart=Never --image="$IMAGE" --overrides="{\"spec\":{\"nodeName\":\"$node\",\"containers\":[{\"name\":\"c\",\"image\":\"$IMAGE\",\"command\":[\"sh\",\"-c\",\"test ! -e /spool/$uid\"],\"volumeMounts\":[{\"name\":\"s\",\"mountPath\":\"/spool\",\"readOnly\":true}]}],\"volumes\":[{\"name\":\"s\",\"hostPath\":{\"path\":\"/var/lib/kubeswift/sandbox-outputs\"}}]}}" >/dev/null
  if kubectl -n "$NS" wait --for=jsonpath='{.status.phase}'=Succeeded pod/c12-spool --timeout=120s >/dev/null 2>&1; then
    pass "c12-delete: spool directory removed on the node"
  else
    fail "c12-delete: spool directory /var/lib/kubeswift/sandbox-outputs/$uid still on $node"
  fi
  kubectl -n "$NS" delete pod c12-spool --wait=false >/dev/null
}

case13() {
  if [ "$RESTART_CONTROLLER" != 1 ]; then echo "SKIP: c13 (set RESTART_CONTROLLER=1 to restart the controller)"; return; fi
  sandbox c13-restart "$NS" "SIZE=104857600; $PRODUCER"
  local uid jobs=""
  uid=$(field c13-restart "$NS" '{.metadata.uid}')
  wait_uploading c13-restart
  kubectl -n "$SYSTEM_NS" delete pod -l app.kubernetes.io/component=controller-manager --wait=false >/dev/null
  local end=$((SECONDS + TIMEOUT))
  while [ $SECONDS -lt $end ]; do
    jobs="$jobs $(kubectl -n "$SYSTEM_NS" get jobs -l "sandbox.kubeswift.io/export-sandbox-uid=$uid" -o jsonpath='{.items[*].metadata.uid}')"
    case $(field c13-restart "$NS" '{.status.phase}') in Completed | Failed) break ;; esac
    sleep 1
  done
  expect c13-restart "$NS" Completed Exported
  local n
  n=$(echo "$jobs" | tr ' ' '\n' | sort -u | grep -c . || true)
  [ "$n" = 1 ] && pass "c13-restart: one export Job across the restart" || fail "c13-restart: $n export Jobs"
}

case14() {
  local probe='env | grep -qiE "dockerconfig|registry_auth|export-cred" && exit 20
grep -rlsE "\"auths\"|dockerconfigjson|outputs-test-push" / --exclude-dir=proc --exclude-dir=sys --exclude-dir=dev 2>/dev/null | grep -q . && exit 21
grep -qiE "auth|docker|secret" /proc/cmdline && exit 22
test -e /var/run/secrets/kubernetes.io && exit 24
R=$(wget -q -T 5 -O - --no-check-certificate https://kubernetes.default.svc/api/v1/namespaces/'"$SYSTEM_NS"'/secrets/outputs-test-push 2>&1); echo "$R" | grep -q dockerconfigjson && exit 23
echo x > /output/f'
  sandbox c14-guest "$NS" "$probe" '{"network":{"mode":"open"}}'
  expect c14-guest "$NS" Completed Exported
  if kubectl -n "$NS" get pod c14-guest -o json | grep -q outputs-test-push; then
    fail "c14-guest: the launcher pod references the push Secret"
  else
    pass "c14-guest: the launcher pod does not reference the push Secret"
  fi
  local sa
  sa=$(kubectl -n "$NS" get pod c14-guest -o jsonpath='{.spec.serviceAccountName}')
  if [ "$(kubectl auth can-i get secrets -n "$SYSTEM_NS" --as="system:serviceaccount:$NS:$sa" || true)" = no ]; then
    pass "c14-guest: the launcher's service account cannot read Secrets in $SYSTEM_NS"
  else
    fail "c14-guest: the launcher's service account can read Secrets in $SYSTEM_NS"
  fi
}

case16() {
  if ! command -v cosign >/dev/null; then echo "SKIP: c16 (cosign not found)"; return; fi
  local ref
  ref=$(field rt-producer "$NS" '{.status.outputs[0].reference}')
  (cd "$WORK" && COSIGN_PASSWORD= cosign generate-key-pair >/dev/null 2>&1)
  python3 - "$WORK/cosign.pub" "$NS" "$REPOSITORY_PREFIX" <<'PY' | kubectl apply -f - >/dev/null
import json, sys
key, ns, prefix = open(sys.argv[1]).read(), sys.argv[2], sys.argv[3]
print(json.dumps({"apiVersion": "sandbox.kubeswift.io/v1alpha1", "kind": "SwiftArtifactVerificationPolicy",
  "metadata": {"name": "outputs-test"},
  "spec": {"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": ns}},
           "repositories": [prefix + "/**"],
           "authorities": [{"name": "signer", "key": {"publicKey": key}}],
           "claims": [{"key": "outputs-test.kubeswift.io/reviewed", "value": "true"}]}}))
PY
  consumer c16-unsigned "$ref" '{"requirePlatformVerification":true}'
  wait_done c16-unsigned "$NS" || true
  [ "$(field c16-unsigned "$NS" '{.status.phase}')" = Failed ] &&
    pass "c16: unsigned export refused ($(field c16-unsigned "$NS" '{.status.conditions[?(@.type=="GuestRunning")].reason}'))" ||
    fail "c16: unsigned export was not refused"
  # The signer is separate from KubeSwift; no transparency log upload.
  COSIGN_PASSWORD= cosign sign --yes --key "$WORK/cosign.key" --tlog-upload=false \
    -a outputs-test.kubeswift.io/reviewed=true "$ref" >/dev/null 2>&1
  consumer c16-signed "$ref" '{"requirePlatformVerification":true}'
  wait_done c16-signed "$NS" || true
  [ "$(field c16-signed "$NS" '{.status.phase}/{.status.exitCode}')" = Completed/0 ] &&
    pass "c16: signed export consumed under the policy" ||
    fail "c16: signed export $(field c16-signed "$NS" '{.status.phase} {.status.message}')"
  kubectl delete savp outputs-test --ignore-not-found >/dev/null
}

fixture() {
  local build='set -e; apk add --no-cache -q build-base >/dev/null; mkdir -p /src /output/bin /output/obj
printf "#include <stdio.h>\nvoid greet(const char *w) { printf(\"hello from %%s\\\\n\", w); }\n" > /src/greet.c
printf "void greet(const char *w);\nint main(void) { greet(\"an exported build\"); return 0; }\n" > /src/main.c
for f in greet main; do gcc -O2 -c /src/$f.c -o /output/obj/$f.o; done
gcc -static /output/obj/greet.o /output/obj/main.o -o /output/bin/hello
cd /output && find . -type f ! -name SHA256SUMS | sort | xargs sha256sum > SHA256SUMS'
  sandbox fx-build "$NS" "$build" '{"network":{"mode":"open"},"memory":"1Gi"}'
  TIMEOUT=600 expect fx-build "$NS" Completed Exported
  consumer fx-run "$(field fx-build "$NS" '{.status.outputs[0].reference}')" \
    "{\"check\":\"cd /in && sha256sum -c SHA256SUMS >/dev/null || exit 12; [ \\\"\$(/in/bin/hello)\\\" = 'hello from an exported build' ] || exit 15; touch /in/x 2>/dev/null && exit 13; exit 0\"}"
  wait_done fx-run "$NS" || true
  [ "$(field fx-run "$NS" '{.status.phase}/{.status.exitCode}')" = Completed/0 ] &&
    pass "fixture: exported build verified and run read-only" ||
    fail "fixture: $(field fx-run "$NS" '{.status.phase} exit={.status.exitCode} {.status.message}')"
}

# perf: per size, the launcher's exit and packaging lines, the export Job's
# first sight and its pod, and the first sight of Exported. Kubernetes
# timestamps have a 1 s resolution; first-sight times are sampled every 0.5 s.
perf_one() {
  local size=$1 name="perf-$1" uid t_job="" t_exp="" start finish
  kubectl -n "$NS" delete swiftsandbox "$name" --ignore-not-found --wait=true >/dev/null
  sandbox "$name" "$NS" "SIZE=$size; $PRODUCER"
  uid=$(field "$name" "$NS" '{.metadata.uid}')
  local end=$((SECONDS + 900)) now pods
  while [ $SECONDS -lt $end ]; do
    now=$(date -u +%s.%N)
    if [ -z "$t_job" ] && [ -n "$(kubectl -n "$SYSTEM_NS" get jobs -l "sandbox.kubeswift.io/export-sandbox-uid=$uid" -o name)" ]; then t_job=$now; fi
    pods=$(kubectl -n "$SYSTEM_NS" get pods -l "sandbox.kubeswift.io/export-sandbox-uid=$uid,app.kubernetes.io/component=sandbox-export" \
      -o jsonpath='{.items[0].status.containerStatuses[0].state.terminated.startedAt} {.items[0].status.containerStatuses[0].state.terminated.finishedAt}' 2>/dev/null || true)
    [ -n "${pods// /}" ] && read -r start finish <<<"$pods"
    if [ -z "$t_exp" ] && [ "$(field "$name" "$NS" '{.status.outputs[0].state}')" = Exported ]; then t_exp=$now; break; fi
    case $(field "$name" "$NS" '{.status.phase}') in Failed) break ;; esac
    sleep 0.5
  done
  kubectl -n "$NS" logs "$name" -c launcher >"$WORK/$name.log" 2>/dev/null || true
  python3 - "$WORK/$name.log" "$size" "$t_job" "$t_exp" "${start:-}" "${finish:-}" <<'PY'
import re, sys, datetime as dt
log, size, t_job, t_exp, start, finish = sys.argv[1:7]
ts = lambda s: dt.datetime.fromisoformat(s.replace("Z", "+00:00")).timestamp()
text = open(log).read()
ex = re.search(r"\[(\S+) INFO  swiftletd\] sandbox_exit_reported", text)
pk = re.search(r"\[(\S+) INFO  swiftletd\] output name=\S+ state=Packaged", text)
if not (ex and pk and t_job and t_exp and start and finish):
    print(f"{int(size):>10} incomplete"); sys.exit(1)
e = ts(ex.group(1))
print(f"{int(size):>10} packaging {ts(pk.group(1)) - e:6.2f}s  finalize->export start {float(t_job) - e:6.2f}s  "
      f"upload {ts(finish) - ts(start):5.1f}s  finalize->Exported {float(t_exp) - e:6.2f}s")
PY
}

perf() {
  echo "      size  (seconds after the guest's exit line)"
  for s in 262144 10485760 104857600; do perf_one "$s" || FAIL=$((FAIL + 1)); done
}

cleanup() {
  kubectl delete savp outputs-test --ignore-not-found >/dev/null
  kubectl delete saep outputs-test outputs-test-badcred outputs-test-unreachable outputs-test-tarpit --ignore-not-found >/dev/null
  kubectl -n "$SYSTEM_NS" delete secret outputs-test-push outputs-test-bad --ignore-not-found >/dev/null
  kubectl delete namespace "$NS" "$OTHER_NS" "$BAD_NS" "$NET_NS" --ignore-not-found >/dev/null
  echo "cleaned up (pushed test artifacts under $REPOSITORY_PREFIX are left in the registry)"
}

case ${1:-} in
validate)
  setup
  round_trip
  security
  fixture
  ;;
perf)
  setup
  perf
  ;;
cleanup) cleanup ;;
*)
  echo "usage: $0 validate|perf|cleanup" >&2
  exit 2
  ;;
esac

echo "passed $PASS, failed $FAIL"
[ "$FAIL" -eq 0 ]
