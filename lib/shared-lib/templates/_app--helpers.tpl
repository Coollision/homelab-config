{{- define "shared-lib.image" -}}
  {{- $config := .Values.deployment | default .Values.statefullset -}}
  {{- $i := $config.image -}}
  {{- if $i.digest -}}
    {{- printf "%s@sha256:%s" $i.repository $i.digest -}}
  {{- else if contains ":" $i.repository -}}
    {{- printf "%s" $i.repository -}}
  {{- else -}}
    {{- printf "%s:%s" $i.repository (default "latest" $i.tag) -}}
  {{- end -}}
{{- end -}}

{{- define "shared-lib.imagePullPolicy" -}}
{{- $config := .Values.deployment | default .Values.statefullset -}}
{{- $i := $config.image -}}
{{- $i.imagePullPolicy | default "IfNotPresent" | quote -}}
{{- end -}}

{{- define "shared-lib.env" -}}
{{- if .Values.config }}
env:
  {{- range $key, $value := .Values.config }}
  - name: {{ $key }}
    {{- if not (kindIs "map" $value) }}
    value: {{ $value | quote }}
    {{- else if hasKey $value "secretKeyRef" }}
    valueFrom:
      secretKeyRef:
        name: {{ $value.secretKeyRef.name }}
        key: {{ $value.secretKeyRef.key }}
    {{- else if hasKey $value "configMapKeyRef" }}
    valueFrom:
      configMapKeyRef:
        name: {{ $value.configMapKeyRef.name }}
        key: {{ $value.configMapKeyRef.key }}
    {{- else }}
    # Invalid configuration for {{ $key }}
    value: "INVALID_CONFIGURATION"
    {{- end }}
  {{- end }}
{{- end }}
{{- end -}}

{{- define "shared-lib.getPorts" }}
{{- if .Values.deployment -}}
{{- .Values.deployment.ports | toYaml -}}
{{- else -}}
{{- .Values.statefullset.ports | toYaml -}}
{{- end -}}
{{- end }}


{{- define "shared-lib.securityContext" -}}
{{- if .Values.securityContext -}}
securityContext:
  {{- toYaml .Values.securityContext | nindent 2 -}}
{{- end -}}
{{- end -}}

{{- define "shared-lib.probes" -}}
{{- if .Values.probes -}}
{{- if .Values.probes.liveness -}}
livenessProbe:
  {{- toYaml .Values.probes.liveness| nindent 2 -}}
{{- end -}}
{{- if .Values.probes.readiness }}
readinessProbe:
  {{- toYaml .Values.probes.readiness | nindent 2 -}}
{{- end -}}
{{- end }}
{{- end -}}

{{- define "shared-lib.hostNetwork" -}}
{{- if .Values.hostNetwork -}}
hostNetwork: true
{{- end -}}
{{- end -}}

{{- define "shared-lib.dnsPolicy" -}}
{{- if .Values.dnsPolicy -}}
dnsPolicy: {{ .Values.dnsPolicy }}
{{- end -}}
{{- end -}}
