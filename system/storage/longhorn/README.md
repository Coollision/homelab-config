# Longhorn Distributed Storage

## Overview

Longhorn is a lightweight, reliable, and powerful distributed block storage system for Kubernetes. This deployment provides persistent volume management with high availability, automatic replication, and snapshot capabilities for the homelab cluster.

**Key Features:**

- **Distributed Storage**: Block storage distributed across multiple nodes
- **High Availability**: 3-way replication for data redundancy
- **Snapshots & Backups**: Point-in-time recovery and disaster recovery
- **Storage Orchestration**: Automatic scheduling and replica management
- **Web UI**: Management interface at `longhorn.declerck.dev`

## Architecture

### Deployment Configuration

Longhorn is deployed via ArgoCD using Helm chart version **1.10.1** with the following specifications:

```yaml
Namespace: storage
Replica Count: 3
Version: 1.10.1
Chart: https://charts.longhorn.io
```

### Node Configuration

Longhorn runs on **master nodes only** with dedicated disk storage:

| Node          | Disk Device      | Mount Path      | Storage Tag |
| ------------- | ---------------- | --------------- | ----------- |
| master-green  | `/dev/nvme0n1p3` | `/mnt/longhorn` | fast        |
| master-blue   | `/dev/sda3`      | `/mnt/longhorn` | fast        |
| master-silver | `/dev/sda3`      | `/mnt/longhorn` | fast        |

### Tolerations & Scheduling

The deployment is configured to run on master nodes despite taints:

```yaml
tolerations:
  - key: "node-role.kubernetes.io/master"
    operator: "Exists"
    effect: "NoSchedule"
  - key: "dedicated"
    operator: "Equal"
    value: "master"
    effect: "NoSchedule"
```

**Node Selector**: `node-role.kubernetes.io/master: "true"`

## Replication Strategy

### Default Replication

**Replica Count**: `3` (configured in `defaultReplicaCount`)

This means:

- Every persistent volume is replicated across **3 different nodes**
- Data is automatically synchronized between replicas
- Loss of 1-2 nodes still maintains data availability
- Minimum healthy replicas for read/write: 2 out of 3

### Replication Behavior

1. **Write Operations**:

   - Data written to primary replica
   - Synchronously replicated to secondary replicas
   - Write acknowledged after majority (2/3) confirmation

2. **Read Operations**:

   - Reads served from any healthy replica
   - Load balanced across available replicas

3. **Node Failure**:
   - Automatic failover to healthy replicas
   - New replica created on available node
   - Data rebalanced to maintain 3 replicas

### Replica Placement

Replicas are intelligently scheduled:

- **Anti-affinity**: Never place replicas on the same node
- **Disk-aware**: Considers available disk space
- **Tag-based**: Uses `fast` tag for optimal disk selection
- **Manual override**: Can be configured per StorageClass

## Storage Classes

Longhorn automatically creates storage classes for dynamic provisioning:

### Default Storage Class: `longhorn`

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: longhorn
provisioner: driver.longhorn.io
allowVolumeExpansion: true
parameters:
  numberOfReplicas: "3"
  staleReplicaTimeout: "2880"
  fromBackup: ""
  diskSelector: "fast"
  nodeSelector: ""
```

### Usage Example

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: my-app-data
spec:
  accessModes:
    - ReadWriteOnce
  storageClassName: longhorn
  resources:
    requests:
      storage: 10Gi
```

## Disk Management

### Automated Setup (Ansible)

The Ansible role `k3s_master/tasks/mount-disks.yml` automatically:

1. **Creates mount directory**: `/mnt/longhorn`
2. **Formats disk**: ext4 filesystem (non-destructive if already formatted)
3. **Mounts disk**: Persistent mount with `defaults,nofail` options
4. **Updates fstab**: Ensures mount persists across reboots

### Manual Disk Configuration

Each node has a custom disk configuration in `longhorn-extra/disks.yaml`:

```yaml
apiVersion: longhorn.io/v1beta2
kind: Node
metadata:
  name: master-green
  namespace: storage
spec:
  allowScheduling: true
  disks:
    longhorn-disk:
      path: /mnt/longhorn
      allowScheduling: true
      tags:
        - fast
```

**Configuration Options**:

- `allowScheduling`: Enable/disable replica scheduling on this disk
- `path`: Mount path for Longhorn storage
- `tags`: Metadata for disk selection (e.g., `fast`, `ssd`, `archive`)

## High Availability Features

### Data Redundancy

- **3-way replication**: Tolerates up to 2 node failures
- **Automatic healing**: Failed replicas rebuilt automatically
- **Consistent snapshots**: Point-in-time recovery across replicas

### Failure Scenarios

| Scenario          | Impact                       | Recovery                             |
| ----------------- | ---------------------------- | ------------------------------------ |
| 1 node down       | No data loss, full operation | Automatic rebuild on remaining nodes |
| 2 nodes down      | No data loss, degraded mode  | Read/write from single replica       |
| Disk failure      | Replica marked failed        | New replica on different disk        |
| Network partition | Split-brain protection       | Quorum-based access control          |

### Self-Healing

Longhorn continuously monitors replica health:

- **Checksums**: Detects silent data corruption
- **Automatic repair**: Rebuilds corrupted replicas
- **Health checks**: Periodic verification of replica consistency

## Access & Management

### Web UI

**URL**: `https://longhorn.declerck.dev`

**Features**:

