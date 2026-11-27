#!/usr/bin/env bash
#
# Sandbox policy pack (docs/sandbox/policy.md): the chart's
# ValidatingAdmissionPolicies refuse what the platform does not allow in a
# namespace labelled kubeswift.io/sandbox-policy=enforced, and nothing else.
#
# Checks, with server-side dry-run (no sandbox starts):
#   - network mode outside sandboxPolicy.allowedNetworkModes is refused, for a
#     SwiftSandbox and a SwiftSandboxPool; an allowed one is admitted;
#   - env valueFrom.secretKeyRef and secretFiles are refused;
#   - nestedVirtualization: Enabled is refused;
#   - an egress proxy image outside sandboxPolicy.allowedProxyImages is
#     refused, a preset is admitted;
#   - an unlabelled namespace is not affected;
# and, with one real pool (minWarm 0, so nothing boots), that a metadata update
# of an object admitted before its namespace was labelled still passes while a
# spec change is refused.
#
# Subcommands:
#   ./sandbox-policy.sh validate
#   ./sandbox-policy.sh cleanup
#
# Prerequisites: the chart with sandboxPolicy.enabled and the default rules
# (allowedNetworkModes [brokered, proxied], forbidGuestSecrets,
# requireNestedDisabled, allowedProxyImages empty, validationActions [Deny]).

set -euo pipefail

NS="${NS:-sandbox-policy}"
FREE="${FREE:-sandbox-policy-free}"
IMAGE="${IMAGE:-docker.io/library/busybox:1.37.0}"
MARK="kubeswift.io/sandbox-policy=enforced"

log() { printf '==> %s\n' "$*"; }
die() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

cleanup() {
	kubectl delete namespace "$NS" "$FREE" --ignore-not-found --wait=false >/dev/null
	log "deleted namespaces $NS $FREE"
}

# apply NS KIND NAME SPEC_LINES [EXTRA_KUBECTL_ARGS]: server-side dry-run
# create; prints the API server's answer.
apply() {
	local ns=$1 kind=$2 name=$3 spec=$4
	shift 4
	cat <<EOF | kubectl apply --dry-run=server "$@" -f - 2>&1 || true
apiVersion: sandbox.kubeswift.io/v1alpha1
kind: $kind
metadata: {name: $name, namespace: $ns}
spec:
  image: $IMAGE
  kernelProfileRef: {name: sandbox}
$spec
EOF
}

refused() { # refused WHAT NS KIND SPEC
	local out
	out=$(apply "$2" "$3" "p-$RANDOM" "$4")
	grep -q "$MARK" <<<"$out" || die "$1 was not refused by the policy: $out"
	log "refused: $1"
}

admitted() { # admitted WHAT NS KIND SPEC
	local out
	out=$(apply "$2" "$3" "p-$RANDOM" "$4")
	grep -q "$MARK" <<<"$out" && die "$1 was refused by the policy: $out"
	log "admitted: $1"
}

BROKERED='  network: {mode: brokered}'

validate() {
	kubectl get validatingadmissionpolicy kubeswift-sandbox-policy >/dev/null 2>&1 ||
		die "the policy pack is not installed (chart sandboxPolicy.enabled)"
	kubectl create namespace "$NS" >/dev/null 2>&1 || true
	kubectl create namespace "$FREE" >/dev/null 2>&1 || true

	log "a pool admitted before the label"
	cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: sandbox.kubeswift.io/v1alpha1
kind: SwiftSandboxPool
metadata: {name: before, namespace: $NS}
spec:
  image: $IMAGE
  kernelProfileRef: {name: sandbox}
  minWarm: 0
  network: {mode: restricted}
EOF
	kubectl label namespace "$NS" "$MARK" --overwrite >/dev/null
	sleep 2

	refused "default network mode (restricted)" "$NS" SwiftSandbox ""
	refused "network mode open" "$NS" SwiftSandbox '  network: {mode: open}'
	refused "pool in network mode restricted" "$NS" SwiftSandboxPool '  network: {mode: restricted}'
	admitted "network mode brokered" "$NS" SwiftSandbox "$BROKERED"
	admitted "network mode proxied with a preset" "$NS" SwiftSandbox '  network:
    mode: proxied
    egress:
      allow: [{host: example.com}]
      proxy: {preset: envoy}'
	admitted "pool in network mode brokered" "$NS" SwiftSandboxPool "$BROKERED"
	refused "env from a Secret" "$NS" SwiftSandbox "$BROKERED
  env:
  - name: TOKEN
    valueFrom: {secretKeyRef: {name: s, key: k}}"
	admitted "plain env" "$NS" SwiftSandbox "$BROKERED
  env: [{name: A, value: b}]"
	refused "secretFiles" "$NS" SwiftSandbox "$BROKERED
  secretFiles: [{secretName: s, items: [{key: k, path: /etc/x}]}]"
	refused "nestedVirtualization Enabled" "$NS" SwiftSandbox "$BROKERED
  nestedVirtualization: Enabled"
	refused "egress proxy image not allowed" "$NS" SwiftSandbox '  network:
    mode: proxied
    egress:
      allow: [{host: example.com}]
      proxy: {image: docker.io/library/nginx:1.27}'
	admitted "restricted in a namespace without the label" "$FREE" SwiftSandbox ""

	log "an object admitted before the label"
	kubectl -n "$NS" label swiftsandboxpool before touched=yes --overwrite >/dev/null ||
		die "a metadata update of pool before was refused"
	log "admitted: metadata update"
	out=$(kubectl -n "$NS" patch swiftsandboxpool before --type=merge -p '{"spec":{"minWarm":1}}' 2>&1 || true)
	grep -q "$MARK" <<<"$out" || die "a spec change of pool before was not refused: $out"
	log "refused: spec change while still non-compliant"
	kubectl -n "$NS" delete swiftsandboxpool before --wait=true --timeout=60s >/dev/null || die "pool before cannot be deleted"
	log "deleted pool before"

	log "PASS: the policy pack refuses what it should, and only that"
}

case "${1:-}" in
validate) validate ;;
cleanup) cleanup ;;
*)
	echo "usage: $0 validate|cleanup" >&2
	exit 2
	;;
esac
