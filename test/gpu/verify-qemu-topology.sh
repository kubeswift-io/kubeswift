#!/usr/bin/env bash
# Boot-smoke the generated Tier-2/3 HGX QEMU topology WITHOUT GPU hardware.
#
# What it proves: qemu-system-x86_64 ACCEPTS AND REALIZES the exact machine the
# swift-qemu-client builder emits — per-device pcie-root-port hierarchy (unique
# chassis/slot), NUMA memory backends + bindings, SMP topology — by booting it
# with emulated PCIe endpoints (e1000e) substituted on the SAME root ports.
# What it does NOT prove: VFIO bind, BAR mapping, NVLink, Fabric Manager — those
# need real HGX hardware (docs/design/gpu-testing-without-hardware.md).
#
# Drift-proof: the args come from `cargo run --example hgx_tier2_args`, i.e. the
# real QemuConfig::to_args() output — never a hand-copied command line.
#
# Default runs against the SHIPPING QEMU inside the swiftletd image (docker,
# needs /dev/kvm). Set QEMU=/path/to/qemu-system-x86_64 to run host-local.
#
# Lab tool (needs KVM + docker or a local qemu); not wired into CI.
set -euo pipefail

REPO_ROOT="$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)"
IMAGE="${SWIFTLETD_IMAGE:-ghcr.io/kubeswift-io/kubeswift/swiftletd:v0.11.0}"
WORK="$(mktemp -d /tmp/hgx-topology-smoke.XXXXXX)"
trap 'rm -rf "$WORK"' EXIT

echo "==> generating topology args from the builder (cargo example)"
(cd "$REPO_ROOT/rust" && cargo run -q -p swift-qemu-client --example hgx_tier2_args -- \
  --dummy --run-dir /work) > "$WORK/args.txt"

mapfile -t ARGS < "$WORK/args.txt"
echo "    $(printf '%s ' "${ARGS[@]}" | head -c 400)..."

# Sanity: the load-bearing topology args must be present before we bother booting.
JOINED="$(printf '%s ' "${ARGS[@]}")"
for want in \
  "pcie-root-port,id=rp0,bus=pcie.0,chassis=1,slot=1" \
  "pcie-root-port,id=rp3,bus=pcie.0,chassis=4,slot=4" \
  "e1000e,bus=rp0" \
  "node,nodeid=1,cpus=2-3,memdev=ram1" \
  "4,sockets=2,cores=2,threads=1"; do
  if ! grep -qF -- "$want" <<<"$JOINED"; then
    echo "FAIL: expected arg missing: $want"
    exit 1
  fi
done
echo "==> topology args sane (4 root ports, NUMA bindings, SMP topology)"

# The QMP driver script that runs NEXT TO qemu (inside the container or host):
# boot frozen (-S: devices are realized before vCPUs start, so topology errors
# abort at init), then over QMP: negotiate, query-status (expect prelaunch),
# query-cpus-fast (SMP came up), quit.
cat > "$WORK/drive.sh" <<'EOF'
set -eu
cd /work
cp /usr/share/OVMF/OVMF_VARS.fd /work/OVMF_VARS.fd
mapfile -t ARGS < /work/args.txt
"${QEMU_BIN:-qemu-system-x86_64}" "${ARGS[@]}" -S &
QPID=$!
for i in $(seq 1 50); do [ -S /work/qmp.sock ] && break; sleep 0.2; done
[ -S /work/qmp.sock ] || { echo "FAIL: QMP socket never appeared"; kill $QPID 2>/dev/null; exit 1; }
OUT=$(printf '%s\n' \
  '{"execute":"qmp_capabilities"}' \
  '{"execute":"query-status"}' \
  '{"execute":"query-cpus-fast"}' \
  '{"execute":"quit"}' | socat -t 5 - UNIX-CONNECT:/work/qmp.sock)
echo "$OUT" > /work/qmp.out
wait $QPID 2>/dev/null || true
grep -q '"status": *"prelaunch"' /work/qmp.out || { echo "FAIL: not prelaunch"; cat /work/qmp.out; exit 1; }
CPUS=$(grep -o '"cpu-index"' /work/qmp.out | wc -l)
[ "$CPUS" -eq 4 ] || { echo "FAIL: expected 4 vCPUs, saw $CPUS"; cat /work/qmp.out; exit 1; }
echo "OK: machine realized (prelaunch), 4 vCPUs, topology accepted"
EOF
chmod +x "$WORK/drive.sh"

if [ -n "${QEMU:-}" ]; then
  echo "==> booting on host qemu: $QEMU"
  ( export QEMU_BIN="$QEMU"; cd "$WORK" && sed "s#/work#$WORK#g" drive.sh > drive.local.sh && bash drive.local.sh )
else
  echo "==> booting on the shipping QEMU in $IMAGE"
  docker run --rm --device /dev/kvm -v "$WORK:/work" --entrypoint bash "$IMAGE" /work/drive.sh
fi

echo "PASS: the generated Tier-2 HGX topology boots on QEMU (root ports + NUMA + SMP realized)"
