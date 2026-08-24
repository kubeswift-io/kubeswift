{{/*
Default image tag: matches what CI publishes.
- Dev (0.0.0-dev.<sha>): sha-<sha>
- RC/Stable (X.Y.Z): vX.Y.Z
Override with controllerManager.image.tag / swiftletd.image.tag when using local builds (e.g. latest).

appVersion is expected BARE (X.Y.Z). The release workflows pass it that way
(`--app-version "${TAG#v}"`), but Chart.yaml is hand-edited at release time and
once carried "v0.13.11", which this helper then turned into the tag `vv0.13.11`:
a nonexistent image for the controller AND for the five launcher images it
passes on by env var. Released charts were unaffected (the workflow flag wins),
so only `helm install`/`helm template` from a checkout broke — quietly, and only
at pod-pull time. trimPrefix makes the leading "v" unspellable rather than
merely currently-absent; hack/verify-image-tags.sh fails the build if a rendered
tag is malformed anyway.
*/}}
{{- define "kubeswift.imageTag" -}}
{{- $tag := .tag | default "latest" -}}
{{- if ne $tag "latest" -}}
{{- $tag -}}
{{- else -}}
{{- $app := trimPrefix "v" .appVersion -}}
{{- if hasPrefix "0.0.0-dev." $app -}}
{{- printf "sha-%s" (trimPrefix "0.0.0-dev." $app) -}}
{{- else -}}
{{- printf "v%s" $app -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
kubeswift.role — the effective federation role: federation.role, defaulting to
"standalone". standalone = today (no federation); hub = management plane
(gateway + UI + self-registration); edge = a federated member (onboarding).
*/}}
{{- define "kubeswift.role" -}}
{{- default "standalone" .Values.federation.role -}}
{{- end -}}

{{/*
kubeswift.gatewayEnabled / kubeswift.uiEnabled — "true" when the component runs.
role=hub PRESETS both on; an explicit gateway.enabled / ui.enabled adds them in
any role. (A hub always carries its own gateway+UI; to run these standalone,
leave role=standalone and set the toggles.) Emits "true" or "".
*/}}
{{- define "kubeswift.gatewayEnabled" -}}
{{- if or .Values.gateway.enabled (eq (include "kubeswift.role" .) "hub") -}}true{{- end -}}
{{- end -}}
{{- define "kubeswift.uiEnabled" -}}
{{- if or .Values.ui.enabled (eq (include "kubeswift.role" .) "hub") -}}true{{- end -}}
{{- end -}}

{{/*
kubeswift.selfRegisterEnabled — "true" when the chart should self-register this
cluster as a local fleet member: role=hub with federation.selfRegister.enabled
(default true for a hub). Emits "true" or "".
*/}}
{{- define "kubeswift.selfRegisterEnabled" -}}
{{- if and (eq (include "kubeswift.role" .) "hub") (ne (toString (dig "selfRegister" "enabled" true .Values.federation)) "false") -}}true{{- end -}}
{{- end -}}

{{/*
kubeswift.edgeRBAC — "true" when the chart should apply edge onboarding on this
cluster: role=edge with federation.edge.applyMemberRBAC (default true). Emits
"true" or "".
*/}}
{{- define "kubeswift.edgeRBAC" -}}
{{- if and (eq (include "kubeswift.role" .) "edge") (ne (toString (dig "edge" "applyMemberRBAC" true .Values.federation)) "false") -}}true{{- end -}}
{{- end -}}

{{/*
kubeswift.ingress.annotations — the merged annotation map for an Ingress: the
raw .annotations, plus (when .tlsAuto.enabled) the cert-manager issuer
annotation. Input: an ingress config dict (e.g. .Values.ui.ingress). Returns
YAML for a map; the caller does `include ... | fromYaml` and guards emptiness so
the `annotations:` key is omitted entirely when the map is empty.
*/}}
{{- define "kubeswift.ingress.annotations" -}}
{{- $ing := . -}}
{{- $ann := deepCopy (default (dict) $ing.annotations) -}}
{{- $auto := default (dict) $ing.tlsAuto -}}
{{- if $auto.enabled -}}
  {{- if and $auto.clusterIssuer $auto.issuer -}}
    {{- fail "ingress.tlsAuto: set only one of clusterIssuer or issuer, not both" -}}
  {{- else if $auto.clusterIssuer -}}
    {{- $_ := set $ann "cert-manager.io/cluster-issuer" $auto.clusterIssuer -}}
  {{- else if $auto.issuer -}}
    {{- $_ := set $ann "cert-manager.io/issuer" $auto.issuer -}}
  {{- else -}}
    {{- fail "ingress.tlsAuto.enabled=true requires clusterIssuer or issuer" -}}
  {{- end -}}
{{- end -}}
{{- toYaml $ann -}}
{{- end -}}

{{/*
kubeswift.ingress.tls — the tls[] list for an Ingress: derived from .tlsAuto
when enabled (one host, cert-manager Secret named "<host>-tls" unless overridden),
else the raw .tls escape-hatch list. Input: an ingress config dict. Returns YAML
list items (empty string when neither is set). tlsAuto wins over a raw tls[].
*/}}
{{- define "kubeswift.ingress.tls" -}}
{{- $ing := . -}}
{{- $auto := default (dict) $ing.tlsAuto -}}
{{- if $auto.enabled -}}
- secretName: {{ default (printf "%s-tls" $ing.host) $auto.secretName }}
  hosts:
    - {{ $ing.host | quote }}
{{- else -}}
{{- with $ing.tls -}}
{{- toYaml . -}}
{{- end -}}
{{- end -}}
{{- end -}}
