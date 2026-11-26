#!/usr/bin/env bash
#
# Pod mode acceptance test: ordinary Pods with runtimeClassName
# kubeswift-microvm run in microVMs behind an unprivileged stand-in.
#
# Checks, on a cluster with sandboxMaterialize.podMode.enabled:
#   - fail closed: a Pod naming the RuntimeClass outside an opted-in
#     namespace, a field the microVM cannot honour, and a hand-made stand-in
#     are refused at admission;
#   - the stand-in is the shim (pod-shim image) and its workload runs in the
#     guest: a Service reaches it, `kubectl exec` and `kubectl logs` reach the
#     guest, exec carries the exit code;
#   - Secrets and ConfigMaps (env, envFrom, volumes) reach the guest, no value
#     is in the SwiftSandbox, a missing Secret holds the microVM until it
#     exists;
#   - the workload's exit code ends the Pod, a Job completes, restartPolicy
#     Always boots a new microVM per restart;
#   - the relay refuses a Pod that is not a stand-in, even with a token for
#     its audience;
#   - deleting a stand-in removes its SwiftSandbox, egress policy and Secret
#     copy.
#
# Why this needs a real cluster with KVM: every check is a microVM booted by
# the controller from an admitted Pod. Unit tests cover the rewrite and the
# controller's decisions.
#
# Subcommands:
#   ./pod-mode.sh validate   # run the checks
#   ./pod-mode.sh cleanup    # delete the test namespaces (and the kernel it made)
#
# Prerequisites:
#   - the chart with sandboxMaterialize.podMode.enabled and webhook.enabled
#   - a node labeled kubeswift.io/kernel-node=true with KVM
#   - KERNEL_IMAGE pullable by the nodes: when the pod-mode namespace has no
#     SwiftKernel "sandbox", the test creates one (and cleanup deletes it)
#
# Acceptance: exit 0 on success. Non-zero on any check failing.

set -euo pipefail

NS="${NS:-pod-mode-test}"
OTHER_NS="${OTHER_NS:-pod-mode-test-other}"
PM_NS="${PM_NS:-kubeswift-sandboxes}"
KERNEL_IMAGE="${KERNEL_IMAGE:-ghcr.io/kubeswift-io/kubeswift/kernels/sandbox:6.6.24}"
IMAGE="${IMAGE:-docker.io/library/busybox:1.37.0}"
CURL_IMAGE="${CURL_IMAGE:-docker.io/curlimages/curl:8.11.1}"
TIMEOUT="${TIMEOUT:-300}"
TEST_LABEL="kubeswift.io/test=pod-mode"

# Pipelines run with pipefail: grep reads all its input (no -q), so the
# writer never dies of SIGPIPE and fails the pipeline after a match.
log() { printf '==> %s\n' "$*"; }
die() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

cleanup() {
	kubectl delete namespace "$NS" "$OTHER_NS" --ignore-not-found --wait=false >/dev/null
	kubectl -n "$PM_NS" delete swiftkernel -l "$TEST_LABEL" --ignore-not-found >/dev/null 2>&1 || true
	log "deleted namespaces $NS and $OTHER_NS"
}

# until SECONDS DESCRIPTION CMD...: run CMD until it succeeds.
until_ok() {
	local deadline=$((SECONDS + $1)) what=$2
	shift 2
	until "$@" >/dev/null 2>&1; do
		[ "$SECONDS" -lt "$deadline" ] || die "timed out: $what"
		sleep 2
	done
}

# refused NAMESPACE WANT: create the Pod on stdin (server dry run) and
# expect admission to refuse it with a message containing WANT.
refused() {
	local out
	if out=$(kubectl -n "$1" create --dry-run=server -f - 2>&1); then
		die "admitted, want refused with \"$2\""
	fi
	case "$out" in *"$2"*) ;; *) die "refused for another reason: $out" ;; esac
}

pod_field() { kubectl -n "$NS" get pod "$1" -o jsonpath="$2"; }
pod_uid() { pod_field "$1" '{.metadata.uid}'; }
condition() { pod_field "$1" "{.status.conditions[?(@.type==\"$2\")].$3}"; }

