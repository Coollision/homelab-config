# Access Longhorn Volumes from Your Mac - AUTO-DISCOVERY

## Quick Setup (Zero Configuration!)

### 1. Deploy NFS Server

```bash
# Just deploy - no configuration needed!
kubectl apply -f system/storage/longhorn-extra/nfs-server.yaml
```

That's it! All Longhorn PVCs are automatically discovered and exported.

### 2. Get NFS Server IP

The NFS server has a **static IP** assigned by MetalLB (stored in Vault).

```bash
kubectl get svc longhorn-nfs-server -n storage -o jsonpath='{.status.loadBalancer.ingress[0].ip}'
```

Or retrieve from Vault: `kv/data/storage/longhorn-nfs~ip`

**Note**: You need to add this IP to Vault before deploying:

```bash
# Example: Set IP to 192.168.30.250
vault kv put kv/storage/longhorn-nfs ip=192.168.30.250
```

Choose an IP from your MetalLB pool range.

### 3. Mount on Your Mac

#### Via Finder (Easiest)

1. Open Finder
2. Press `⌘K` (or Go → Connect to Server)
3. Enter: `nfs://192.168.30.100/`
4. Click Connect

All volumes appear organized by namespace!

#### Via Terminal

```bash
# Mount root (all volumes)
sudo mount -t nfs -o resvport 192.168.30.100:/ /Volumes/longhorn

# Browse everything
ls /Volumes/longhorn/
# services/  databases/  secrets/  etc...

ls /Volumes/longhorn/services/
# tautulli/  sonarr/  radarr/  etc...
```

## Access Specific Volumes

### Mount Individual Volume

```bash
# Via Finder
# Press ⌘K, enter: nfs://192.168.30.100/services/tautulli

# Via Terminal
sudo mount -t nfs -o resvport 192.168.30.100:/services/tautulli /Volumes/tautulli
open /Volumes/tautulli
```

### Browse All Volumes at Once

```bash
# Mount everything
sudo mount -t nfs 192.168.30.100:/ /Volumes/longhorn

# Navigate by namespace/service
cd /Volumes/longhorn/services/tautulli
cd /Volumes/longhorn/databases/cloudbeaver
cd /Volumes/longhorn/secrets/vaultwarden
```

## Volume Organization

Volumes are automatically organized by namespace:

```
/Volumes/longhorn/
├── services/
│   ├── tautulli/
│   ├── sonarr/
│   ├── radarr/
│   └── ...
├── databases/
│   └── cloudbeaver/
├── secrets/
│   └── vaultwarden/
└── ...
```

## Permanent Mount (Auto-mount on Mac startup)

```bash
# Add to /etc/fstab.local (replace IP):
192.168.30.100:/ /Volumes/longhorn nfs resvport,rw,bg,hard,intr 0 0
```

Or use macOS automount:

```bash
# Create auto_nfs file
sudo nano /etc/auto_nfs

# Add line:
longhorn -fstype=nfs,resvport,rw 192.168.30.100:/

# Add to auto_master
sudo nano /etc/auto_master
# Add: /- auto_nfs

# Reload automount
sudo automount -vc
```

## Usage Examples

### Edit a config file

```bash
# Mount
sudo mount -t nfs 192.168.30.100:/tautulli /Volumes/longhorn-tautulli

# Edit with your favorite editor
code /Volumes/longhorn-tautulli/config.ini
# or
vim /Volumes/longhorn-tautulli/config.ini
```

### Copy files

```bash
# Copy from Mac to Longhorn
cp ~/Downloads/backup.db /Volumes/longhorn-tautulli/

# Copy from Longhorn to Mac
cp /Volumes/longhorn-tautulli/data.db ~/Desktop/
```

### Browse with Finder

Just drag and drop files like any normal folder!

## Troubleshooting

### Can't connect

```bash
# Check NFS server is running
kubectl get pods -n storage -l app=longhorn-nfs-server

# Check LoadBalancer IP
kubectl get svc longhorn-nfs-server -n storage

# Check from terminal
showmount -e 192.168.30.100
```

### Permission denied

The NFS server exports with `no_root_squash`, so permissions should work. If you have issues:

```bash
# Check file permissions in the volume
kubectl exec -n storage deployment/longhorn-nfs-server -- ls -la /exports/tautulli
```

### Slow performance

NFS over network will be slower than direct disk access. For quick edits, it's fine. For large data transfers, consider using `kubectl cp` or the backup system.