- Volume management (create, delete, attach, detach)
- Replica status and health monitoring
- Snapshot creation and restoration
- Backup configuration
- Node and disk management
- System settings and tuning

### CLI Access

```bash
# Install longhornctl
kubectl apply -f https://raw.githubusercontent.com/longhorn/cli/main/longhornctl.yaml

# View volumes
kubectl -n storage get volumes.longhorn.io

# View replicas
kubectl -n storage get replicas.longhorn.io

# View nodes
kubectl -n storage get nodes.longhorn.io
```

## Prerequisites

### Node Requirements

**Packages** (installed via `ansible/roles/node-common/tasks/main.yml`):

- `open-iscsi` - iSCSI initiator for volume attachment
- `nfs-common` - NFS support for backups
- `cryptsetup` - Encryption support
- `dmsetup` - Device mapper for volume management

**Kernel Modules**:

- `iscsi_tcp` - Loaded automatically by open-iscsi
- `dm_crypt` - For encrypted volumes (optional)

### Network Requirements

- **Port 9500**: Longhorn Manager API
- **Port 8000**: Longhorn Engine (replica communication)
- **Port 9502**: Longhorn Conversion Webhook

## Backup & Disaster Recovery

### Snapshot Configuration

Snapshots can be created via:

1. **Web UI**: Manual snapshot creation
2. **RecurringJob**: Automated scheduled snapshots
3. **kubectl**: `kubectl -n storage create -f snapshot.yaml`

### Backup Targets

Supported backup destinations:

- **NFS**: Network file system
- **S3**: AWS S3 or compatible (MinIO, Wasabi)
- **CIFS**: SMB/CIFS shares

Configure in Longhorn UI under **Settings → Backup Target**:

```
# NFS example
nfs://nfs-server.example.com:/backups

# S3 example
s3://bucket-name@region/
```

## Performance Tuning

### Disk Tags

The `fast` tag is applied to all disks for performance-sensitive workloads:

```yaml
tags:
  - fast
```

Create additional storage classes targeting specific disk types:

```yaml
parameters:
  diskSelector: "fast"      # SSDs/NVMe
  diskSelector: "archive"   # HDDs for cold storage
```

### Replica Optimization

For performance-critical applications:

- **Single replica**: Higher performance, lower redundancy
- **Local volumes**: Pin replicas to specific nodes

For data safety:

- **3+ replicas**: Maximum redundancy
- **Cross-zone**: Distribute across failure domains

## Monitoring

### Metrics Exported

Longhorn exports Prometheus metrics on port `9500`:

- Volume capacity and usage
- Replica status and health
- I/O operations and latency
- Snapshot and backup status

### Alerts

Consider setting up alerts for:

- Replica degradation
- Disk space exhaustion
- Volume attachment failures
- Backup job failures

## Troubleshooting

### Common Issues

**1. Volume Won't Attach**

```bash
# Check replica status
kubectl -n storage get replicas.longhorn.io -o wide

# Check node connectivity
kubectl -n storage get nodes.longhorn.io
```

**2. Disk Space Issues**

```bash
# Check disk usage on nodes
kubectl -n storage get nodes.longhorn.io -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.diskStatus}{"\n"}{end}'
```

**3. Replica Rebuild Stuck**

- Check network connectivity between nodes
- Verify disk I/O performance
- Review Longhorn manager logs

### Log Collection

```bash
# Longhorn manager logs
kubectl -n storage logs -l app=longhorn-manager

# Longhorn UI logs
kubectl -n storage logs -l app=longhorn-ui

# Engine logs (per volume)
kubectl -n storage logs <engine-pod-name>
```

## Maintenance

### Upgrading Longhorn

**Current Version**: 1.10.1 (November 2024)

**Key improvements in v1.10.x**:

- V2 Data Engine enhancements (interrupt mode, volume expansion, cloning)
- IPv6 support for V1 Data Engine
- Delta replica rebuilding for faster recovery
- Configurable backup block size
- CSI Storage Capacity support for capacity-aware scheduling
- Improved stability and performance

**Upgrade Process**:

1. Update `targetRevision` in `longhorn.yaml`
2. Commit and push changes
3. ArgoCD auto-syncs the update
4. Longhorn performs rolling update (zero downtime)

**Note**: Ensure Kubernetes v1.25+ before upgrading to v1.10.1

### Adding New Disks

1. Update Ansible inventory with new `longhorn_disk_device`
2. Run disk mounting playbook
3. Apply new disk configuration in `longhorn-extra/disks.yaml`
4. Longhorn automatically starts using new storage

### Node Maintenance

```bash
# Disable scheduling on node
kubectl -n storage patch nodes.longhorn.io <node-name> --type='json' -p='[{"op": "replace", "path": "/spec/allowScheduling", "value":false}]'

# Evict replicas (via UI or kubectl)
# Perform maintenance
# Re-enable scheduling
```

## Security Considerations

- **Network Policies**: Consider restricting replica communication
- **Encryption**: Enable volume encryption for sensitive data
- **Access Control**: RBAC rules for Longhorn resources
- **UI Authentication**: Configure ingress authentication (OAuth2, BasicAuth)

## References

- **Longhorn Documentation**: https://longhorn.io/docs/
- **Helm Chart**: https://github.com/longhorn/longhorn
- **GitHub Repository**: https://github.com/longhorn/longhorn
- **Community Support**: https://slack.cncf.io/ (#longhorn)