setup() {
	kubectl get runtimeclass kubeswift-microvm >/dev/null 2>&1 ||
		die "RuntimeClass kubeswift-microvm not found: install the chart with sandboxMaterialize.podMode.enabled"
	kubectl get namespace "$PM_NS" >/dev/null || die "pod-mode namespace $PM_NS not found"
	if ! kubectl -n "$PM_NS" get swiftkernel sandbox >/dev/null 2>&1; then
		log "creating SwiftKernel $PM_NS/sandbox ($KERNEL_IMAGE)"
		cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: kernel.kubeswift.io/v1alpha1
kind: SwiftKernel
metadata: {name: sandbox, namespace: $PM_NS, labels: {kubeswift.io/test: pod-mode}}
spec: {profile: sandbox, kernelCmdline: console=ttyS0, ociRef: {image: "$KERNEL_IMAGE"}}
EOF
	fi
	kubectl -n "$PM_NS" wait swiftkernel/sandbox --for=jsonpath='{.status.phase}'=Ready --timeout="${TIMEOUT}s" >/dev/null
	cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Namespace
metadata:
  name: $NS
  labels: {kubeswift.io/pod-mode: enabled, pod-security.kubernetes.io/enforce: restricted}
---
apiVersion: v1
kind: Namespace
metadata:
  # No Pod Security level, so its refusal cannot hide the pod-mode one.
  name: $OTHER_NS
EOF
}

fail_closed() {
	log "fail closed: a Pod naming the RuntimeClass outside an opted-in namespace"
	refused "$OTHER_NS" "was not rewritten into a stand-in" <<EOF
apiVersion: v1
kind: Pod
metadata: {name: outside}
spec:
  runtimeClassName: kubeswift-microvm
  containers: [{name: main, image: $IMAGE}]
EOF
	log "fail closed: a field the microVM cannot honour is refused by name"
	refused "$NS" "not supported in pod mode: spec.volumes[host]" <<EOF
apiVersion: v1
kind: Pod
metadata: {name: hostpath}
spec:
  runtimeClassName: kubeswift-microvm
  containers: [{name: main, image: $IMAGE, volumeMounts: [{name: host, mountPath: /host}]}]
  volumes: [{name: host, hostPath: {path: /}}]
EOF
	log "fail closed: a hand-made stand-in"
	refused "$NS" "a stand-in has exactly the shim container" <<EOF
apiVersion: v1
kind: Pod
metadata: {name: forged, labels: {podmode.kubeswift.io/stand-in: "true"}}
spec:
  securityContext: {runAsNonRoot: true, runAsUser: 1000, seccompProfile: {type: RuntimeDefault}}
  containers:
  - name: main
    image: $IMAGE
    securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}
EOF
}

web() {
	log "a web Pod: stand-in shape, Service, exec, logs"
	cat <<EOF | kubectl -n "$NS" apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata: {name: web, labels: {app: web}}
spec:
  runtimeClassName: kubeswift-microvm
  containers:
  - name: main
    image: $IMAGE
    command: [sh, -c, "mkdir -p /www && uname -r > /www/index.html && exec httpd -f -p 8080 -h /www"]
    ports: [{containerPort: 8080}]
    readinessProbe: {httpGet: {path: /, port: 8080}}
---
apiVersion: v1
kind: Service
metadata: {name: web}
spec:
  selector: {app: web}
  ports: [{port: 80, targetPort: 8080}]
EOF
	kubectl -n "$NS" wait pod/web --for=condition=Ready --timeout="${TIMEOUT}s" >/dev/null ||
		die "web not Ready: $(condition web podmode.kubeswift.io/microvm-ready message)"
	case "$(pod_field web '{.spec.containers[0].image}')" in
		*/pod-shim:*) ;;
		*) die "stand-in image $(pod_field web '{.spec.containers[0].image}'), want pod-shim" ;;
	esac
	[ "$(pod_field web '{.spec.runtimeClassName}')" = "" ] || die "the stand-in still names a RuntimeClass"
	local node guest node_kernel
	node=$(pod_field web '{.spec.nodeName}')
	node_kernel=$(kubectl get node "$node" -o jsonpath='{.status.nodeInfo.kernelVersion}')
	guest=$(kubectl -n "$NS" exec web -- sh -c 'uname -r')
	[ -n "$guest" ] && [ "$guest" != "$node_kernel" ] || die "exec ran on the node (uname -r $guest), not in the guest"
	log "exec runs in the guest: $guest (node: $node_kernel)"
	local rc=0
	kubectl -n "$NS" exec web -- sh -c 'exit 7' 2>/dev/null || rc=$?
	[ "$rc" = 7 ] || die "exec exit code $rc, want 7"
	kubectl -n "$NS" exec web -- ls / >/dev/null 2>&1 && die "exec of a command other than the shell ran locally"
	kubectl -n "$NS" logs web | grep "Linux version" >/dev/null || die "kubectl logs does not show the guest console"

	local got
	got=$(kubectl -n "$NS" run curl --rm -i --restart=Never --image="$CURL_IMAGE" --quiet \
		--overrides='{"spec":{"securityContext":{"runAsNonRoot":true,"runAsUser":1000,"seccompProfile":{"type":"RuntimeDefault"}},"containers":[{"name":"curl","image":"'"$CURL_IMAGE"'","command":["curl","-sf","-m","10","http://web"],"securityContext":{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]}}}]}}' 2>/dev/null | tr -d '\r')
	[ "$got" = "$guest" ] || die "the Service answered \"$got\", want the guest's \"$guest\""
	log "the Service reaches the guest"
}

