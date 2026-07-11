#!/usr/bin/env bash
#
# B0 cluster integration test — cross-node pod-to-pod TCP from launcher pods.
#
# Verifies the launcher pod's internal br0 subnet does NOT collide with
# the cluster's per-node Calico pod CIDR allocations, which would shadow
# the eth0 default route for return traffic in the colliding /24 and
# silently drop SYN-ACK replies. See:
#   docs/design/live-migration-phase-3a-spike.md  (B0 finding)
#
# Subcommands:
#   ./b0-cross-node-tcp.sh validate   # full pass: structural + cross-node TCP
#   ./b0-cross-node-tcp.sh cleanup    # tear down test resources
#
# What "validate" exercises:
#   1. Static check: br0 IP inside the launcher pod is in the
#      192.168.99.0/24 default. Catches any revert of the B0 constant.
#   2. Structural check: br0 subnet does NOT overlap with the launcher
#      pod's eth0 (Calico-assigned) /24. Catches B0-shape regressions
#      even if the constant is changed to a different value.
#   3. Cross-node TCP probe: from a busybox pod on a DIFFERENT node,
#      open a TCP connection to a temporary nc listener inside the
#      launcher pod's launcher container. Asserts SYN-ACK round-trip
#      completes — which is exactly what B0 was breaking.
#
# Prerequisites:
#   - 2+ nodes labeled kubeswift.io/kernel-node=true
#   - SwiftKernel `faas-minimal` Ready (or set SWIFTKERNEL env var)
#   - SwiftGuestClass `default` exists
#
# Acceptance: exit 0 on success. Non-zero on any check failing.

set -euo pipefail

NS="${NS:-default}"
SG_NAME="${SG_NAME:-b0-cross-node}"
PROBE_POD="${PROBE_POD:-b0-probe}"
SWIFTKERNEL="${SWIFTKERNEL:-faas-minimal}"
SWIFTGUESTCLASS="${SWIFTGUESTCLASS:-default}"
PORT="${PORT:-12345}"
TIMEOUT_SECS="${TIMEOUT_SECS:-180}"

cmd="${1:-help}"

ensure_two_kernel_nodes() {
    local nodes
    nodes=$(kubectl get nodes -l kubeswift.io/kernel-node=true \
        -o jsonpath='{.items[*].metadata.name}')
    local count
    count=$(echo "$nodes" | wc -w)
    if [ "$count" -lt 2 ]; then
        echo "ERROR: need ≥2 nodes labeled kubeswift.io/kernel-node=true (have $count: $nodes)"
        exit 1
    fi
    SOURCE_NODE=$(echo "$nodes" | awk '{print $1}')
    PROBE_NODE=$(echo "$nodes" | awk '{print $2}')
    echo "  source node:  $SOURCE_NODE"
    echo "  probe node:   $PROBE_NODE"
}

ensure_prereqs() {
    kubectl get swiftkernel "$SWIFTKERNEL" -n "$NS" >/dev/null \
        || { echo "ERROR: SwiftKernel $NS/$SWIFTKERNEL not found"; exit 1; }
    local phase
    phase=$(kubectl get swiftkernel "$SWIFTKERNEL" -n "$NS" -o jsonpath='{.status.phase}')
    [ "$phase" = "Ready" ] \
        || { echo "ERROR: SwiftKernel $SWIFTKERNEL phase=$phase, need Ready"; exit 1; }
    kubectl get swiftguestclass "$SWIFTGUESTCLASS" -n "$NS" >/dev/null \
        || { echo "ERROR: SwiftGuestClass $NS/$SWIFTGUESTCLASS not found"; exit 1; }
}

apply_swiftguest() {
    cat <<EOF | kubectl apply -f -
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuest
metadata:
  name: $SG_NAME
  namespace: $NS
spec:
  kernelRef:
    name: $SWIFTKERNEL
  guestClassRef:
    name: $SWIFTGUESTCLASS
  kernelCmdline: "console=ttyS0 root=/dev/ram0 rdinit=/init"
  runPolicy: Running
  nodeName: $SOURCE_NODE
EOF
}

