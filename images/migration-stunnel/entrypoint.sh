#!/bin/sh
# KubeSwift live-migration mTLS stunnel sidecar entrypoint (Phase 3c).
#
# Selects the server-vs-client stunnel config by STUNNEL_ROLE and injects the
# per-migration parameters (peer IP, peer SAN) from env. Role and peer are
# env-parameterized, NEVER image-baked - W-3c-2 load-bearing property: the
# controller DeepCopies the source pod's sidecar onto the destination and
# flips STUNNEL_ROLE rather than maintaining two images.
#
# Env contract (stamped by the SwiftMigration controller in PR 3):
#   STUNNEL_ROLE   server | client          (required)
#   CHECK_HOST     expected peer node SAN    (required, both roles)
#   DST_POD_IP     destination pod IP        (required, client role only)
#
# The mounted ConfigMap (STUNNEL_CONFIG_DIR) carries server.conf + client.conf
# with __CHECK_HOST__ / __DST_POD_IP__ placeholders; this script renders the
# active config into a writable runtime path and execs stunnel on it.
#
# Pure ASCII; explicit interpreter; POSIX sh (BusyBox-compatible).
set -eu

CONFIG_DIR="${STUNNEL_CONFIG_DIR:-/etc/stunnel-config}"
RUNTIME_DIR="${STUNNEL_RUNTIME_DIR:-/tmp}"
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

# CHECK_HOST (expected peer node SAN) is required for BOTH roles. It is the
# subject-check that closes the W-3c-4 gap: verifyChain alone proves "chains
# to the migration CA"; checkHost pins the peer to the specific node the
# controller chose from the SwiftMigration CR. Fail fast if unset rather than
# render a config that would accept any CA-signed peer.
if [ -z "${CHECK_HOST:-}" ]; then
  echo "stunnel-sidecar: CHECK_HOST (expected peer node SAN) is required" >&2
  exit 1
fi

# DST_POD_IP is the TLS connect target; required only for the client (source)
# sidecar. It is known only after the destination pod is scheduled, so the
# controller stamps it during Preparing-live (W-3c-2 sequencing).
if [ "${ROLE}" = "client" ] && [ -z "${DST_POD_IP:-}" ]; then
  echo "stunnel-sidecar: DST_POD_IP is required for client role" >&2
  exit 1
fi

ACTIVE_CONF="${RUNTIME_DIR}/stunnel.conf"
# The mounted ConfigMap is read-only; render the substituted config into a
# writable runtime path.
cp "${SRC_CONF}" "${ACTIVE_CONF}"
sed -i "s|__CHECK_HOST__|${CHECK_HOST}|g" "${ACTIVE_CONF}"
if [ "${ROLE}" = "client" ]; then
  sed -i "s|__DST_POD_IP__|${DST_POD_IP}|g" "${ACTIVE_CONF}"
fi

echo "stunnel-sidecar: starting role=${ROLE} peer-SAN=${CHECK_HOST}${DST_POD_IP:+ dst-ip=${DST_POD_IP}}" >&2
exec stunnel "${ACTIVE_CONF}"
