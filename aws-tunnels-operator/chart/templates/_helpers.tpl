{{- define "aws-tunnels-operator.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "aws-tunnels-operator.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- include "aws-tunnels-operator.name" . -}}
{{- end -}}
{{- end -}}

{{- define "aws-tunnels-operator.namespace" -}}
{{- default .Release.Namespace .Values.namespaceOverride -}}
{{- end -}}

{{- define "aws-tunnels-operator.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "aws-tunnels-operator.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- required "serviceAccount.name is required when serviceAccount.create=false" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}
