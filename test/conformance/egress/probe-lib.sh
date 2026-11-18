# Probe helpers the conformance guest sources (/conformance/probe-lib.sh).
# Each probe prints PROBE-OK or PROBE-FAIL; the workload exits with $f, the
# failure count. WANT is reach or block.
f=0
empty=$(printf '' | md5sum)
res() { if [ "$1" = "$2" ]; then echo "PROBE-OK   $3 $1"; else echo "PROBE-FAIL $3 got $1, want $2"; f=$((f+1)); fi; }
# dns NAME WANT HOST [SERVER]: an A lookup through the guest's resolver (or SERVER).
dns() { n=$1; w=$2; shift 2; if nslookup "$@" >/dev/null 2>&1; then g=reach; else g=block; fi; res $g $w "$n"; }
# dig NAME WANT ARGS...: reach when dig prints an answer.
digq() { n=$1; w=$2; shift 2; if [ -n "$(dig +time=3 +tries=1 +short "$@" 2>/dev/null | grep -v '^;')" ]; then g=reach; else g=block; fi; res $g $w "$n"; }
# http NAME WANT CURL-ARGS...: reach on any HTTP answer (add -f to need 2xx).
http() { n=$1; w=$2; shift 2; if curl -sS -m 8 -o /dev/null "$@" 2>/dev/null; then g=reach; else g=block; fi; res $g $w "$n"; }
# banner NAME WANT ADDR PORT PATTERN: reach when the server's greeting matches.
banner() { if timeout 8 nc "$3" "$4" </dev/null 2>/dev/null | head -c 256 | grep -q "$5"; then g=reach; else g=block; fi; res $g $2 "$1"; }
# nic NAME WANT: reach when the guest has a network device other than lo
# (read from /proc/net/dev, which must list lo for the answer to count).
nic() { d=$(awk -F: 'NR>2{gsub(/ /,"",$1); print $1}' /proc/net/dev 2>/dev/null)
  if ! printf '%s\n' "$d" | grep -qx lo; then echo "PROBE-FAIL $1 cannot read /proc/net/dev"; f=$((f+1)); return; fi
  if printf '%s\n' "$d" | grep -qvx lo; then g=reach; else g=block; fi; res $g $2 "$1"; }
# icmp NAME WANT ADDR [-6]
icmp() { if ping $4 -c 1 -W 3 "$3" >/dev/null 2>&1; then g=reach; else g=block; fi; res $g $2 "$1"; }
# ntp NAME WANT ADDR: one NTP exchange over UDP 123.
ntp() { if timeout 8 ntpd -n -q -p "$3" >/dev/null 2>&1; then g=reach; else g=block; fi; res $g $2 "$1"; }
# spoof NAME SCHEME HOST IP: a request for HOST forced to IP is either refused
# or answered by HOST's own address (same body), never by IP.
spoof() { p=443; [ "$2" = http ] && p=80
  a=$(curl -sfk -m 8 "$2://$3/" | md5sum); b=$(curl -sfk -m 8 --resolve "$3:$p:$4" "$2://$3/" | md5sum)
  if [ "$a" = "$empty" ]; then echo "PROBE-FAIL $1 no answer from $3 itself: the check proves nothing"; f=$((f+1))
  elif [ "$b" = "$empty" ]; then echo "PROBE-OK   $1 refused"
  elif [ "$a" = "$b" ]; then echo "PROBE-OK   $1 went to the name's address"
  else echo "PROBE-FAIL $1 reached another address"; f=$((f+1)); fi; }
