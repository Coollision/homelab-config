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
{{- $affinity := .Values.affinity | default dict -}}
{{- $nodeAffinity := $affinity.node | default dict -}}
{{- $podAffinity := $affinity.pod | default dict -}}
{{- $podAntiAffinity := $affinity.podAnti | default dict -}}
{{- $autoNodeMatchExpressions := list -}}
{{- if and .Values.multusNetworks (ne .Values.multusAutoNodeAffinity false) -}}
  {{- range $network := .Values.multusNetworks -}}
    {{- $labelKey := $network.nodeLabelKey | default "" -}}
    {{- if not $labelKey -}}
      {{- if hasPrefix "vlan" ($network.parentInterface | default "") -}}
        {{- $labelKey = printf "%s-trunk" $network.parentInterface -}}
      {{- else -}}
        {{- $vlanFromName := regexFind "vlan[0-9]+" ($network.name | default "") -}}
        {{- if $vlanFromName -}}
          {{- $labelKey = printf "%s-trunk" $vlanFromName -}}
        {{- end -}}
      {{- end -}}
    {{- end -}}
    {{- if $labelKey -}}
      {{- $autoNodeMatchExpressions = append $autoNodeMatchExpressions (dict "key" $labelKey "operator" "In" "values" (list "true")) -}}
    {{- end -}}
  {{- end -}}
{{- end -}}

{{- if or $affinity (gt (len $autoNodeMatchExpressions) 0) }}
affinity:
  {{- if or $nodeAffinity (gt (len $autoNodeMatchExpressions) 0) }}
  nodeAffinity:
    {{- if or $nodeAffinity.required (gt (len $autoNodeMatchExpressions) 0) }}
    requiredDuringSchedulingIgnoredDuringExecution:
      nodeSelectorTerms:
        {{- if $nodeAffinity.required }}
        {{- range $nodeAffinity.required }}
        {{- $termExpressions := concat (.matchExpressions | default (list)) $autoNodeMatchExpressions }}
        - matchExpressions:
            {{- toYaml $termExpressions | nindent 12 }}
        {{- end }}
        {{- else }}
        - matchExpressions:
            {{- toYaml $autoNodeMatchExpressions | nindent 12 }}
        {{- end }}
    {{- end }}
    {{- if $nodeAffinity.preferred }}
    preferredDuringSchedulingIgnoredDuringExecution:
      {{- range $nodeAffinity.preferred }}
      - weight: {{ .weight }}
        preference:
          matchExpressions:
            {{- toYaml .matchExpressions | nindent 12 }}
      {{- end }}
    {{- end }}
  {{- end }}
  {{- if $podAffinity }}
  podAffinity:
    {{- if $podAffinity.required }}
    requiredDuringSchedulingIgnoredDuringExecution:
      {{- toYaml $podAffinity.required | nindent 6 }}
    {{- end }}
    {{- if $podAffinity.preferred }}
    preferredDuringSchedulingIgnoredDuringExecution:
      {{- toYaml $podAffinity.preferred | nindent 6 }}
    {{- end }}
  {{- end }}
  {{- if $podAntiAffinity }}
  podAntiAffinity:
    {{- if $podAntiAffinity.required }}
    requiredDuringSchedulingIgnoredDuringExecution:
      {{- toYaml $podAntiAffinity.required | nindent 6 }}
    {{- end }}
    {{- if $podAntiAffinity.preferred }}
    preferredDuringSchedulingIgnoredDuringExecution:
      {{- toYaml $podAntiAffinity.preferred | nindent 6 }}
    {{- end }}
  {{- end }}
{{- end }}
{{- end }}

