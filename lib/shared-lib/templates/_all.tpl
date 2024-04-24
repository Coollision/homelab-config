{{/*
Main entrypoint for the shared library chart. It will render all underlying templates based on the provided values.
*/}}

{{- define "shared-lib.all" -}}
{{ include "shared-lib.service" . }}
---
{{ include "shared-lib.deployment" . }}
---
{{- if .Values.ingress_internal -}}
{{ include "shared-lib.ingress_internal" . }}
{{ end }}
---
{{- if .Values.serviceMonitor -}}
{{ include "shared-lib.servicemonitor" . }}
{{- end -}}
---
{{ end }}