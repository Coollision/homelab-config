# Ansible: node provisioning

[← Back to Home](../Home.md) · See also: [Bootstrap](bootstrap.md) · [Ansible cheat sheet](../05-operations/ansible-cheatsheet.md)

Ansible's entire job is: turn a freshly-preseeded box into a joined, hardened k3s node.
It has **zero knowledge of Kubernetes objects, Helm, or ArgoCD** — that boundary is
deliberate (see [Bootstrap](bootstrap.md)).

## Inventory

```text
ansible/inventory/hosts
```

```ini
[k3scluster:children]
k3smaster
k3sworkers

[k3smaster]
<control-plane-node> ansible_host=<control-plane-node>.<internal-domain>

[k3sworkers]
<worker-node-1> ansible_host=<worker-node-1>.<internal-domain>
<worker-node-2> ansible_host=<worker-node-2>.<internal-domain>
<pi-node-1> ansible_host=<pi-node-1>.<internal-domain>

[nodes:children]
vm
mini
rpi

[rpi]
<pi-node-1>       # the Raspberry Pi worker

[vm]
<worker-node-2>   # the VM-class worker

[mini]
<worker-node-1>   # Mac mini-class hosts
<control-plane-node>

[unused]
<retired-node>    # kept in the inventory, not in any active group
```

(The real inventory uses per-host codenames; group structure and role split is what
matters here, not the specific names.)

**⚠️ Naming trap:** the k3s *node* names (as seen by `kubectl get nodes`) are **not the
same strings** as the Ansible/DNS/SSH hostnames for the same machines — a k3s node name
will NXDOMAIN if you try to resolve or SSH to it directly. Always use the Ansible
inventory hostname for SSH/DNS, and the `kubectl get nodes` name only for Kubernetes
operations. See [Known Issues](../05-operations/known-issues.md#node-naming-split).

`ansible/inventory_example/` is a sanitized template of the real inventory — use it as a
reference for the shape without real hosts/secrets. `ansible.cfg` points `roles_path`,
`inventory`, `group_vars`, and `host_vars` all at the `ansible/` subtree, so all Ansible
commands below assume you've `cd ansible` first.

## Roles, in the order they run

| Role | Runs on | Does |
|---|---|---|
| `node-common` | all nodes | hostname, locale/timezone, aliases, `apt upgrade`, baseline packages (`nfs-common`, `open-iscsi`, `cryptsetup`, …), inotify limits, NFS kernel modules, includes `journald-config` |
| `rpi-common` | Pis only | Pi-specific tweaks, temp/voltage aliases via `vcgencmd`, disables swap, tunes UDP buffers |
| `python` | (not in `node_setup.yml`) | idempotently ensures `python3` exists (needed before Ansible can gather facts at all on a truly blank box) |
| `sudoers` | (not in `node_setup.yml`) | grants the ansible user passwordless sudo if not already present |
| `setup-ssh-key` | all nodes | pushes/rotates the SSH public key into `authorized_keys` |
| `journald-config` | all nodes | templates `journald.conf` with retention caps, vacuums logs |
| `vlan-trunk` / `vlan-trunk-teardown` | nodes needing VLAN legs | builds VLAN sub-interfaces (systemd-networkd or ifupdown, auto-detected), plus the IPv6 RA-acceptance sysctl needed by the Thread border router |
| `k3s_pre` | all cluster nodes | downloads the pinned k3s binary, loads `dm_crypt` (needed by Longhorn), templates the private registry mirror config |
| `k3s_master` | control-plane node(s) | mounts storage disks, installs k3s server (primary gets no token; additional masters fetch the primary's join token), sets up an etcd-defrag timer |
| `k3s_worker` | worker nodes | fetches the join token from the primary, installs `k3s-node.service` |
| `k3s_teardown` / `node_teardown` | — | full reset: stops services, kills lingering containerd shims, unmounts, deletes data dirs |
| `node_update` / `cluster_wait_stable` | all cluster nodes | one cordon → drain → uncordon pass per node covering OS packages and the k3s binary bump; `cluster_wait_stable` is the barrier between nodes (pods settled, every Longhorn volume Healthy) |
| `dns` / `dns_teardown` | — | RFC 2136 dynamic DNS updates (TSIG-signed) against Technitium — node A/AAAA records, `k3s.<zone>`, the ingress wildcard |

## Playbooks

```text
ansible/playbooks/
  node_setup.yml         # node-common / rpi-common (python + sudoers are commented out)
  cluster_update.yml      # rolling OS + k3s upgrade, dry-run by default (-e dry_run=false to apply)
  k3s_setup.yml           # k3s_pre → vlan-trunk → k3s_master → k3s_worker
  dns.yml                 # DNS record injection (run AFTER Technitium is live)
  setup-ssh-keys.yml
  vlan_trunk_setup.yml / vlan_trunk_teardown.yml
  node_teardown.yml / k3s_teardown.yml
```

## Running things

See the [Ansible cheat sheet](../05-operations/ansible-cheatsheet.md) for copy-pasteable
ad-hoc commands (uptime checks, rolling reboots, re-running a single role against one
host, etc).
