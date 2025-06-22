{{/*
Expand the name of the chart.
*/}}

{{- define "helm.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "helm.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "helm.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "helm.labels" -}}
helm.sh/chart: {{ include "helm.chart" . }}
{{ include "helm.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- if .Values.labels }}
{{- toYaml .Values.labels | nindent 0 }}
{{- end }}
{{- end }}

{{/*
{{- end }}

{{- end }}

{{/*

{{- end }}

{{/*
Selector labels
*/}}
{{- define "helm.selectorLabels" -}}
app.kubernetes.io/name: {{ include "helm.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}


{{- define "helm.var_dump" -}}
{{- . | mustToPrettyJson | printf "\nThe JSON output of the dumped var is: \n%s" }}
{{- end -}}

{{- define "helm.var_dump_fail" -}}
{{- . | mustToPrettyJson | printf "\nThe JSON output of the dumped var is: \n%s" | fail }}
{{- end -}}

{{/*
Affinity helper: supports required/preferred lists for nodeAffinity and podAffinity.
Example values:
affinity:
  nodeAffinity:
    required:
      - matchExpressions:
          - key: feature.node.kubernetes.io/iot-zigbee-coordinator
            operator: In
            values: ["true"]
    preferred:
      - weight: 50
        matchExpressions:
          - key: some-key
            operator: In
            values: ["foo"]
  podAffinity:
    preferredDuringSchedulingIgnoredDuringExecution:
      - weight: 100
        podAffinityTerm:
          labelSelector:
            matchExpressions:
              - key: app.kubernetes.io/component
                operator: In
                values: [homeassistant-db]
          topologyKey: kubernetes.io/hostname
*/}}
{{- define "shared-lib.affinity" -}}
{{- if .Values.affinity }}
affinity:
  {{- if .Values.affinity.node }}
  nodeAffinity:
    {{- if .Values.affinity.node.required }}
    requiredDuringSchedulingIgnoredDuringExecution:
      nodeSelectorTerms:
        {{- range .Values.affinity.node.required }}
        - matchExpressions:
            {{- toYaml .matchExpressions | nindent 12 }}
        {{- end }}
    {{- end }}
    {{- if .Values.affinity.node.preferred }}
    preferredDuringSchedulingIgnoredDuringExecution:
      {{- range .Values.affinity.node.preferred }}
      - weight: {{ .weight }}
        preference:
          matchExpressions:
            {{- toYaml .matchExpressions | nindent 12 }}
      {{- end }}
    {{- end }}
  {{- end }}
  {{- if .Values.affinity.pod }}
  podAffinity:
    {{- if .Values.affinity.pod.required }}
    requiredDuringSchedulingIgnoredDuringExecution:
      {{- toYaml .Values.affinity.pod.required | nindent 6 }}
    {{- end }}
    {{- if .Values.affinity.pod.preferred }}
    preferredDuringSchedulingIgnoredDuringExecution:
      {{- toYaml .Values.affinity.pod.preferred | nindent 6 }}
    {{- end }}
  {{- end }}
  {{- if .Values.affinity.podAnti }}
  podAntiAffinity:
    {{- if .Values.affinity.podAnti.required }}
    requiredDuringSchedulingIgnoredDuringExecution:
      {{- toYaml .Values.affinity.podAnti.required | nindent 6 }}
    {{- end }}
    {{- if .Values.affinity.podAnti.preferred }}
    preferredDuringSchedulingIgnoredDuringExecution:
      {{- toYaml .Values.affinity.podAnti.preferred | nindent 6 }}
    {{- end }}
  {{- end }}
{{- end }}
{{- end }}

