#!/bin/sh
# Throwaway helper: drive Cloud Hypervisor HTTP API for a spike SwiftGuest.
# AF_UNIX paths are limited to 108 chars on Linux, and the kubelet emptyDir
# path is too long, so we symlink to /tmp/<guest>.sock first.
#
# Usage (inside ch-debug-* pod):
#   ch-api.sh <pod-uid> <guest-name> <method> <endpoint> [body-json]
#
# Body goes to stdout (clean — no trailers). HTTP=<code> goes to stderr.
# Non-2xx returns a non-zero exit.
set -eu
pod_uid="$1"
guest="$2"
method="$3"
endpoint="$4"
body="${5:-}"
real_sock="/host/kubelet-pods/${pod_uid}/volumes/kubernetes.io~empty-dir/run/default-${guest}/ch.sock"
short_sock="/tmp/${guest}.sock"
if [ ! -S "$real_sock" ]; then
  echo "ch.sock not found at $real_sock" >&2
  exit 1
fi
ln -sf "$real_sock" "$short_sock"
body_file="/tmp/ch-api-body.$$"
if [ -n "$body" ]; then
  status=$(curl --silent --unix-socket "$short_sock" -X "$method" \
       -H 'Content-Type: application/json' -d "$body" \
       "http://localhost${endpoint}" -o "$body_file" -w '%{http_code}')
else
  status=$(curl --silent --unix-socket "$short_sock" -X "$method" \
       "http://localhost${endpoint}" -o "$body_file" -w '%{http_code}')
fi
[ -s "$body_file" ] && cat "$body_file"
rm -f "$body_file"
echo >&2 "HTTP=$status"
case "$status" in
  2*) exit 0 ;;
  *)  exit 1 ;;
esac
