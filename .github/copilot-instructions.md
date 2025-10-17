# Homelab Kubernetes Infrastructure - AI Agent Guide

## Architecture Overview

This is a GitOps-managed Kubernetes homelab using **ArgoCD** for deployment automation and **K3s** for the cluster runtime. The repository follows a structured approach where ArgoCD manages itself and all applications through Git-based configuration.

## Key Components & Dependencies

- **Bootstrap Order**: Storage → Vault → ArgoCD → Everything else
- **ArgoCD**: Self-managing GitOps controller with ApplicationSets for auto-discovery
- **Vault**: HashiCorp Vault for secret management with `<secret:kv/path~key>` interpolation
- **Traefik**: Dual ingress setup (internal/external) with IngressRoute CRDs
- **Shared Library**: Helm template library in `lib/shared-lib/` for common patterns

## Critical Workflows

### Initial Setup

```bash
# Always run helm dependency update before installation
helm dependency update

# Bootstrap sequence (replace secrets with your values)
helm install storage system/kube-system/storage -n kube-system
helm install vault system/secrets/vault -n secrets --create-namespace
# Unseal vault manually, then:
helm install argocd argocd -n argocd --create-namespace
```

### Managing Applications

- **Enable/Disable**: Prefix directories with `disabled-` to exclude from ArgoCD discovery
- **ApplicationSets**: Auto-discover applications in `system/*/*` and `workload/*/*` paths
- **Projects**: `system` (infrastructure) vs `workloads` (applications) separation

### Secret Management

Use Vault with ArgoCD Lovely Plugin syntax:

```yaml
# In values.yaml files
server: <secret:kv/data/storage/nfs~server-ip>
password: <secret:kv/data/domains~cloudflare-token>
```

## Directory Structure Patterns

### System vs Workload Organization

```
system/           # Infrastructure (namespaced by k8s namespace)
├── kube-system/  # Core cluster services
├── secrets/      # Vault, sealed-secrets
└── monitoring/   # Observability stack

workload/         # Applications (namespaced by function)
├── services/     # Media services (Sonarr, Radarr, etc.)
├── smarthome/    # IoT and automation
└── databases/    # Data persistence
```

### Ansible Infrastructure

```
ansible/
├── inventory/           # Production hosts
├── inventory_example/   # Template inventory
├── playbooks/          # K3s setup/teardown
└── roles/              # Reusable components
```

## Development Conventions

### ArgoCD Applications

- Use `argocd-lovely-plugin` for Vault secret interpolation
- ApplicationSets automatically discover subdirectories
- Sync waves control deployment order via `syncWave` values

### Helm Shared Library (`lib/shared-lib/`)

The shared library provides a comprehensive templating system for consistent service deployment:

**Core Template**: `{{ include "shared-lib.all" . }}` renders complete application stacks
**Key Components**:

- `_app-deployment.yaml` / `_app-statefullset.yaml`: Workload definitions with built-in affinity, probes, storage
- `_service.yaml`: Automatic service creation from `deployment.ports` or `statefullset.ports`
- `_storage.yaml`: NFS storage with path-based (`storagePath`) or name-based provisioning
- `_ingress-*.yaml`: Traefik IngressRoute patterns (internal, internal-secure, external-secure)

**Usage Patterns**:

```yaml
# Chart.yaml - Always include shared-lib dependency
dependencies:
  - name: shared-lib
    version: 0.1.0
    repository: file://../../../lib/shared-lib

# values.yaml - Standard structure
deployment: # or statefullset:
  image:
    repository: myapp
  ports:
    http: 8080 # Auto-creates service + container ports

ingress_internal: # Creates Traefik IngressRoute
  host: app.<secret:kv/data/domains~local>
  port: http # References ports above

storage: # Auto-mounts + creates PVCs
  - mountPath: /data
    storagePath: myapp/data # nfs.io/storage-path annotation
    size: 10Gi
    type: nfs-client

syncWave: -10 # ArgoCD sync ordering
```

**Multi-Service Pattern** (like `wishlist`):

```helm
# Split values into separate services
{{ $serviceAv := deepCopy . }}
{{ $_ := set $serviceAv "Values" .Values.serviceA }}
{{ include "shared-lib.all" $serviceAv }}
```

### Service Definitions

Two distinct patterns:

1. **Shared Library Services**: Use `{{ include "shared-lib.all" . }}` template (preferred)
   - Single-line template file: `templates/app.yaml`
   - Configuration-driven via `values.yaml`
   - Examples: `workload/testing/echo`, `workload/wishlist/wishlist`
2. **Raw YAML Services**: Direct Kubernetes manifests
   - Full YAML definitions in `servicename.yaml`
   - Manual resource management
   - Examples: `workload/services/sonarr`

### Node Management

- Raspberry Pi nodes: `type: rpi` label, special tolerations needed
- Affinity rules prevent critical services on Pi nodes
- `preventDeschedule: "true"` label for stateful services

## Common Debugging

- **ArgoCD sync issues**: Check if monitoring/traefik creates circular dependencies
- **Storage problems**: Ensure NFS provisioner is running before other services
- **Vault sealed**: Applications fail if Vault is sealed after restart
- **Missing secrets**: Check Vault KV store at `kv/data/path` structure

## File Patterns to Recognize

- `values.yaml`: Helm chart values with Vault secret references
- `*app.yaml`: ArgoCD Application/ApplicationSet definitions
- `disabled-*`: Excluded from auto-discovery
- `Chart.yaml`: Helm chart dependencies (run `helm dependency update`)
