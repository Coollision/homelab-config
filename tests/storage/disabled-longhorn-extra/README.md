# Longhorn Extra Configuration

This directory contains additional Longhorn configurations not included in the base Longhorn Helm chart.

## Components

### Storage Classes

- **storageclass-shared.yaml**: RWX (ReadWriteMany) storage class using NFS share-manager

  - `reclaimPolicy: Retain` - Data persists after PVC deletion
  - `nfsOptions: vers=4.2,noresvport`
  - 3 replicas
  - Supports `fromBackup` parameter for restore operations

- **storageclass-retain.yaml**: RWO (ReadWriteOnce) storage class with data protection
  - `reclaimPolicy: Retain` - Data persists after PVC deletion
  - Standard block storage (no NFS)
  - 3 replicas
  - Ideal for databases and single-pod persistent workloads

### Backup Configuration

- **backup-config.yaml**: Longhorn backup infrastructure
  - **NFS PVC**: `longhorn-backup-nfs` (500Gi) for backup storage
  - **Backup Target**: Configured via `longhorn-default-setting` ConfigMap patch
  - Target URL: `nfs://longhorn-backup-nfs.storage.svc.cluster.local:/`

### SMB Operator

Exposes Longhorn RWX volumes via SMB/CIFS for macOS/Windows access.

- **smb-operator-config.yaml**: ConfigMap with operator settings

  - Namespace: storage
  - Reconcile interval: 30s
  - LoadBalancer IP: 192.168.30.100

- **smb-operator-deploy.yaml**: Deployment, RBAC, and Service

  - Python-based operator with Watch API
  - Privileged container for NFS mounting
  - LoadBalancer service on port 445 (SMB)
  - Features: retry logic, event recording, health checks

- **smb_operator_v2.py**: Operator implementation (loaded via ConfigMap)
  - Watches PVCs with `smb-access` annotation
  - Auto-discovers Longhorn share-manager endpoints
  - Configures Samba shares dynamically
  - Supports multiple access modes: shared, read-only

## Deployment

### Using Kustomize

```bash
# Apply everything with automatic cleanup of old ConfigMaps
kubectl apply -k system/storage/longhorn-extra/ \
  --prune \
  --prune-allowlist core/v1/ConfigMap \
  --prune-allowlist core/v1/PersistentVolumeClaim \
  --prune-allowlist core/v1/ServiceAccount \
  --prune-allowlist apps/v1/Deployment \
  --prune-allowlist rbac.authorization.k8s.io/v1/ClusterRole \
  --prune-allowlist rbac.authorization.k8s.io/v1/ClusterRoleBinding \
  --prune-allowlist storage.k8s.io/v1/StorageClass \
  -l app.kubernetes.io/part-of=longhorn-extra

# Or without automatic cleanup (old ConfigMaps remain)
kubectl apply -k system/storage/longhorn-extra/

# Preview what will be applied
kubectl kustomize system/storage/longhorn-extra/
```

**Note on ConfigMap Pruning**: When the SMB operator code (`smb_operator_v2.py`) changes, Kustomize creates a new ConfigMap with a new hash suffix. Using the `--prune` flag with `--prune-allowlist` automatically removes old ConfigMaps that are no longer referenced. Without `--prune`, old ConfigMaps remain in the cluster until manually deleted.

### Manual Steps (if needed)

1. **Apply storage classes**:

   ```bash
   kubectl apply -f storageclass-shared.yaml
   kubectl apply -f storageclass-retain.yaml
   ```

2. **Configure backups**:

   ```bash
   # Create backup NFS PVC
   kubectl apply -f backup-config.yaml

   # Patch Longhorn settings to add backup-target
   kubectl patch configmap longhorn-default-setting -n storage --type=merge -p '{
     "data": {
       "default-setting.yaml": "taint-toleration: node-role.kubernetes.io/control-plane:NoSchedule;dedicated=master:NoSchedule\npriority-class: \"longhorn-critical\"\ndisable-revision-counter: \"{\\\"v1\\\":\\\"true\\\"}\"\nbackup-target: \"nfs://longhorn-backup-nfs.storage.svc.cluster.local:/\""
     }
   }'
   ```

3. **Deploy SMB operator**:
   ```bash
   kubectl apply -f smb-operator-config.yaml
   kubectl apply -f smb-operator-deploy.yaml
   ```

## Usage

### Creating Volumes with Backup/Restore

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: my-data
  namespace: my-app
  annotations:
    # For restore from backup
    longhorn.io/volume-restore-from-backup: "s3://backup-url/..."
