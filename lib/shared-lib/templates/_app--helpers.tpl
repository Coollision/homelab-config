{{- define "shared-lib.image" -}}
  {{- $config := .Values.deployment | default .Values.statefulset -}}
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
{{- $config := .Values.deployment | default .Values.statefulset -}}
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
{{- .Values.statefulset.ports | toYaml -}}
{{- end -}}
{{- end }}

{{- /* A port value may be a scalar (the port number, protocol TCP) or a map
       {port: <n>, protocol: <TCP|UDP|...>}. These helpers normalise either form.
       Scalar input renders byte-identically to the previous hardcoded behaviour. */ -}}
{{- define "shared-lib.portNumber" -}}
{{- if kindIs "map" . }}{{ .port }}{{ else }}{{ . }}{{ end -}}
{{- end -}}

{{- define "shared-lib.portProtocol" -}}
{{- if kindIs "map" . }}{{ .protocol | default "TCP" }}{{ else }}TCP{{ end -}}
{{- end -}}


{{- define "shared-lib.podSecurityContext" -}}
{{- if .Values.podSecurityContext -}}
securityContext:
  {{- toYaml .Values.podSecurityContext | nindent 2 -}}
{{- end -}}
{{- end -}}

{{- define "shared-lib.containerSecurityContext" -}}
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
{{- /* A startupProbe suspends the liveness/readiness probes until it first succeeds,
       so slow-booting apps can't be killed mid-startup by a liveness failure. Prefer
       this over inflating livenessProbe.initialDelaySeconds: the delay has to cover
       the worst-case boot, which then also delays detection of a real hang. */ -}}
{{- if .Values.probes.startup }}
startupProbe:
  {{- toYaml .Values.probes.startup | nindent 2 -}}
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

{{- /* Pod-template checksums of the chart's own `configMaps` / `secrets` values (the common Helm
checksum/config pattern): editing their content changes the pod template, so the Deployment or
StatefulSet rolls on the next ArgoCD sync. Only emitted when the chart defines any, so charts without
them keep an unchanged pod template. ConfigMaps/Secrets created elsewhere are not covered. */ -}}
{{- define "shared-lib.configChecksumAnnotations" -}}
{{- $out := dict -}}
{{- if .Values.configMaps -}}
{{- $_ := set $out "checksum/configmaps" (toYaml .Values.configMaps | sha256sum) -}}
{{- end -}}
{{- if .Values.secrets -}}
{{- $_ := set $out "checksum/secrets" (toYaml .Values.secrets | sha256sum) -}}
{{- end -}}
{{- toYaml $out -}}
{{- end -}}
