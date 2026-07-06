{{/*
Default image tag: matches what CI publishes.
- Dev (0.0.0-dev.<sha>): sha-<sha>
- RC/Stable (X.Y.Z): vX.Y.Z
Override with controllerManager.image.tag / swiftletd.image.tag when using local builds (e.g. latest).
*/}}
{{- define "kubeswift.imageTag" -}}
{{- $tag := .tag | default "latest" -}}
{{- if ne $tag "latest" -}}
{{- $tag -}}
{{- else -}}
{{- if hasPrefix "0.0.0-dev." .appVersion -}}
{{- printf "sha-%s" (trimPrefix "0.0.0-dev." .appVersion) -}}
{{- else -}}
{{- printf "v%s" .appVersion -}}
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
