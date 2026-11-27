#!/usr/bin/env bash
#
# Sandbox hardening test: what a sandbox guest runs as and what its kernel and
# launcher give it.
#
# Guards the sandbox hardening in v0.25.0:
#   - Landlock and VM Generation ID in the sandbox kernel (#938): the guest's
#     LSM list has landlock, the vmgenid driver is bound, and nono confines a
#     command to a read-only workspace.
#   - the image USER and spec.securityContext (#940): the workload runs as the
#     image's user, or the one securityContext names; allowPrivilegeEscalation
#     false sets no_new_privs; runAsNonRoot over a root image fails before boot.
#   - nestedVirtualization (#942): Cloud Hypervisor gets nested=off by
#     default, nested=on with Enabled. Checked on the launcher's arguments:
#     the stock sandbox kernels have no KVM, and Linux clears the VMX flag
#     in a kernel without KVM, so the guest cannot show the difference.
#   - launcher requests (#943, #944): guest memory plus 64Mi, CPU_PER_VCPU per
#     vCPU, no limits.
#
# Why this needs a real cluster with KVM: each check reads the running guest
# (its kernel, its process credentials, its CPU flags) or the launcher pod
# the controller built. Unit tests cover the controller's decisions.
#
# Subcommands:
#   ./hardening.sh validate   # run the sandboxes, assert from the guest
#   ./hardening.sh cleanup    # delete the test namespace
#
# Prerequisites:
#   - a node labeled kubeswift.io/kernel-node=true with KVM
#   - KERNEL_IMAGE (default the current sandbox kernel) pullable by the nodes
#   - NONO_URL reachable from a restricted sandbox (public internet);
#     NONO_URL= skips the nono check
#   - CPU_PER_VCPU: the chart's sandboxMaterialize.launcherCPUPerVCPU
#     (default 250m)
#
# Acceptance: exit 0 on success. Non-zero on any check failing.

set -euo pipefail

NS="${NS:-sandbox-hardening}"
KERNEL_IMAGE="${KERNEL_IMAGE:-ghcr.io/kubeswift-io/kubeswift/kernels/sandbox:6.6.25}"
ROOT_IMAGE="${ROOT_IMAGE:-docker.io/library/busybox:1.36}"
# An image whose config names a non-root USER and has a shell.
USER_IMAGE="${USER_IMAGE:-docker.io/curlimages/curl:8.11.1}"
NONO_URL="${NONO_URL-https://github.com/nolabs-ai/nono/releases/download/v0.81.0/nono-v0.81.0-x86_64-unknown-linux-musl.tar.gz}"
CPU_PER_VCPU="${CPU_PER_VCPU:-250m}"
TIMEOUT="${TIMEOUT:-300}"

log() { printf '==> %s\n' "$*"; }
die() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

cleanup() {
	kubectl delete namespace "$NS" --ignore-not-found --wait=false >/dev/null
	log "deleted namespace $NS"
}

# sandbox NAME IMAGE CPU EXTRA_SPEC_YAML SCRIPT
sandbox() {
	local script
	script=$(printf '%s' "$5" | sed 's/^/      /')
	kubectl -n "$NS" delete swiftsandbox "$1" --ignore-not-found --wait=true >/dev/null
	cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: sandbox.kubeswift.io/v1alpha1
kind: SwiftSandbox
metadata: {name: $1, namespace: $NS}
spec:
  image: $2
  kernelProfileRef: {name: sandbox}
  cpu: $3
  memory: 256Mi
  network: {mode: restricted}
$4
  command: [sh, -c]
  args:
    - |
$script
EOF
}

field() { kubectl -n "$NS" get swiftsandbox "$1" -o jsonpath="$2"; }

# finished NAME: waits for the sandbox to end; prints its phase.
finished() {
	local deadline=$((SECONDS + TIMEOUT)) phase
	while :; do
		phase=$(field "$1" '{.status.phase}')
		case "$phase" in Completed|Failed) echo "$phase"; return ;; esac
		[ "$SECONDS" -lt "$deadline" ] || die "sandbox $1 still $phase after ${TIMEOUT}s"
		sleep 1
	done
}

