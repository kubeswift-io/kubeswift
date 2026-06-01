#!/bin/sh
# KubeSwift live-migration mTLS stunnel sidecar entrypoint (Phase 3c).
#
# Selects the server-vs-client stunnel config by STUNNEL_ROLE and renders
# the per-migration parameters (peer IP, peer SAN) into the active config.
# Role and peer are env/file-parameterized, NEVER image-baked - W-3c-2
# load-bearing property: the controller picks role + peer per migration
# rather than maintaining two images.
#
# Two input models, by role:
#
#   server (DESTINATION pod): created fresh by the SwiftMigration
#     controller at migration time, so CHECK_HOST and the identity Secret
#     are available at container start. Inputs come from ENV + the mounted
#     per-node identity Secret. Starts immediately.
#
#   client (SOURCE pod): lives inside the pre-existing, immutable launcher
#     pod, born long before any migration. Its inputs (peer IP, peer SAN,
#     and this guest's identity) are unknown at pod creation and arrive
#     only when a migration starts: the controller stamps pod annotations
#     (surfaced here via a downward-API volume) and populates the per-guest
#     identity Secret. So the client IDLE-POLLS for those inputs and only
#     then starts stunnel. Idling keeps the launcher pod Running/Ready at
#     rest (no migration in flight) instead of crash-looping.
#
# Env contract:
#   STUNNEL_ROLE        server | client                 (required)
#   CHECK_HOST          expected peer node SAN           (server: required)
#   STUNNEL_CONFIG_DIR  server.conf/client.conf dir      (default /etc/stunnel-config)
#   STUNNEL_INPUT_DIR   downward-API input dir (client)  (default /etc/migration-input)
#   STUNNEL_TLS_DIR     identity Secret mount            (default /etc/migration-tls)
#   STUNNEL_RUNTIME_DIR writable dir for rendered config (default /tmp)
#   STUNNEL_POLL_SECONDS client idle-poll interval       (default 2)
#
# Client inputs arrive as files (controller-stamped, downward-API + Secret):
#   ${STUNNEL_INPUT_DIR}/dst-ip    destination pod IP   (TLS connect target)
#   ${STUNNEL_INPUT_DIR}/peer-san  destination node SAN (checkHost pin)
#   ${STUNNEL_TLS_DIR}/tls.crt, tls.key, ca.crt          (this guest's identity)
#
# Pure ASCII; explicit interpreter; POSIX sh (BusyBox-compatible).
set -eu

CONFIG_DIR="${STUNNEL_CONFIG_DIR:-/etc/stunnel-config}"
RUNTIME_DIR="${STUNNEL_RUNTIME_DIR:-/tmp}"
INPUT_DIR="${STUNNEL_INPUT_DIR:-/etc/migration-input}"
TLS_DIR="${STUNNEL_TLS_DIR:-/etc/migration-tls}"
POLL_SECONDS="${STUNNEL_POLL_SECONDS:-2}"
ROLE="${STUNNEL_ROLE:-}"

case "${ROLE}" in
  server)
    SRC_CONF="${CONFIG_DIR}/server.conf"
    ;;
  client)
    SRC_CONF="${CONFIG_DIR}/client.conf"
    ;;
  *)
    echo "stunnel-sidecar: STUNNEL_ROLE must be 'server' or 'client' (got '${ROLE}')" >&2
    exit 1
    ;;
esac

if [ ! -f "${SRC_CONF}" ]; then
  echo "stunnel-sidecar: config not found: ${SRC_CONF} (is the ConfigMap mounted at ${CONFIG_DIR}?)" >&2
  exit 1
fi

# read_file prints a file's contents with trailing newlines stripped, or an
# empty string when the file is absent. Used to read controller-stamped
# downward-API inputs without tripping 'set -e' on a missing file.
read_file() {
  if [ -f "$1" ]; then
    tr -d '\n\r' < "$1"
  else
    printf ''
  fi
}

EFF_DST_POD_IP=""

if [ "${ROLE}" = "server" ]; then
  # Destination: CHECK_HOST (expected SOURCE node SAN) arrives by env at
  # container start; the identity Secret is mounted populated. The SAN pin
  # closes the W-3c-4 subject-check gap - fail fast rather than render a
  # config that would accept any CA-signed peer.
  if [ -z "${CHECK_HOST:-}" ]; then
    echo "stunnel-sidecar: CHECK_HOST (expected peer node SAN) is required for server role" >&2
    exit 1
  fi
  EFF_CHECK_HOST="${CHECK_HOST}"
else
  # Source: idle-poll for the controller-stamped inputs (downward-API
  # annotations) and the populated per-guest identity Secret. Stays in this
  # loop - container Running, pod Ready - until a migration starts.
  echo "stunnel-sidecar: client idle; waiting for migration inputs (dst-ip, peer-san, identity) under ${INPUT_DIR} + ${TLS_DIR}" >&2
  while : ; do
    EFF_CHECK_HOST="$(read_file "${INPUT_DIR}/peer-san")"
    EFF_DST_POD_IP="$(read_file "${INPUT_DIR}/dst-ip")"
    if [ -n "${EFF_CHECK_HOST}" ] && [ -n "${EFF_DST_POD_IP}" ] && \
       [ -s "${TLS_DIR}/tls.crt" ] && [ -s "${TLS_DIR}/tls.key" ] && [ -s "${TLS_DIR}/ca.crt" ]; then
      break
    fi
    sleep "${POLL_SECONDS}"
  done
  echo "stunnel-sidecar: inputs ready (peer-san=${EFF_CHECK_HOST} dst-ip=${EFF_DST_POD_IP}); starting" >&2
fi

ACTIVE_CONF="${RUNTIME_DIR}/stunnel.conf"
# The mounted ConfigMap is read-only; render the substituted config into a
# writable runtime path.
cp "${SRC_CONF}" "${ACTIVE_CONF}"
sed -i "s|__CHECK_HOST__|${EFF_CHECK_HOST}|g" "${ACTIVE_CONF}"
if [ "${ROLE}" = "client" ]; then
  sed -i "s|__DST_POD_IP__|${EFF_DST_POD_IP}|g" "${ACTIVE_CONF}"
fi

echo "stunnel-sidecar: starting role=${ROLE} peer-san=${EFF_CHECK_HOST}${EFF_DST_POD_IP:+ dst-ip=${EFF_DST_POD_IP}}" >&2
exec stunnel "${ACTIVE_CONF}"
