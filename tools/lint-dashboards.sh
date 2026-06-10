#!/bin/bash
# lint-dashboards.sh — provisioning-native hygiene checks for the Grafana
# dashboards under config/grafana/.
#
# Dashboards ship to operators as sidecar-provisioned ConfigMaps (Helm
# monitoring.dashboards). Import-style JSON (__inputs blocks, ${DS_*}
# placeholder variables) renders fine on manual import but silently breaks
# under provisioning: the inputs are never bound, panels resolve no
# datasource, and every panel shows "no data" (observability design doc
# D6; confirmed live 2026-06-10). These checks keep the JSONs in the form
# that works in BOTH paths: a `datasource` templating variable defaulting
# to the instance default, referenced as ${datasource}.
#
# Rules:
#   1. valid JSON
#   2. no __inputs / __requires blocks
#   3. no ${DS_*} import-time placeholders
#   4. uid present and equal to the filename stem (stable cross-version)
#   5. tags include "kubeswift"; editable: true; title present
#   6. every ${datasource} reference is backed by a templating variable
#      named "datasource" of type "datasource" with a current value
set -euo pipefail

cd "$(dirname "$0")/.."

fail=0
for f in config/grafana/*.json; do
    python3 - "$f" <<'PY' || fail=1
import json
import os
import sys

path = sys.argv[1]
stem = os.path.splitext(os.path.basename(path))[0]
raw = open(path).read()

errs = []
try:
    d = json.loads(raw)
except ValueError as e:
    print(f"FAIL {path}: invalid JSON: {e}")
    sys.exit(1)

for key in ("__inputs", "__requires"):
    if key in d:
        errs.append(f"has import-style '{key}' block (breaks sidecar provisioning)")

if "${DS_" in raw:
    errs.append("references a ${DS_*} import placeholder (use ${datasource})")

if d.get("uid") != stem:
    errs.append(f"uid {d.get('uid')!r} != filename stem {stem!r}")
if "kubeswift" not in (d.get("tags") or []):
    errs.append("tags missing 'kubeswift'")
if d.get("editable") is not True:
    errs.append("editable must be true")
if not d.get("title"):
    errs.append("title missing")

if "${datasource}" in raw:
    tvars = {v.get("name"): v for v in d.get("templating", {}).get("list", [])}
    ds = tvars.get("datasource")
    if ds is None:
        errs.append("${datasource} referenced but no 'datasource' templating variable")
    elif ds.get("type") != "datasource":
        errs.append("'datasource' templating variable must have type 'datasource'")
    elif not (ds.get("current") or {}).get("value"):
        errs.append("'datasource' templating variable needs a current default value")

if errs:
    for e in errs:
        print(f"FAIL {path}: {e}")
    sys.exit(1)
print(f"  ok {path}")
PY
done

if [ "$fail" -ne 0 ]; then
    echo "dashboard lint FAILED"
    exit 1
fi
echo "  all dashboards provisioning-native"
