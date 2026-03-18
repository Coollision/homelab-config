{{/*
  longhorn-storage-lib.volumes
  ─────────────────────────────
  Renders a Longhorn Volume CR + static PersistentVolume for every entry in
  .Values.storage.

  Global config via .Values.storageConfig (all optional, shown with defaults):

    storageConfig:
      prefix:             "monitoring"   # prepended to nameSuffix → volume/PV name
      longhornNamespace:  "storage"      # namespace the Longhorn Volume CR lives in
      claimNamespace:     "monitoring"   # namespace used in claimRef (when pvcName set)

  Per-entry fields in .Values.storage[]:

    nameSuffix    string    (required) combined with prefix → "<prefix>-<nameSuffix>"
    size          string    (required) e.g. "10Gi"
    accessMode    string    default "ReadWriteOnce"
    backupType    string    default "daily"
    protect       string    default "true"
    recurringJobs []string  optional recurring-job names to label-enable
    pvcName       string    optional — adds claimRef for StatefulSet-managed PVCs
                            whose name differs from the PV name
                            ⚠️  Always set pvcName for StatefulSet-managed PVCs.
                            If the PV ends up in Released state after a PVC deletion,
                            recover with:
                              kubectl patch pv <pv-name> --type=merge -p '{"spec":{"claimRef":null}}'
*/}}
{{- define "longhorn-storage-lib.volumes" -}}
{{- $cfg           := .Values.storageConfig | default dict }}
{{- $prefix        := required "storageConfig.prefix is required" $cfg.prefix }}
{{- $longhornNs    := $cfg.longhornNamespace | default "storage" }}
{{- $claimNs       := required "storageConfig.claimNamespace is required (used in claimRef when pvcName is set)" $cfg.claimNamespace }}
{{- $first := true }}
{{- range .Values.storage }}
{{- if not $first }}
---
{{- end }}
{{- $first = false }}
{{- $volName := printf "%s-%s" $prefix .nameSuffix }}
{{- $accessMode := .accessMode | default "ReadWriteOnce" }}
{{- $accessModeShort := "rwo" }}
{{- if eq $accessMode "ReadWriteMany" }}{{- $accessModeShort = "rwx" }}{{- end }}
{{- if eq $accessMode "ReadWriteOncePod" }}{{- $accessModeShort = "rwop" }}{{- end }}
{{- $sizeBytes := "" }}
{{- if hasSuffix "Ti" .size }}{{- $sizeBytes = printf "%.0f" (mulf (trimSuffix "Ti" .size | float64) 1099511627776.0) }}
{{- else if hasSuffix "Gi" .size }}{{- $sizeBytes = printf "%.0f" (mulf (trimSuffix "Gi" .size | float64) 1073741824.0) }}
{{- else if hasSuffix "Mi" .size }}{{- $sizeBytes = printf "%.0f" (mulf (trimSuffix "Mi" .size | float64) 1048576.0) }}
{{- else }}{{- $sizeBytes = .size }}
{{- end }}
# Longhorn Volume CR
apiVersion: longhorn.io/v1beta2
kind: Volume
metadata:
  name: {{ $volName }}
  namespace: {{ $longhornNs }}
  annotations:
    argocd.argoproj.io/sync-options: Delete=false,Prune=false
  labels:
    backup-type: "{{ .backupType | default "daily" }}"
    protect: "{{ .protect | default "true" }}"
{{- if .recurringJobs }}
{{- range .recurringJobs }}
    "recurring-job.longhorn.io/{{ . }}": "enabled"
{{- end }}
{{- end }}
spec:
  accessMode: "{{ $accessModeShort }}"
  dataEngine: "v1"
  dataLocality: "best-effort"
  frontend: "blockdev"
  numberOfReplicas: 2
  size: "{{ $sizeBytes }}"
  diskSelector: []
  nodeSelector: []
  replicaAutoBalance: "ignored"
  revisionCounterDisabled: true
  snapshotDataIntegrity: "ignored"
  Standby: false
  migratable: false
---
# Kubernetes PersistentVolume
apiVersion: v1
kind: PersistentVolume
metadata:
  name: {{ $volName }}
  annotations:
    argocd.argoproj.io/sync-options: Delete=false,Prune=false
spec:
  capacity:
    storage: {{ .size }}
  accessModes:
    - {{ $accessMode }}
  storageClassName: longhorn
  persistentVolumeReclaimPolicy: Retain
  csi:
    driver: driver.longhorn.io
    volumeHandle: {{ $volName }}
    fsType: ext4
{{- if .pvcName }}
  claimRef:
    namespace: {{ $claimNs }}
    name: {{ .pvcName }}
{{- end }}
{{- end }}
{{- end }}
