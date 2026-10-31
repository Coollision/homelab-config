{{- define "storage-operator.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "storage-operator.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- include "storage-operator.name" . -}}
{{- end -}}
{{- end -}}

{{- define "storage-operator.namespace" -}}
{{- default .Release.Namespace .Values.namespaceOverride -}}
{{- end -}}

{{- define "storage-operator.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "storage-operator.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- required "serviceAccount.name is required when serviceAccount.create=false" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/* Selector: must stay "app: <fullname>" (immutable; the live smb-operator Deployment uses it). */}}
{{- define "storage-operator.selectorLabels" -}}
app: {{ include "storage-operator.fullname" . }}
{{- end -}}

{{- define "storage-operator.labels" -}}
app.kubernetes.io/name: {{ include "storage-operator.name" . }}
{{- end -}}