config() {
	log "Secrets and ConfigMaps reach the guest"
	cat <<EOF | kubectl -n "$NS" apply -f - >/dev/null
apiVersion: v1
kind: Secret
metadata: {name: creds}
stringData: {token: tok-$RANDOM, tls.key: key-$RANDOM}
---
apiVersion: v1
kind: ConfigMap
metadata: {name: settings}
data: {MODE: fast, app.yaml: "listen: 8080"}
---
apiVersion: v1
kind: Pod
metadata: {name: cfg}
spec:
  runtimeClassName: kubeswift-microvm
  restartPolicy: Never
  containers:
  - name: main
    image: $IMAGE
    command: [sh, -c, 'echo "TOKEN=\$TOKEN CFG_MODE=\$CFG_MODE"; echo "KEY=\$(cat /etc/tls/tls.key) MODE=\$(stat -c %a /etc/tls/tls.key) APP=\$(cat /etc/app.yaml)"; sleep 30']
    env: [{name: TOKEN, valueFrom: {secretKeyRef: {name: creds, key: token}}}]
    envFrom: [{prefix: CFG_, configMapRef: {name: settings}}]
    volumeMounts:
    - {name: tls, mountPath: /etc/tls, readOnly: true}
    - {name: conf, mountPath: /etc/app.yaml, subPath: app.yaml}
  volumes:
  - {name: tls, secret: {secretName: creds, items: [{key: tls.key, path: tls.key, mode: 0400}]}}
  - {name: conf, configMap: {name: settings}}
---
apiVersion: v1
kind: Pod
metadata: {name: late}
spec:
  runtimeClassName: kubeswift-microvm
  restartPolicy: Never
  containers:
  - name: main
    image: $IMAGE
    command: [sh, -c, 'echo "LATE=\$LATE"; sleep 30']
    env: [{name: LATE, valueFrom: {secretKeyRef: {name: late, key: v}}}]
EOF
	local token key uid
	token=$(kubectl -n "$NS" get secret creds -o jsonpath='{.data.token}' | base64 -d)
	key=$(kubectl -n "$NS" get secret creds -o jsonpath='{.data.tls\.key}' | base64 -d)
	kubectl -n "$NS" wait pod/cfg --for=condition=Ready --timeout="${TIMEOUT}s" >/dev/null ||
		die "cfg not Ready: $(condition cfg podmode.kubeswift.io/microvm-ready message)"
	until_ok 60 "cfg output" sh -c "kubectl -n $NS logs cfg | grep -q '^KEY='"
	kubectl -n "$NS" logs cfg | grep -x "TOKEN=$token CFG_MODE=fast" >/dev/null || die "env not set in the guest: $(kubectl -n "$NS" logs cfg | grep TOKEN=)"
	kubectl -n "$NS" logs cfg | grep -x "KEY=$key MODE=400 APP=listen: 8080" >/dev/null || die "files not in the guest: $(kubectl -n "$NS" logs cfg | grep KEY=)"
	uid=$(pod_uid cfg)
	kubectl -n "$PM_NS" get swiftsandbox "pm-$uid" -o json | grep -E "$token|$key" >/dev/null && die "a Secret value is in the SwiftSandbox"
	kubectl -n "$PM_NS" get secret "pm-$uid" >/dev/null || die "no Secret copy pm-$uid in $PM_NS"

	until_ok 60 "late waits for its Secret" sh -c "[ \"\$(kubectl -n $NS get pod late -o jsonpath='{.status.conditions[?(@.type==\"podmode.kubeswift.io/microvm-ready\")].reason}')\" = MicroVMConfigError ]"
	kubectl -n "$PM_NS" get swiftsandbox "pm-$(pod_uid late)" >/dev/null 2>&1 && die "a microVM was created without its Secret"
	kubectl -n "$NS" create secret generic late --from-literal=v=here >/dev/null
	kubectl -n "$NS" wait pod/late --for=condition=Ready --timeout="${TIMEOUT}s" >/dev/null || die "late did not start once its Secret existed"
	until_ok 60 "late output" sh -c "kubectl -n $NS logs late | grep -qx 'LATE=here'"
	log "env, envFrom, files and a late Secret: ok"
}

