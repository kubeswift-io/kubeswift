#!/bin/bash
# Module ABI guard for the gpu-sandbox kernel (WS1.5).
#
# The gpu-sandbox kernel loads out-of-tree modules (the NVIDIA driver from the
# guest image). A module works only with the kernel it was built against, so the
# kernel's release string carries a module ABI epoch (CONFIG_LOCALVERSION=
# "-ksabi<N>" in configs/gpu-sandbox.fragment) and every change to the inputs of
# that ABI must bump it. configs/gpu-sandbox.abi records one line per epoch:
#
#   <epoch> <sha256 of the normalised kernel config inputs>
#
# This script fails when the inputs changed but the last line does not match
# them, when the fragment's epoch is not the last line's, or when an epoch is
# reused. `--print` prints the line to append after a bump.
#
# Inputs: configs/sandbox-linux.config + configs/gpu-sandbox.fragment (minus the
# CONFIG_LOCALVERSION line itself), comments and blank lines dropped (but
# "# CONFIG_X is not set" kept), and the pinned Linux version.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ABI_FILE="$DIR/configs/gpu-sandbox.abi"
FRAGMENT="$DIR/configs/gpu-sandbox.fragment"

inputs_hash() {
	{
		cat "$DIR/configs/sandbox-linux.config" "$FRAGMENT" |
			grep -v '^CONFIG_LOCALVERSION=' |
			grep -E '^CONFIG_|^# CONFIG_[A-Z0-9_]+ is not set' |
			LC_ALL=C sort -u
		grep '^BR2_LINUX_KERNEL_CUSTOM_VERSION_VALUE=' "$DIR/configs/gpu-sandbox_defconfig"
	} | sha256sum | cut -d' ' -f1
}

epoch="$(sed -n 's/^CONFIG_LOCALVERSION="-\(ksabi[0-9]\+\)"$/\1/p' "$FRAGMENT")"
[ -n "$epoch" ] || { echo "verify-module-abi: ERROR: $FRAGMENT sets no CONFIG_LOCALVERSION=\"-ksabi<N>\"" >&2; exit 1; }
hash="$(inputs_hash)"

if [ "${1:-}" = "--print" ]; then
	echo "$epoch $hash"
	exit 0
fi

[ -f "$ABI_FILE" ] || { echo "verify-module-abi: ERROR: $ABI_FILE missing" >&2; exit 1; }
last="$(grep -v '^#' "$ABI_FILE" | grep -v '^$' | tail -1)"
last_epoch="${last%% *}"
last_hash="${last##* }"
dups="$(grep -v '^#' "$ABI_FILE" | grep -v '^$' | cut -d' ' -f1 | sort | uniq -d)"
if [ -n "$dups" ]; then
	echo "verify-module-abi: ERROR: epoch reused in $ABI_FILE: $dups" >&2
	exit 1
fi
if [ "$hash" != "$last_hash" ]; then
	echo "verify-module-abi: ERROR: the gpu-sandbox kernel config changed but its module ABI epoch did not." >&2
	echo "  Modules built for the previous kernel would load and may crash the guest." >&2
	echo "  Bump CONFIG_LOCALVERSION in configs/gpu-sandbox.fragment (now -$epoch), then append" >&2
	echo "  the output of '$0 --print' to configs/gpu-sandbox.abi." >&2
	exit 1
fi
if [ "$epoch" != "$last_epoch" ]; then
	echo "verify-module-abi: ERROR: fragment epoch $epoch does not match the last line of $ABI_FILE ($last_epoch)" >&2
	exit 1
fi
echo "verify-module-abi: OK (gpu-sandbox module ABI $epoch)"
