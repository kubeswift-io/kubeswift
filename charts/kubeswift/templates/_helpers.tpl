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
