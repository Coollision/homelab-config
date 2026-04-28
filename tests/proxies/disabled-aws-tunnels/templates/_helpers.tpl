{{- define "aws-tunnels.namespace" -}}
{{- .Values.namespace -}}
{{- end -}}

{{- define "aws-tunnels.partOf" -}}
aws-tunnel
{{- end -}}

{{- define "aws-tunnels.excludedNodeAffinity" -}}
nodeAffinity:
  requiredDuringSchedulingIgnoredDuringExecution:
    nodeSelectorTerms:
      - matchExpressions:
          - key: type
            operator: NotIn
            values:
              - {{ .Values.nodeAffinity.excludedType | quote }}
{{- end -}}

{{- define "aws-tunnels.tunnelFullName" -}}
{{- printf "aws-tunnel-%s" .name -}}
{{- end -}}

{{- define "aws-tunnels.authProfilesCsv" -}}
{{- $profiles := list .Values.aws.profile -}}
{{- range .Values.aws.extraProfiles }}
{{- $profiles = append $profiles .name -}}
{{- end -}}
{{- join "," (uniq $profiles) -}}
{{- end -}}

{{/*
Helper placeholder: we compute checksums inline where needed using .Files.Get and sha256sum.
*/}}
