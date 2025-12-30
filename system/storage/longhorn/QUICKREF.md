# Longhorn Quick Reference

## Enable Backups (One-Time Setup)

```bash
# 1. Deploy NFS backup storage and configure backup target via CRD
kubectl apply -f system/storage/longhorn-extra/backup-config.yaml

# 2. Backup target is automatically configured via Setting CRD
# No UI configuration needed!

# 3. (Optional) Create recurring backup jobs by uncommenting in backup-config.yaml
```

## Create Recurring Backups (CRD-Based)

```bash
# 1. Edit backup-config.yaml and uncomment RecurringJob examples
vim system/storage/longhorn-extra/backup-config.yaml

# 2. Apply
kubectl apply -f system/storage/longhorn-extra/backup-config.yaml

# 3. Attach backup job to volumes
kubectl -n storage label volume pvc-xxxxx backup=daily
```

## Access Volume Files from Your Mac

```bash
# 1. Set static IP in Vault (one-time, before deployment)
vault kv put kv/storage/longhorn-nfs ip=192.168.30.250

# 2. Deploy NFS server (auto-discovers all volumes)
kubectl apply -f system/storage/longhorn-extra/nfs-server.yaml

# 3. Get NFS server IP (or use the one you set in Vault)
kubectl get svc longhorn-nfs-server -n storage -o jsonpath='{.status.loadBalancer.ingress[0].ip}'

# 4. Mount on Mac via Finder
# Press ⌘K, enter: nfs://<IP>/
# All volumes organized by namespace: /services/tautulli, /databases/cloudbeaver, etc.

# Or via terminal:
sudo mount -t nfs -o resvport <IP>:/ /Volumes/longhorn
open /Volumes/longhorn

# See MAC-ACCESS.md for detailed instructions
```

## Common Tasks

### List All Longhorn Volumes

```bash
kubectl get volumes.longhorn.io -n storage
kubectl get pvc -A --sort-by=.spec.storageClassName | grep longhorn
```

### Create Backup of Volume

```bash
# Via UI: Volume page → Select volume → Create Backup

# Via kubectl (get volume name first)
kubectl -n storage get volumes.longhorn.io
kubectl -n storage annotate volume <volume-name> longhorn.io/backup-volume=true
```

### Schedule Recurring Backup

```bash
# Via UI: Volume page → Select volume → Schedule Recurring Backup
# Cron examples:
#   Daily at 2 AM:    0 2 * * *
#   Every 6 hours:    0 */6 * * *
#   Weekly Sunday:    0 3 * * 0
```

### Restore from Backup

```bash
# Via UI:
# 1. Go to Backup page
# 2. Select backup
# 3. Click "Restore"
# 4. Create new PVC or restore to existing
```

### Check Volume Health

```bash
# Get volume details
kubectl -n storage get volumes.longhorn.io <volume-name> -o yaml

# Check replicas
kubectl -n storage get replicas.longhorn.io | grep <volume-name>

# Via UI: Volume page shows health status
```

## Backup Storage Location

Your backups are stored on NFS:

```bash
# Location: <nfs-server>:<nfs-path>/longhorn-backups/
# This integrates with your existing NFS backup strategy
```

## File Access Methods

1. **On-Demand Pod** (recommended): Mount specific volumes, access files, cleanup
2. **Node DaemonSet** (optional): Persistent access to raw Longhorn storage on each node
3. **NFS Direct** (legacy): Access old NFS-backed data directly on NFS server