lifecycle() {
	log "exit code, Job, restarts"
	cat <<EOF | kubectl -n "$NS" apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata: {name: exit3}
spec:
  runtimeClassName: kubeswift-microvm
  restartPolicy: Never
  containers: [{name: main, image: $IMAGE, command: [sh, -c, "sleep 2; exit 3"]}]
---
apiVersion: v1
kind: Pod
metadata: {name: crashy}
spec:
  runtimeClassName: kubeswift-microvm
  restartPolicy: Always
  containers: [{name: main, image: $IMAGE, command: [sh, -c, "sleep 3; exit 1"]}]
---
apiVersion: batch/v1
kind: Job
metadata: {name: job}
spec:
  backoffLimit: 0
  template:
    spec:
      runtimeClassName: kubeswift-microvm
      restartPolicy: Never
      containers: [{name: main, image: $IMAGE, command: [sh, -c, "echo done"]}]
EOF
	# The Job's Pod template is checked against Pod Security as written, so
	# the apply warns; its Pods are rewritten into stand-ins, which pass.
	kubectl -n "$NS" wait pod/exit3 --for=jsonpath='{.status.phase}'=Failed --timeout="${TIMEOUT}s" >/dev/null || die "exit3 did not fail"
	local code
	code=$(pod_field exit3 '{.status.containerStatuses[0].state.terminated.exitCode}')
	[ "$code" = 3 ] || die "exit3 ended with exit code $code, want 3"
	kubectl -n "$NS" wait job/job --for=condition=Complete --timeout="${TIMEOUT}s" >/dev/null || die "the Job did not complete"
	until_ok "$TIMEOUT" "crashy restarted in a new microVM" sh -c \
		"[ \"\$(kubectl -n $NS get pod crashy -o jsonpath='{.metadata.annotations.podmode\.kubeswift\.io/run}')\" -ge 2 ] && [ \"\$(kubectl -n $NS get pod crashy -o jsonpath='{.status.containerStatuses[0].restartCount}')\" -ge 1 ]"
	log "exit code 3, Job complete, restart in run $(pod_field crashy '{.metadata.annotations.podmode\.kubeswift\.io/run}')"
}

relay_auth() {
	log "the relay refuses a Pod that is not a stand-in"
	local addr
	addr=$(pod_field web '{.metadata.annotations.podmode\.kubeswift\.io/relay}' | sed -n 's/.*"addr":"\([^"]*\)".*/\1/p')
	[ -n "$addr" ] || die "web has no relay annotation"
	cat <<EOF | kubectl -n "$NS" apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata: {name: intruder}
spec:
  securityContext: {runAsNonRoot: true, runAsUser: 1000, seccompProfile: {type: RuntimeDefault}}
  containers:
  - name: c
    image: $CURL_IMAGE
    command: [sleep, "3600"]
    securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}
    volumeMounts: [{name: t, mountPath: /t}]
  volumes:
  - name: t
    projected: {sources: [{serviceAccountToken: {audience: kubeswift-podmode, path: token}}]}
EOF
	kubectl -n "$NS" wait pod/intruder --for=condition=Ready --timeout="${TIMEOUT}s" >/dev/null
	local status
	status=$(kubectl -n "$NS" exec intruder -- sh -c "curl -sk -m 10 -o /dev/null -w '%{http_code}' -H \"Authorization: Bearer \$(cat /t/token)\" https://$addr/podmode/logs")
	[ "$status" = 403 ] || die "relay answered $status to a Pod that is not a stand-in, want 403"
}

teardown() {
	log "deleting a stand-in removes its microVM, policy and Secret copy"
	local uid
	uid=$(pod_uid cfg)
	kubectl -n "$NS" delete pod web cfg --wait=true --timeout="${TIMEOUT}s" >/dev/null
	until_ok 120 "SwiftSandbox and Secret copy of cfg removed" sh -c \
		"! kubectl -n $PM_NS get swiftsandbox pm-$uid && ! kubectl -n $PM_NS get secret pm-$uid && ! kubectl -n $NS get networkpolicy kubeswift-podmode-$uid"
}

case "${1:-}" in
	validate)
		setup
		fail_closed
		web
		config
		lifecycle
		relay_auth
		teardown
		log "PASS"
		;;
	cleanup) cleanup ;;
	*) echo "usage: $0 validate|cleanup" >&2; exit 2 ;;
esac
