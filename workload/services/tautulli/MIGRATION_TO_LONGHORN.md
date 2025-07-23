# Tautulli Migration to Longhorn Storage

## Overview

Simple migration from NFS to Longhorn storage. Since NFS data is never deleted (`onDelete: retain`), we can safely proceed without backups. and data on NFS is backed up regularly via existing external backup jobs, so no worries there.

## Current Configuration

- **Storage Type**: NFS (`nfs-client`)
- **Mount Path**: `/config`
- **Size**: 10Gi
- **Current PVC**: `tautulli` in `services` namespace
- **NFS Retention**: Data retained on NFS even after PVC deletion

## Migration Strategy

**Simple approach**: Scale down → Copy data → Swap storage → Scale up

### Estimated Downtime: 11-17 minutes

**Timeline Breakdown:**

- Pause ArgoCD (label + sync policy): ~10 seconds
- Scale down Tautulli: ~30 seconds
- Data migration job: ~5-10 minutes (depends on data size)
- Update values.yaml + git push: ~1 minute
- Re-enable ArgoCD + force sync: ~30 seconds
- Pod startup: ~2-3 minutes

**Note**: Using ArgoCD CLI commands is much faster than git-based directory renaming.

---

## Pre-Migration Checklist

- [ ] Verify Longhorn is installed and operational
- [ ] Ensure Longhorn has sufficient storage capacity (10Gi + overhead)
- [ ] Verify nodes have the `fast` disk tag configured

---

## ArgoCD Management Options

Since Tautulli is managed by an ApplicationSet with `selfHeal: true` and `prune: true`, you need to temporarily disable ArgoCD's auto-sync to make manual changes. Here are the available methods:

### Method 1: Label + Sync Policy (Recommended for Migration)

**Best for**: Extended maintenance work like this migration

```bash
# Pause ArgoCD management completely
kubectl label app tautulli -n argocd argocd.argoproj.io/applicationset-ignore=true
kubectl patch app tautulli -n argocd --type=json -p='[{"op": "remove", "path": "/spec/syncPolicy"}]'

# ApplicationSet will now ignore this app until you remove the label
# You can make any changes without interference

# When done, restore management
kubectl patch app tautulli -n argocd --type=merge -p='{"spec":{"syncPolicy":{"automated":{"prune":true,"selfHeal":true}}}}'
kubectl label app tautulli -n argocd argocd.argoproj.io/applicationset-ignore-
kubectl patch app tautulli -n argocd -p '{"metadata": {"annotations":{"argocd.argoproj.io/refresh":"hard"}}}' --type=merge
```

### Method 2: Sync Policy Only (Quick Edits)

**Best for**: Quick changes (but ApplicationSet will restore policy after ~3 minutes)

```bash
# Suspend auto-sync temporarily
kubectl patch app tautulli -n argocd --type=json -p='[{"op": "remove", "path": "/spec/syncPolicy"}]'

# Make your changes quickly...

# Re-enable
kubectl patch app tautulli -n argocd --type=merge -p='{"spec":{"syncPolicy":{"automated":{"prune":true,"selfHeal":true}}}}'
```

### Method 3: Directory Rename (Git-Based)

**Best for**: If you don't have ArgoCD CLI access

```bash
# Rename directory to exclude from ApplicationSet
cd /Users/youri/Desktop/_HomeLabConfig
mv workload/services/tautulli workload/services/disabled-tautulli
git add -A && git commit -m "temp: disable tautulli" && git push

# Wait for ApplicationSet to prune (~30-60 seconds)
watch kubectl get app tautulli -n argocd

# Make your changes...

# Re-enable by renaming back
mv workload/services/disabled-tautulli workload/services/tautulli
git add -A && git commit -m "feat: re-enable tautulli" && git push
```

**For this migration, we'll use Method 1** (Label + Sync Policy) as it's the cleanest approach.

---

## Migration Steps

### Step 1: Disable Auto-Sync and Scale Down

**⏱️ Downtime starts here**

```bash
# Pause ArgoCD management (Method 1)
kubectl label app tautulli -n argocd argocd.argoproj.io/applicationset-ignore=true
kubectl patch app tautulli -n argocd --type=json -p='[{"op": "remove", "path": "/spec/syncPolicy"}]'

# Scale down Tautulli
kubectl scale statefulset tautulli -n services --replicas=0

# Wait for pod to terminate
kubectl get pods -n services -l app=tautulli -w
# Press Ctrl+C when pod is gone
```

---

### Step 2: Create Longhorn PVC

```bash
# Create new Longhorn PVC
cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: tautulli-longhorn
  namespace: services
spec:
  accessModes:
    - ReadWriteOnce
  storageClassName: longhorn
  resources:
    requests:
      storage: 10Gi
EOF

# Verify PVC is bound
kubectl get pvc -n services tautulli-longhorn
# Should show "Bound" status
```

