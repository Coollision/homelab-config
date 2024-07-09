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
{{- if .Values.deployment }}
{{ include "shared-lib.deployment" . }}
{{ else if .Values.statefullset }}
{{ include "shared-lib.statefullset" . }}
{{ else}}
{{- "need to have a deployment or statefullset" | fail -}}
{{- end}}
---
{{ include "shared-lib.service" . }}
---
{{- if .Values.ingress_internal }}
{{ include "shared-lib.ingress_internal" . }}
---
{{- end }}
{{- if .Values.ingress_internal_secure }}
{{ include "shared-lib.ingress_internal_secure" . }}
---
{{- end }}
{{- if .Values.ingress_external_secure }}
{{ include "shared-lib.ingress_external_secure" . }}
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
{{- if .Values.imagePreSync -}}
{{ include "shared-lib.imagePreSync" . }}
---
{{- end -}}
{{ end }}