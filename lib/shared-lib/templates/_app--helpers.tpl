{{- define "shared-lib.image" -}}
{{- if contains ":" .repository -}}
{{- printf "%s" .repository -}}
{{- else -}}
{{- printf "%s:%s" .repository ( default "latest" .tag ) -}}
{{- end -}}
{{- end -}}

{{- define "shared-lib.env" -}}
{{- if .Values.config }}
env:
  {{- range $key, $value := .Values.config }}
  - name: {{ $key }}
    {{- if kindIs "string" $value }}
    value: {{ $value | quote }}
    {{- else }}
    valueFrom:
      secretKeyRef:
        name: {{ $value.secretKeyRef.name }}
        key: {{ $value.secretKeyRef.key }}
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