---

### Step 3: Copy Data from NFS to Longhorn

```bash
# Create one-time migration job
cat <<EOF | kubectl apply -f -
apiVersion: batch/v1
kind: Job
metadata:
  name: tautulli-migration
  namespace: services
spec:
  template:
    spec:
      restartPolicy: Never
      containers:
      - name: rsync
        image: instrumentisto/rsync-ssh:latest
        command:
        - /bin/sh
        - -c
        - |
          echo "Starting migration at \$(date)"
          rsync -av --info=progress2 /source/ /destination/
          echo "Migration completed at \$(date)"
          echo "Verifying data..."
          du -sh /source /destination
        volumeMounts:
        - name: source
          mountPath: /source
        - name: destination
          mountPath: /destination
      volumes:
      - name: source
        persistentVolumeClaim:
          claimName: tautulli
      - name: destination
        persistentVolumeClaim:
          claimName: tautulli-longhorn
EOF

# Monitor the migration
kubectl logs -n services -l job-name=tautulli-migration -f

# Wait for completion
kubectl wait --for=condition=complete --timeout=30m job/tautulli-migration -n services
```

---

### Step 4: Delete Old NFS PVC

NFS data is retained on the server (`onDelete: retain`), so we can safely delete the PVC.

```bash
# Delete old NFS PVC
kubectl delete pvc tautulli -n services

# Verify it's gone
kubectl get pvc -n services | grep tautulli
# Should only show: tautulli-longhorn
```

---

### Step 5: Rename Longhorn PVC to Production Name

```bash
# Rename the Longhorn PVC to match what values.yaml expects
kubectl get pvc tautulli-longhorn -n services -o yaml | \
  sed 's/name: tautulli-longhorn/name: tautulli/' | \
  kubectl apply -f -

# Delete the temp name
kubectl delete pvc tautulli-longhorn -n services
```

---

### Step 6: Update values.yaml

Update the Tautulli configuration to use Longhorn storage class.

**Edit `workload/services/tautulli/values.yaml`:**

```yaml
# Change this:
storage:
  - mountPath: /config
    storagePath: tautulli/config
    size: 10Gi
    type: nfs-client

# To this:
storage:
  - mountPath: /config
    size: 10Gi
    type: longhorn
    storageClass: longhorn
```

**Commit and push:**

```bash
cd /Users/youri/Desktop/_HomeLabConfig
git add workload/services/tautulli/values.yaml
git commit -m "chore(tautulli): migrate to Longhorn storage"
git push origin storrage/longhorn
```

---

---

### Step 7: Re-enable ArgoCD Management and Deploy

Restore ArgoCD management and sync the new configuration.

```bash
# Re-enable ArgoCD management
kubectl patch app tautulli -n argocd --type=merge -p='{"spec":{"syncPolicy":{"automated":{"prune":true,"selfHeal":true}}}}'
kubectl label app tautulli -n argocd argocd.argoproj.io/applicationset-ignore-

# Force sync to deploy with new Longhorn configuration
kubectl patch app tautulli -n argocd -p '{"metadata": {"annotations":{"argocd.argoproj.io/refresh":"hard"}}}' --type=merge

# Wait for Tautulli to come back up
kubectl get pods -n services -l app=tautulli -w
# Press Ctrl+C when pod is Running
```

**Verify the pod is running:**

```bash
kubectl get pods -n services -l app=tautulli
kubectl logs -n services -l app=tautulli -f
```

**⏱️ Downtime ends here**

---

### Step 8: Verify Migration Success

1. **Check Tautulli UI**: Access `tautulli.<your-domain>`
2. **Verify data**: Check that your:
   - History is intact
   - Users are present
   - Settings are preserved
   - Recent activity shows correctly
3. **Check Longhorn UI**: Visit `longhorn.declerck.dev`
   - Verify the volume shows healthy
   - Check replica count (should be 3)
   - Confirm storage usage

```bash
# Check PVC binding
kubectl get pvc -n services tautulli
# Should show: storageClassName: longhorn

# Check volume in Longhorn
kubectl get volumes.longhorn.io -n storage | grep tautulli

# Verify replicas
kubectl get replicas.longhorn.io -n storage | grep tautulli
```

---

### Step 9: Cleanup

Once verified (can be done immediately):

```bash
# Delete migration job
kubectl delete job tautulli-migration -n services
```

**Note**: NFS data remains on the NFS server at `services/tautulli` path. You can manually delete it later if needed, or keep it as a backup.

---

## Performance Comparison

### Before (NFS)

- Network-attached storage
- Subject to network latency
- Single point of failure (NFS server)
- Shared bandwidth with other services

