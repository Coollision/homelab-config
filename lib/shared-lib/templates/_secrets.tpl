{{- define "shared-lib.secrets" }}
{{- $root := . }}
{{- range $secret := .Values.secrets }}
apiVersion: v1
kind: Secret
metadata:
  name: {{ $secret.name }}
  namespace: {{ $root.Release.Namespace }}
  labels:
    {{- include "helm.labels" $root | nindent 4 }}
type: Opaque
data:
  {{- range $key, $value := $secret.secret_kv }}
  {{ $key }}: {{ $value | b64enc | quote }}
  {{- end }}
---
{{- end }}
{{- end }}
