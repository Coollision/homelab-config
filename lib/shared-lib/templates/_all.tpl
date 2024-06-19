{{/*
Main entrypoint for the shared library chart. It will render all underlying templates based on the provided values.
*/}}

{{- define "shared-lib.var_dump" -}}
{{- . | mustToPrettyJson | printf "\nThe JSON output of the dumped var is: \n%s" | fail }}
{{- end -}}

{{- define "shared-lib.all" -}}
{{- if .Values.secrets }}
{{ include "shared-lib.secrets" . }}
{{- end }}
---
{{ include "shared-lib.deployment" . }}
---
{{ include "shared-lib.service" . }}
---
{{- if .Values.ingress_internal }}
{{ include "shared-lib.ingress_internal" . }}
---
{{- end }}
{{- if .Values.servicemonitor -}}
{{ include "shared-lib.servicemonitor" . }}
---
{{- end -}}
{{- if .Values.storage -}}
{{ include "shared-lib.storage" . }}
---
{{- end -}}
{{ end }}