### After (Longhorn)

- Distributed block storage
- Direct attached (iSCSI)
- 3-way replication (high availability)
- Per-volume performance isolation
- Automatic replica rebuilding on node failure

---

## Troubleshooting

### Issue: Migration job stuck or slow

```bash
# Check job pod logs
kubectl logs -n services -l job-name=tautulli-migration

# Check source PVC mount
kubectl describe pvc tautulli -n services

# Check destination PVC mount
kubectl describe pvc tautulli-longhorn-temp -n services
```

### Issue: Tautulli won't start after migration

```bash
# Check pod events
kubectl describe pod -n services -l app=tautulli

# Check logs
kubectl logs -n services -l app=tautulli

# Verify PVC binding
kubectl get pvc tautulli -n services

# Check file permissions
kubectl exec -n services -it <tautulli-pod> -- ls -la /config
```

### Issue: Data appears incomplete

```bash
# Scale down Tautulli
kubectl scale statefulset tautulli -n services --replicas=0

# Run verification job
cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: verify-tautulli-data
  namespace: services
spec:
  containers:
  - name: verify
    image: busybox
    command: ['sh', '-c', 'ls -laR /data && du -sh /data']
    volumeMounts:
    - name: data
      mountPath: /data
  volumes:
  - name: data
    persistentVolumeClaim:
      claimName: tautulli
  restartPolicy: Never
EOF

# Check output
kubectl logs -n services verify-tautulli-data

# Compare with NFS backup
```

### Issue: Need to rollback

See "Rollback Plan" section below. NFS data is retained and can be remounted.

---

## Rollback Plan

If something goes wrong, NFS data is still on the server (`onDelete: retain` policy).

**Quick Rollback:**

```bash
# 1. Scale down Tautulli (if it's running with Longhorn)
kubectl scale statefulset tautulli -n services --replicas=0

# Wait for pod to terminate
kubectl get pods -n services -l app=tautulli -w

# 2. Delete Longhorn PVC
kubectl delete pvc tautulli -n services

# 3. Recreate NFS PVC pointing to existing data
cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: tautulli
  namespace: services
  annotations:
    nfs.io/storage-path: "services/tautulli"
spec:
  accessModes:
    - ReadWriteOnce
  storageClassName: nfs-client
  resources:
    requests:
      storage: 10Gi
EOF

# 4. Revert values.yaml to use type: nfs-client
# 4. Revert values.yaml to use type: nfs-client
cd /Users/youri/Desktop/_HomeLabConfig/workload/services/tautulli
# Edit values.yaml and change:
#   storage:
#     - type: longhorn  ← change back to: nfs-client
git add -A
git commit -m "rollback(tautulli): revert to NFS storage"
git push origin storrage/longhorn

# 5. Force ArgoCD to sync the reverted configuration
kubectl patch app tautulli -n argocd -p '{"metadata": {"annotations":{"argocd.argoproj.io/refresh":"hard"}}}' --type=merge

# Wait for pod to start
kubectl get pods -n services -l app=tautulli -w
```

**Note**: Since we used the ApplicationSet ignore label during migration, ArgoCD should still be managing the app normally for rollback. If you had removed the label, it will auto-sync the changes anyway.

---

## Timeline Estimate

| Step                          | Time           |
| ----------------------------- | -------------- |
| Pause ArgoCD (label + policy) | 10 sec         |
| Scale down app                | 30 sec         |
| Create Longhorn PVC           | 1 min          |
| Copy data (10Gi)              | 5-10 min       |
| Delete & rename PVCs          | 1 min          |
| Update values.yaml & commit   | 1 min          |
| Re-enable ArgoCD & force sync | 30 sec         |
| Pod startup                   | 2-3 min        |
| **Total Downtime**            | **~11-17 min** |

---

## Post-Migration Benefits

✅ **High Availability**: 3 replicas across different nodes  
✅ **Better Performance**: Local disk speeds via iSCSI  
✅ **Automatic Snapshots**: Schedule recurring snapshots via Longhorn  
✅ **Backup to S3**: Configure Longhorn backup target for offsite backups  
✅ **Volume Management**: Resize, clone, and manage via Longhorn UI

---

## Next Steps After Migration

1. **Configure automated snapshots** in Longhorn UI
2. **Set up backup schedule** to S3/NFS backup target
3. **Monitor volume health** in Longhorn dashboard
4. **Repeat process** for other services (Sonarr, Radarr, etc.)

---

## Notes

- The old NFS storage path `tautulli/config` is no longer needed after migration
- Longhorn will create its own volume structure in `/mnt/longhorn` on the nodes
- You can view the physical storage location in the Longhorn UI
- Consider adding `preventDeschedule: "true"` label to the Tautulli pod for stability
