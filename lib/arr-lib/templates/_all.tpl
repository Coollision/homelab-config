{{- define "arr-lib.all" -}}
{{- $ctx := deepCopy . -}}
{{- $vals := deepCopy .Values -}}
{{- if and $vals.arrXmlPostgres $vals.arrXmlPostgres.enabled }}
{{- $generated := (include "arr-lib.arrPostgresInitContainer" $ctx | fromYamlArray) -}}
{{- $existing := ($vals.initContainers | default (list)) -}}
{{- $_ := set $vals "initContainers" (concat $generated $existing) -}}
{{- end }}
{{- $_ := set $ctx "Values" $vals -}}
{{ include "shared-lib.all" $ctx }}
{{- end }}