spec:
  storageClassName: longhorn-shared # or longhorn-retain
  accessModes:
    - ReadWriteMany # or ReadWriteOnce for longhorn-retain
  resources:
    requests:
      storage: 10Gi
```

### Enabling SMB Access

Add the `smb-access` annotation to your PVC:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: my-data
  annotations:
    smb-access: "shared" # or "read-only"
spec:
  storageClassName: longhorn-shared
  accessModes:
    - ReadWriteMany
  resources:
    requests:
      storage: 10Gi
```

The SMB operator will automatically:

1. Discover the Longhorn share-manager endpoint
2. Mount it via NFS
3. Configure a Samba share
4. Expose it at `smb://192.168.30.100/<pvc-name>`

### Creating Backups

Via Longhorn UI:

1. Navigate to Volume → Actions → Create Backup
2. Backup will be stored in NFS backup target

Via kubectl:

```bash
kubectl create -f - <<EOF
apiVersion: longhorn.io/v1beta2
kind: Backup
metadata:
  name: my-volume-backup-$(date +%Y%m%d%H%M%S)
  namespace: storage
spec:
  snapshotName: manual-backup
  labels:
    app: my-app
EOF
```

## Data Protection Strategy

### Reclaim Policies

Both storage classes use `Retain` policy:

- **On PVC deletion**: PV becomes "Released" (not deleted)
- **Data preserved**: Volume data remains intact
- **Manual cleanup**: Admin must manually delete PV when truly done

### Backup/Restore Workflow

1. **Create backup** (via Longhorn UI or kubectl)
2. **Delete resources** (pods, PVCs, namespaces)
3. **Recreate with restore annotation**:
   ```yaml
   annotations:
     longhorn.io/volume-restore-from-backup: "<backup-url>"
   ```
4. **Pod starts with original data**

### PV Re-linking Workflow

1. **Note PV name** before deletion
2. **Delete PVC** (PV goes to "Released")
3. **Recreate PVC with same spec** + `volumeName` field:
   ```yaml
   spec:
     volumeName: pvc-abc123...
     storageClassName: longhorn-shared
   ```
4. **PV rebinds** to new PVC with existing data

## Monitoring

### SMB Operator Status

```bash
# Check operator health
kubectl get pods -n storage -l app=smb-operator

# View operator logs
kubectl logs -n storage -l app=smb-operator -f

# Check metrics endpoint
kubectl port-forward -n storage svc/smb-operator 9090:9090
curl http://localhost:9090/metrics

# View events
kubectl get events -n storage --field-selector involvedObject.kind=PersistentVolumeClaim
```

### Backup Status

```bash
# List all backups
kubectl get backups.longhorn.io -n storage

# Check backup target setting
kubectl get settings.longhorn.io -n storage -o jsonpath='{.items[?(@.metadata.name=="backup-target")].value}'

# Verify backup NFS PVC
kubectl get pvc longhorn-backup-nfs -n storage
```

## Troubleshooting

### Backup Target Not Working

1. Check backup NFS PVC is bound:

   ```bash
   kubectl get pvc longhorn-backup-nfs -n storage
   ```

2. Verify backup-target setting:

   ```bash
   kubectl get configmap longhorn-default-setting -n storage -o yaml
   ```

3. Check Longhorn manager logs:
   ```bash
   kubectl logs -n storage -l app=longhorn-manager --tail=100
   ```

### SMB Operator Issues

1. Check operator pod status:

   ```bash
   kubectl describe pod -n storage -l app=smb-operator
   ```

2. Verify RBAC permissions:

   ```bash
   kubectl auth can-i get pvc --as=system:serviceaccount:storage:smb-operator -n storage
   ```

3. Test SMB connectivity from macOS:
   ```bash
   smbutil view //192.168.30.100
   ```

### PV Re-linking Not Working

1. Check PV status:

   ```bash
   kubectl get pv | grep Released
   ```

2. Remove claimRef to make PV available:

   ```bash
   kubectl patch pv <pv-name> -p '{"spec":{"claimRef":null}}'
   ```

3. Recreate PVC with `volumeName` field

## Notes

- **Backup Target Configuration**: The `backup-target` setting is patched into the `longhorn-default-setting` ConfigMap. This requires the ConfigMap to exist (created by Longhorn Helm chart) before applying this kustomization.
- **SMB Access**: Only works with `longhorn-shared` storage class (RWX volumes)
- **Restore Operations**: The `longhorn.io/volume-restore-from-backup` annotation must contain the full backup URL
- **Storage Class Selection**:
  - Use `longhorn-shared` for multi-pod access + SMB
  - Use `longhorn-retain` for single-pod access (databases)
