{{- define "shared-lib.storage" }}
{{- $root := . }}
  {{- range $storage := .Values.storage }}
    {{- if and (eq $storage.type "nfs-client") (hasKey $storage "storagePath")}}
      {{- include "shared-lib.nfs-client-path" (dict "root" $root "storage" $storage)  -}}
    {{- else if eq $storage.type "nfs-client" }}
      {{- include "shared-lib.nfs-client-name" (dict "root" $root "storage" $storage) }}
    {{- else }}
      {{- fail "Unsupported storage type or missing storage path" }}
    {{- end }}
---
  {{- end }}
{{- end }}

{{- define "shared-lib.nfs-client-path" }}
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: {{ printf "%s-%s" (include "helm.fullname" .root ) ( default  "" .storage.nameSuffix )| trunc 63 | trimSuffix "-" }} 
  annotations:
    nfs.io/storage-path: {{ .storage.storagePath }}
  labels:
    {{- include "helm.labels" .root | nindent 4 }}
spec:
  storageClassName: nfs-client
  accessModes:
    - ReadWriteOnce
  resources:
    requests:
      storage: {{ .storage.size }}
{{end}}


{{- define "shared-lib.nfs-client-name"}}
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: {{ printf "%s-%s" (include "helm.fullname" .root ) ( default  "" .storage.nameSuffix )| trunc 63 | trimSuffix "-" }} 
  labels:
    {{- include "helm.labels" .root | nindent 4 }}
spec:
  storageClassName: nfs-client-name-path
  accessModes:
    - ReadWriteOnce
  resources:
    requests:
      storage: {{ .storage.size }}
{{end}}