wait_swiftguest_running() {
    local deadline=$(( $(date +%s) + TIMEOUT_SECS ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        local phase
        phase=$(kubectl get swiftguest "$SG_NAME" -n "$NS" \
            -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
        [ "$phase" = "Running" ] && return 0
        sleep 2
    done
    echo "ERROR: SwiftGuest $SG_NAME did not reach Running within ${TIMEOUT_SECS}s"
    exit 1
}

check_static_subnet() {
    echo "==> static check: launcher br0 IP is in 192.168.99.0/24"
    local br_ip
    br_ip=$(kubectl exec "$SG_NAME" -n "$NS" -c launcher -- \
        ip -4 addr show br0 2>/dev/null | awk '/inet /{print $2}' | head -1)
    if [ -z "$br_ip" ]; then
        echo "ERROR: br0 has no IP inside launcher pod"
        exit 1
    fi
    echo "  br0 IP: $br_ip"
    case "$br_ip" in
        192.168.99.*/24) echo "  PASS — within 192.168.99.0/24 default" ;;
        *) echo "ERROR: br0 IP $br_ip is NOT in the 192.168.99.0/24 default. B0 may have regressed."; exit 1 ;;
    esac
}

check_no_collision_with_eth0() {
    echo "==> structural check: br0 subnet ≠ eth0 /24 subnet"
    local eth0_ip
    eth0_ip=$(kubectl get pod "$SG_NAME" -n "$NS" -o jsonpath='{.status.podIP}')
    local br_octets eth0_octets
    br_octets=$(kubectl exec "$SG_NAME" -n "$NS" -c launcher -- \
        ip -4 addr show br0 2>/dev/null | awk '/inet /{print $2}' | head -1 | cut -d. -f1-3)
    eth0_octets=$(echo "$eth0_ip" | cut -d. -f1-3)
    echo "  br0 /24 prefix:  $br_octets.0/24"
    echo "  eth0 /24 prefix: $eth0_octets.0/24"
    if [ "$br_octets" = "$eth0_octets" ]; then
        echo "ERROR: br0 and eth0 share the same /24 — B0-shape collision"
        exit 1
    fi
    echo "  PASS — distinct /24 prefixes"
}

cross_node_tcp_probe() {
    echo "==> cross-node TCP: launcher pod on $SOURCE_NODE listens; probe pod on $PROBE_NODE connects"
    local launcher_ip
    launcher_ip=$(kubectl get pod "$SG_NAME" -n "$NS" -o jsonpath='{.status.podIP}')
    echo "  launcher pod IP: $launcher_ip"
    echo "  starting socat listener on launcher pod port $PORT..."
    # Listener notes:
    #   - `-u` makes socat unidirectional (no bidirectional stdio EOF
    #     propagation that would close the LISTEN side immediately).
    #   - `,fork` lets the listener serve N probes and stay up; the
    #     parent socat survives the connection and we tear it down
    #     explicitly at the end of this function.
    #   - `nohup ... </dev/null &` inside the container plus
    #     backgrounding the kubectl exec on the host releases the
    #     kubectl API channel; otherwise kubectl keeps the channel
    #     open while any in-container FD-inheritor lives.
    kubectl exec "$SG_NAME" -n "$NS" -c launcher -- \
        sh -c "nohup socat -u TCP-LISTEN:$PORT,reuseaddr,fork SYSTEM:'echo b0-probe-ack' >/tmp/socat-b0.log 2>&1 </dev/null &" \
        >/dev/null 2>&1 &
    local exec_pid=$!
    sleep 2
    # Confirm the listener actually bound; if it didn't, fail fast
    # rather than waste 30s on a probe that can't possibly connect.
    if ! kubectl exec "$SG_NAME" -n "$NS" -c launcher -- \
        sh -c "ss -tln 2>/dev/null | grep -q :$PORT"; then
        echo "ERROR: socat listener did not bind on port $PORT"
        kubectl exec "$SG_NAME" -n "$NS" -c launcher -- cat /tmp/socat-b0.log 2>&1 | tail -5
        kill "$exec_pid" 2>/dev/null || true
        exit 1
    fi
    echo "  socat listener bound on :$PORT"
    echo "  applying probe pod on $PROBE_NODE..."
    cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: $PROBE_POD
  namespace: $NS
spec:
  nodeName: $PROBE_NODE
  restartPolicy: Never
  containers:
  - name: probe
    image: busybox
    command: ["sh","-c","nc -w 5 -v $launcher_ip $PORT"]
EOF
    # Wait for the probe pod to terminate (success) or timeout
    local rc=1
    local deadline=$(( $(date +%s) + 30 ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        local phase
        phase=$(kubectl get pod "$PROBE_POD" -n "$NS" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
        case "$phase" in
            Succeeded) echo "  probe pod Succeeded — TCP connection established"; rc=0; break ;;
            Failed) echo "ERROR: probe pod Failed"
                    kubectl logs "$PROBE_POD" -n "$NS" 2>&1 | tail -5
                    break ;;
        esac
        sleep 1
    done
    if [ "$rc" -ne 0 ] && [ -z "${phase:-}" ]; then
        echo "ERROR: probe pod did not terminate within 30s"
        kubectl describe pod "$PROBE_POD" -n "$NS" 2>&1 | tail -20
    fi
    # Tear down the listener inside the launcher pod (best-effort).
    kubectl exec "$SG_NAME" -n "$NS" -c launcher -- \
        sh -c "pkill -f 'socat.*TCP-LISTEN:$PORT' 2>/dev/null; true" >/dev/null 2>&1 || true
    kill "$exec_pid" 2>/dev/null || true
    [ "$rc" -eq 0 ] || exit 1
}

cleanup() {
    kubectl delete swiftguest "$SG_NAME" -n "$NS" --wait=false 2>/dev/null || true
    kubectl delete pod "$PROBE_POD" -n "$NS" --grace-period=0 --force 2>/dev/null || true
    echo "  cleaned up: SwiftGuest $SG_NAME, Pod $PROBE_POD"
}

case "$cmd" in
    validate)
        ensure_two_kernel_nodes
        ensure_prereqs
        cleanup >/dev/null 2>&1 || true
        sleep 2
        echo "==> applying SwiftGuest $SG_NAME on $SOURCE_NODE..."
        apply_swiftguest
        wait_swiftguest_running
        echo "  SwiftGuest Running"
        check_static_subnet
        check_no_collision_with_eth0
        cross_node_tcp_probe
        echo
        echo "==> ALL CHECKS PASSED"
        ;;
    cleanup)
        cleanup
        ;;
    *)
        echo "usage: $0 {validate|cleanup}"
        exit 1
        ;;
esac
