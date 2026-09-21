#!/usr/bin/env bash
# verify-chart-readme — prove charts/kubeswift/README.md's values tables still
# agree with charts/kubeswift/values.yaml.
#
# THE FAILURE MODE. The README documents every value with its default, and
# nothing checked that third column. v0.13.14 shipped a stale `ui.image.tag`
# default there, and it was caught by reading ~60 rows against values.yaml by
# hand during the release sweep — the kind of step that gets skipped on the
# release where it matters. Operators read this table to decide what to set, so
# a wrong default is wrong advice, not a typo.
#
# Values are read through helm rather than a YAML parser on purpose: every other
# hack/verify-*.sh needs only helm + jq, and this one should not be the reason
# CI grows a YAML dependency. A throwaway chart carrying just values.yaml and a
# `{{ .Values | toJson }}` template gives helm's own view of the defaults.
#
# Run via:
#   make verify-chart-readme
#   ./hack/verify-chart-readme.sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CHART="$ROOT/charts/kubeswift"
README="$CHART/README.md"

command -v helm >/dev/null || { echo "verify-chart-readme: helm not found" >&2; exit 1; }
command -v jq   >/dev/null || { echo "verify-chart-readme: jq not found" >&2; exit 1; }
[[ -f "$README" ]] || { echo "verify-chart-readme: $README is missing" >&2; exit 1; }

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

cp "$CHART/values.yaml" "$work/values.yaml"
printf 'apiVersion: v2\nname: valuesdump\nversion: 0.0.0\n' > "$work/Chart.yaml"
mkdir -p "$work/templates"
printf '%s\n' '{{ .Values | toJson }}' > "$work/templates/dump.yaml"
helm template dump "$work" | grep -vE '^(#|---|$)' > "$work/values.json"

# Rows are parsed only where the comparison is unambiguous. Two shapes are
# skipped deliberately, and both exist in this README today:
#
#   - The prerequisites table has TWO columns (key | requirement) and no
#     default at all. Requiring exactly three columns drops it wholesale.
#   - Rows whose first cell names a RELATIONSHIP rather than one key —
#     "`gpuDiscovery.enabled` / `dra.enabled`", "`launcherSAGate.enabled`
#     (**on by default**)". Requiring that cell to be exactly one backticked
#     key and nothing else drops those, with no exception list to maintain.
#
# A default cell may carry an explanation after the value, as in `""` (release
# name). The first backticked token is the value; the rest is prose.
#
# Key and default are emitted on SEPARATE lines rather than joined by a
# separator, so nothing has to be escaped and no character is forbidden in a
# default.
rows="$(awk -F'|' '
  /^\|/ {
    if (NF - 2 != 3) next
    key = $2; def = $4
    gsub(/^[[:space:]]+|[[:space:]]+$/, "", key)
    gsub(/^[[:space:]]+|[[:space:]]+$/, "", def)
    if (key !~ /^`[a-zA-Z][a-zA-Z0-9_]*(\.[a-zA-Z0-9_]+)*`$/) next
    gsub(/`/, "", key)
    if (def !~ /`/) next
    sub(/^[^`]*`/, "", def); sub(/`.*$/, "", def)
    print key
    print def
  }' "$README")"

[[ -n "$rows" ]] || { echo "verify-chart-readme: parsed no rows from $README — the table format changed" >&2; exit 1; }

fail=0
checked=0
while IFS= read -r key && IFS= read -r documented; do
  actual="$(jq -r --arg k "$key" '
    def get($ps): reduce $ps[] as $p (.;
      if type == "object" and has($p) then .[$p] else "__MISSING__" end);
    get($k | split(".")) |
    if   . == "__MISSING__" then "__MISSING__"
    elif type == "string"  then (if . == "" then "\"\"" else . end)
    elif type == "array"   then (if length == 0 then "[]" else "__SKIP__" end)
    elif type == "object"  then "__SKIP__"
    elif . == null         then "null"
    else tostring end' "$work/values.json")"

  case "$actual" in
    __MISSING__)
      echo "FAIL $key — documented in README.md but absent from values.yaml" >&2
      fail=1; continue ;;
    __SKIP__) continue ;;   # a map or non-empty list: not comparable inline
  esac

  checked=$((checked + 1))
  if [[ "$actual" != "$documented" ]]; then
    echo "FAIL $key — README says '$documented', values.yaml has '$actual'" >&2
    fail=1
  fi
done <<< "$rows"

if [[ $fail -ne 0 ]]; then
  echo "verify-chart-readme: $README disagrees with values.yaml (see above)" >&2
  exit 1
fi
echo "OK: $checked README defaults match charts/kubeswift/values.yaml"
