#!/usr/bin/env bash
# The sandbox bridge (rootfs-overlay/init) runs under `set -u`, so a variable
# the kernel command line may leave unset must get a default first: a missing
# one panics PID 1 on every boot without that argument (it happened twice,
# WORKDIR and EGRESS). Checks every variable the command-line loop assigns.
set -euo pipefail
init="$(dirname "$0")/rootfs-overlay/init"
loop=$(grep -n 'for arg in $(cat /proc/cmdline)' "$init" | head -1 | cut -d: -f1)
[ -n "$loop" ] || { echo "verify-init-defaults: no command-line loop in $init" >&2; exit 1; }
rc=0
for v in $(sed -n "${loop},/^done/p" "$init" | grep -oE '\) *[A-Z_]+="\$\{arg#' | grep -oE '[A-Z_]+' | sort -u); do
	if ! head -n "$loop" "$init" | grep -qE "^$v="; then
		echo "verify-init-defaults: $v is set from the command line but has no default before it" >&2
		rc=1
	fi
done
[ $rc = 0 ] && echo "verify-init-defaults: every command-line variable has a default"
exit $rc
