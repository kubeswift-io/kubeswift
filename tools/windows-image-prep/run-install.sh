#!/bin/bash
# KubeSwift Windows image-prep: drive a fully unattended, HEADLESS Windows Server
# install under QEMU/KVM and produce a virtio-ready qcow2 (see ./README.md and
# docs/windows/image-prep.md). Validated in the boot spike against Windows Server
# 2022 eval + virtio-win stable; the produced image boots on Cloud Hypervisor v52.
#
# Requirements (host): qemu-system-x86_64 with VNC + slirp + std-VGA (the distro
# build, e.g. Debian/Ubuntu /usr/bin/qemu-system-x86_64 — NOT a stripped
# kata-static build), OVMF (4M), genisoimage, python3, KVM.
#
# Usage:
#   WIN_ISO=Windows2022.iso VIRTIO_ISO=virtio-win.iso ./run-install.sh
# Env (all optional except the ISOs):
#   WIN_ISO       path to the Windows installation ISO          (required)
#   VIRTIO_ISO    path to virtio-win.iso                         (required)
#   OUT_QCOW2     output image path                  (default: ./windows.qcow2)
#   DISK_GB       virtual disk size in GiB            (default: 40)
#   OVMF_CODE     OVMF firmware code                  (default: /usr/share/OVMF/OVMF_CODE_4M.fd)
#   OVMF_VARS     OVMF vars template                  (default: /usr/share/OVMF/OVMF_VARS_4M.fd)
#   QEMU          qemu binary                         (default: /usr/bin/qemu-system-x86_64)
#   MEM_MB / CPUS install-time guest memory / vCPUs    (default: 6144 / 4)
set -u
cd "$(dirname "$0")"

WIN_ISO="${WIN_ISO:?set WIN_ISO to the Windows installation ISO}"
VIRTIO_ISO="${VIRTIO_ISO:?set VIRTIO_ISO to virtio-win.iso}"
OUT_QCOW2="${OUT_QCOW2:-./windows.qcow2}"
DISK_GB="${DISK_GB:-40}"
OVMF_CODE="${OVMF_CODE:-/usr/share/OVMF/OVMF_CODE_4M.fd}"
OVMF_VARS="${OVMF_VARS:-/usr/share/OVMF/OVMF_VARS_4M.fd}"
QEMU="${QEMU:-/usr/bin/qemu-system-x86_64}"
MEM_MB="${MEM_MB:-6144}"
CPUS="${CPUS:-4}"
work="$(mktemp -d)"; trap 'rm -rf "$work"' EXIT
serial="$work/serial.log"; qmp="$work/qmp.sock"; vars="$work/OVMF_VARS.fd"

# Build the autounattend seed ISO (autounattend.xml at the volume root).
au_dir="$work/au"; mkdir -p "$au_dir"; cp ./autounattend.xml "$au_dir/"
genisoimage -quiet -J -r -V KSWAUTO -o "$work/autounattend.iso" "$au_dir"

qemu-img create -f qcow2 "$OUT_QCOW2" "${DISK_GB}G" >/dev/null
cp "$OVMF_VARS" "$vars"
echo "=== [$(date -u +%H:%M:%S)] launching unattended install -> $OUT_QCOW2 (headless) ==="
"$QEMU" \
  -name kubeswift-winprep -machine q35,accel=kvm -cpu host -smp "$CPUS" -m "$MEM_MB" \
  -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
  -drive if=pflash,format=raw,file="$vars" \
  -drive file="$OUT_QCOW2",if=virtio,format=qcow2,cache=writeback,discard=unmap \
  -drive file="$WIN_ISO",media=cdrom,readonly=on \
  -drive file="$VIRTIO_ISO",media=cdrom,readonly=on \
  -drive file="$work/autounattend.iso",media=cdrom,readonly=on \
  -netdev user,id=n0,hostfwd=tcp::13389-:3389 -device virtio-net-pci,netdev=n0 \
  -serial file:"$serial" -qmp unix:"$qmp",server,nowait \
  -display none -vnc 127.0.0.1:9 -boot menu=off &
qpid=$!
echo "qemu pid $qpid (VNC 127.0.0.1:9 for inspection; RDP forwarded to host :13389)"

# Defeat the UEFI "Press any key to boot from CD" prompt headlessly: spam SPACE
# via QMP send-key for ~30s (the prompt appears in the first ~15s).
python3 - "$qmp" <<'PY' &
import socket,time,sys
sock=sys.argv[1]; s=None
for _ in range(30):
    if s is None:
        try:
            s=socket.socket(socket.AF_UNIX); s.settimeout(2); s.connect(sock)
            s.recv(65536); s.sendall(b'{"execute":"qmp_capabilities"}\n'); time.sleep(0.2); s.recv(65536)
        except Exception:
            s=None
    if s is not None:
        try:
            s.sendall(b'{"execute":"send-key","arguments":{"keys":[{"type":"qcode","data":"spc"}]}}\n')
            time.sleep(0.1); s.recv(65536)
        except Exception:
            try: s.close()
            except Exception: pass
            s=None
    time.sleep(1)
try:
    if s: s.close()
except Exception: pass
PY
keypid=$!

# The autounattend writes KUBESWIFT_PREP_OK to COM1 then `shutdown /s`. Detect
# either the sentinel or a clean QEMU exit. Cap 90 min.
res="TIMEOUT"
for i in $(seq 1 360); do
  if ! kill -0 "$qpid" 2>/dev/null; then res="EXITED"; echo "[$(date -u +%H:%M:%S)] qemu exited at t=$((i*15))s"; break; fi
  if grep -q KUBESWIFT_PREP_OK "$serial" 2>/dev/null; then res="SENTINEL"; echo "[$(date -u +%H:%M:%S)] completion sentinel at t=$((i*15))s"; break; fi
  if [ $((i % 8)) -eq 0 ]; then
    asz=$(qemu-img info --output=json "$OUT_QCOW2" 2>/dev/null | python3 -c "import sys,json;print(json.load(sys.stdin).get('actual-size',0))" 2>/dev/null)
    echo "[$(date -u +%H:%M:%S)] t=$((i*15))s installing... image=$(( ${asz:-0} /1024/1024 ))MB"
  fi
  sleep 15
done
kill "$keypid" 2>/dev/null

if [ "$res" = "SENTINEL" ]; then
  for i in $(seq 1 24); do kill -0 "$qpid" 2>/dev/null || { echo "qemu shut down cleanly"; break; }; sleep 5; done
fi
kill -0 "$qpid" 2>/dev/null && { echo "force-stopping qemu"; kill "$qpid"; sleep 3; kill -9 "$qpid" 2>/dev/null; }

asz=$(qemu-img info --output=json "$OUT_QCOW2" 2>/dev/null | python3 -c "import sys,json;print(json.load(sys.stdin).get('actual-size',0))" 2>/dev/null)
aszmb=$(( ${asz:-0} /1024/1024 ))
if [ "$res" = "SENTINEL" ] || { [ "$res" = "EXITED" ] && [ "$aszmb" -gt 3500 ]; }; then
  echo "INSTALL_RESULT=SUCCESS image=$OUT_QCOW2 size=${aszmb}MB"
  echo "Next: install cloudbase-init (runbook Part B), then host the image for a SwiftImage."
  exit 0
fi
echo "INSTALL_RESULT=FAIL reason=$res size=${aszmb}MB"
echo "----- serial tail (connect VNC 127.0.0.1:9 to inspect) -----"
tail -25 "$serial" 2>/dev/null
exit 1