# out NAME: the guest's CHECK lines from the console.
out() { kubectl -n "$NS" logs "$1" -c console 2>&1 | grep -E '^CHECK ' || true; }

# want NAME KEY VALUE: the guest printed "CHECK KEY=VALUE".
want() {
	grep -qxF "CHECK $2=$3" <<<"$(out "$1")" ||
		die "sandbox $1: want $2=$3, guest printed: $(out "$1" | tr '\n' ' ')"
}

# cpus NAME WANT: the launcher started Cloud Hypervisor with --cpus ...,WANT.
cpus() {
	local args
	args=$(kubectl -n "$NS" logs "$1" -c launcher 2>/dev/null | grep -o '"--cpus", "[^"]*"' | head -1)
	grep -q ",$2\"" <<<"$args" || die "sandbox $1: Cloud Hypervisor $args, want $2"
	log "sandbox $1: Cloud Hypervisor ${args//\"/}"
}

# ran NAME: the sandbox Completed with exit code 0.
ran() {
	local phase
	phase=$(finished "$1")
	[ "$phase" = Completed ] && [ "$(field "$1" '{.status.exitCode}')" = 0 ] ||
		die "sandbox $1 $phase (exit $(field "$1" '{.status.exitCode}')): $(field "$1" '{.status.message}') $(out "$1" | tr '\n' ' ')"
}

