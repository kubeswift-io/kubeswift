#!/usr/bin/env bash
# Tests the launcher entrypoint's dnsmasq_start / dnsmasq_check helpers with a
# real dnsmasq in a rootless network namespace (unshare -rn): a dnsmasq that
# starts passes; one that exits at start fails the launcher (exit 1) with
# dnsmasq's own error as the termination message. Needs dnsmasq, pgrep, awk
# and unprivileged user namespaces.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
sed -n '/# BEGIN dnsmasq helpers/,/# END dnsmasq helpers/p' "$here/launcher-entrypoint.sh" > "$work/helpers.sh"
grep -q dnsmasq_check "$work/helpers.sh" || { echo "FAIL: helpers not found"; exit 1; }

# case NAME WANT-EXIT EXTRA-DNSMASQ-ARGS: runs the helpers as the entrypoint
# does, with the termination log redirected into the work dir.
case_() {
	local name=$1 want=$2; shift 2
	local dir="$work/$name"
	mkdir -p "$dir"
	set +e
	unshare -rn sh -s "$dir" "$@" > "$dir/out" 2>&1 <<SCRIPT
set -e
lease_dir=\$1; shift
sec_env=/nonexistent
ip link add br0 type bridge && ip addr add 192.168.99.1/24 dev br0 && ip link set br0 up
$(sed 's#/dev/termination-log#"$lease_dir/termination-log"#' "$work/helpers.sh")
dnsmasq_start "\$lease_dir/dnsmasq.leases" --no-daemon --bind-interfaces --interface=br0 --port=0 \
	--dhcp-range=192.168.99.10,192.168.99.10 --pid-file= "\$@" &
sleep 1
dnsmasq_check
echo CHECK-PASSED
pkill -f "dhcp-leasefile=\$lease_dir/dnsmasq.leases" || true
SCRIPT
	local got=$?
	set -e
	if [ "$got" != "$want" ]; then
		echo "FAIL $name: exit $got, want $want"; cat "$dir/out"; exit 1
	fi
	echo "ok   $name (exit $got)"
}

case_ running 0
grep -q CHECK-PASSED "$work/running/out"
grep -q 'started, version' "$work/running/dnsmasq.leases.start" || { echo "FAIL running: log not kept"; exit 1; }

case_ dies 1 --no-such-option
msg=$(cat "$work/dies/termination-log")
case "$msg" in
*"primary NIC's dnsmasq exited at start, so the guest would get no address: dnsmasq: bad command line options"*) echo "ok   dies: termination message: $msg" ;;
*) echo "FAIL dies: termination message: $msg"; cat "$work/dies/out"; exit 1 ;;
esac
echo PASS
