# Bootstrap: from blank disk to self-managing cluster

[← Back to Home](../Home.md) · See also: [Ansible](ansible.md) · [GitOps structure](gitops-structure.md)

Bootstrapping this homelab is a two-phase process with a hard line between them:
**Ansible builds the OS + k3s layer. Everything above that is Helm/ArgoCD.** Ansible
never touches a Kubernetes object; it stops the moment `kubectl get nodes` works.

```
┌─────────────┐   ┌──────────────┐   ┌─────────────┐   ┌──────────────┐
│  Debian      │──▶│  Ansible:    │──▶│  Ansible:   │──▶│  Ansible:    │
│  preseed     │   │  node prep   │   │  k3s install │   │  DNS records │
│  (SSH + sudo)│   │ (node-common)│   │ (join order) │   │ (Technitium) │
└─────────────┘   └──────────────┘   └─────────────┘   └──────────────┘
                                                                │
                                                                ▼
                                          ┌────────────────────────────────────┐
                                          │  Manual helm install, in order:    │
                                          │  1. storage (Longhorn/NFS)         │
                                          │  2. vault (unseal it!)             │
                                          │  3. argocd (app-of-apps)           │
                                          └────────────────────────────────────┘
                                                                │
                                                                ▼
                                          ArgoCD takes over everything else,
                                          forever, via applications/*.yaml
```

## 1. Imaging a new node

`node-setup/debian/preseed.cfg` is a Debian installer preseed — feed it to a fresh
install and you get, with zero interaction: no desktop, `en_US.UTF-8` /
`Europe/Brussels`, DHCP networking, a manual partition layout (512MB EFI + 64GB ext4
root, **rest of the disk left unallocated** — that's the space Longhorn will later
claim), swap disabled, a `homelab` user with passwordless sudo, root login and SSH
password auth both disabled, and an SSH public key injected via `late_command`. The
result is a box that's immediately reachable by Ansible.

## 2. Ansible: OS prep and k3s install

Full role-by-role detail is on the [Ansible page](ansible.md). The playbooks run in this
order:

1. `ansible/playbooks/node_setup.yml` — OS hardening/common config (`node-common` on
   everything, `rpi-common` on Pis).
2. `ansible/playbooks/vlan_trunk_setup.yml` — VLAN sub-interfaces where a node needs one
   (see [Networking](../01-platform/networking.md)); also enables IPv6 RA acceptance for
   the Thread border router's backbone interface.
3. `ansible/playbooks/k3s_setup.yml` — `k3s_pre` (binary + registry mirror config) →
   `k3s_master` (primary, then any HA joins) → `k3s_worker` (token join).
4. `ansible/playbooks/dns.yml` — **run last, and only once Technitium is already up on
   the cluster** — injects A/AAAA records for each node, a `k3s.<zone>` record listing
   all control-plane IPs, and the wildcard `*.<zone>` → the Traefik LoadBalancer IP. A
   Synology NAS DNS server is the bootstrap resolver before this "authority flip" to the
   in-cluster Technitium primary.

At this point `kubectl get nodes` works and Ansible's job is done.

## 3. The manual chicken-and-egg bootstrap (Helm)

From the top-level `README.md`, in order, run manually:

```bash
# 1. Storage must exist first — everything else needs a StorageClass
helm install storage system/kube-system/storage -n kube-system

# 2. Vault next — unseal it before continuing (see the Vault page/readme)
helm install vault system/secrets/vault -n secrets --create-namespace

# 3. ArgoCD — comment out both *-app files first, install, then uncomment + upgrade
#    so it doesn't try to manage itself before it exists
helm install argocd argocd -n argocd --create-namespace
#   ...uncomment argo-app.yaml / aplications-app.yaml...
helm upgrade argocd argocd -n argocd
```

After the final `helm upgrade`, ArgoCD is running **and managing itself** (see
[GitOps structure](gitops-structure.md#argocd-manages-itself)), and its `applications-app`
Application starts generating every other Application from the `applications/` directory.

**Known chicken-and-egg trap:** Traefik's chart wants a `ServiceMonitor`, which needs
monitoring installed; monitoring wants an Ingress, which needs Traefik. The documented
workaround is to disable Traefik's ServiceMonitor via a commit first, let both come up,
then re-enable it.

## Why the split exists

Ansible answers "is there a computer with SSH and k3s on it" — a question about physical
machines. Everything past that (storage classes, secret management, which apps exist) is
answered entirely by files in this repo, reconciled by ArgoCD. This means:

- **Rebuilding a node** is an Ansible re-run, not a Kubernetes operation.
- **Adding/changing an app** is a Git commit, never a manual `kubectl apply` (see
  [Known Issues](../05-operations/known-issues.md) for what happens when someone forgets
  this and hand-patches a live object anyway).
- **Disaster recovery** for the *cluster itself* is: reimage nodes with the same preseed,
  re-run the Ansible playbooks, redo the three-step Helm bootstrap, and ArgoCD reconstructs
  every workload from Git + Vault + Longhorn backups. No single commit in this repo's
  history shows this having actually been needed end-to-end — the March 2026 Longhorn
  buildout added disaster-recovery *tooling* (a DR test chart, `nuke-longhorn.sh`)
  proactively, not reactively. See [History & Evolution](../05-operations/history.md).