validate() {
	kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
	kubectl -n "$NS" delete swiftsandbox --all --wait=true >/dev/null
	cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: kernel.kubeswift.io/v1alpha1
kind: SwiftKernel
metadata: {name: sandbox, namespace: $NS}
spec: {profile: sandbox, kernelCmdline: console=ttyS0, ociRef: {image: "$KERNEL_IMAGE"}}
EOF
	kubectl -n "$NS" wait --for=jsonpath='{.status.phase}'=Ready swiftkernel/sandbox --timeout=300s >/dev/null ||
		die "SwiftKernel sandbox not Ready in $NS"

	# 1. Kernel features, nested off by default, and the launcher's requests.
	sandbox kernel "$ROOT_IMAGE" 2 "" '
echo "CHECK lsm-landlock=$(tr , "\n" </sys/kernel/security/lsm | grep -cx landlock)"
echo "CHECK vmgenid-bound=$(ls /sys/bus/acpi/drivers/vmgenid/ | grep -c ":")"'
	local pod mem cpu lim
	pod=""
	for _ in $(seq 60); do
		pod=$(kubectl -n "$NS" get pods -l sandbox.kubeswift.io/sandbox=kernel -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
		[ -n "$pod" ] && break
		sleep 1
	done
	[ -n "$pod" ] || die "no launcher pod for sandbox kernel"
	mem=$(kubectl -n "$NS" get pod "$pod" -o jsonpath='{.spec.containers[?(@.name=="launcher")].resources.requests.memory}')
	cpu=$(kubectl -n "$NS" get pod "$pod" -o jsonpath='{.spec.containers[?(@.name=="launcher")].resources.requests.cpu}')
	lim=$(kubectl -n "$NS" get pod "$pod" -o jsonpath='{.spec.containers[?(@.name=="launcher")].resources.limits}')
	ran kernel
	want kernel lsm-landlock 1
	want kernel vmgenid-bound 1
	log "kernel: landlock in the LSM list, vmgenid bound"
	cpus kernel nested=off
	[ "$mem" = 320Mi ] || die "launcher memory request $mem, want 320Mi (256Mi guest + 64Mi)"
	local want_cpu
	want_cpu=$(python3 -c "import sys; q=sys.argv[1]; m=float(q[:-1])/1000 if q.endswith('m') else float(q); v=m*2; print(f'{int(v)}' if v==int(v) else f'{int(round(v*1000))}m')" "$CPU_PER_VCPU")
	[ "$cpu" = "$want_cpu" ] || die "launcher cpu request $cpu, want $want_cpu (2 x $CPU_PER_VCPU)"
	[ -z "$lim" ] || die "launcher has limits: $lim"
	log "launcher requests $cpu CPU and $mem, no limits"

	sandbox nested "$ROOT_IMAGE" 1 "  nestedVirtualization: Enabled" 'true'
	ran nested
	cpus nested nested=on

	# 2. Who the workload runs as.
	sandbox imageuser "$USER_IMAGE" 1 "" '
echo "CHECK uid=$(id -u)"'
	ran imageuser
	local iu
	iu=$(field imageuser '{.status.user}')
	[ -n "$iu" ] && [ "$(out imageuser)" != "CHECK uid=0" ] ||
		die "image USER ignored: status.user=$iu, guest printed $(out imageuser)"
	log "image USER honoured: status.user=$iu, $(out imageuser | sed 's/CHECK //')"

	sandbox explicit "$ROOT_IMAGE" 1 "  securityContext: {runAsUser: 1000, runAsGroup: 1000, allowPrivilegeEscalation: false}" '
echo "CHECK uid=$(id -u)"
echo "CHECK gid=$(id -g)"
echo "CHECK nnp=$(awk "/^NoNewPrivs/{print \$2}" /proc/self/status)"'
	ran explicit
	want explicit uid 1000
	want explicit gid 1000
	want explicit nnp 1
	log "securityContext runAsUser/runAsGroup 1000 and no_new_privs applied"

	sandbox nonroot "$ROOT_IMAGE" 1 "  securityContext: {runAsNonRoot: true}" 'echo should-not-run'
	local phase reason
	phase=$(finished nonroot)
	reason=$(kubectl -n "$NS" get swiftsandbox nonroot -o jsonpath='{.status}')
	[ "$phase" = Failed ] && grep -q RunAsNonRootViolation <<<"$reason" ||
		die "runAsNonRoot over a root image: phase $phase, $reason"
	log "runAsNonRoot over a root image fails before boot (RunAsNonRootViolation)"

	# 3. Landlock confinement with nono, as a non-root workload.
	if [ -n "$NONO_URL" ]; then
		sandbox nono "$ROOT_IMAGE" 1 "  securityContext: {runAsUser: 1000, runAsGroup: 1000}" "
cd /tmp && wget -q -O nono.tgz '$NONO_URL' && tar xzf nono.tgz || { echo 'CHECK nono=download-failed'; exit 1; }
N=\$(find /tmp -name nono -type f | head -1)
mkdir -p /tmp/ws && echo hello > /tmp/ws/file
# From /: in /tmp nono asks whether to share the working directory. HOME:
# uid 1000 has no passwd entry, so HOME is / and nono cannot write its state.
export HOME=/tmp/home && mkdir -p \$HOME
cd / && \$N run --read /tmp/ws -- sh -c 'cat /tmp/ws/file; echo x > /tmp/ws/blocked' </dev/null >/tmp/nono.out 2>&1 || true
sed 's/^/nono: /' /tmp/nono.out
echo \"CHECK nono-read=\$(grep -c hello /tmp/nono.out)\"
echo \"CHECK nono-write-blocked=\$([ -e /tmp/ws/blocked ] && echo no || echo yes)\"
# Control: the directory is the workload's own, so without nono the write works.
echo y > /tmp/ws/control && echo \"CHECK control-write=yes\""
		ran nono
		kubectl -n "$NS" logs nono -c console 2>&1 | grep '^nono: ' | sed 's/^/   /' || true
		want nono nono-read 1
		want nono nono-write-blocked yes
		want nono control-write yes
		log "nono (Landlock) as uid 1000: workspace readable, write refused"
	fi
	log "PASS: sandbox hardening checks"
}

case "${1:-}" in
validate) validate ;;
cleanup) cleanup ;;
*) echo "usage: $0 validate|cleanup" >&2; exit 2 ;;
esac
