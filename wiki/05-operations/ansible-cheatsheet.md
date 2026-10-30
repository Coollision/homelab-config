# Ansible cheat sheet

[← Back to Home](../Home.md) · See also: [Ansible (concepts)](../00-setup/ansible.md) · [kubectl cheat sheet](kubectl-cheatsheet.md)

All commands assume you're in the `ansible/` directory (`ansible.cfg` there points
`inventory`/`roles_path`/etc. at the right subpaths).

```bash
cd ansible
```

## Inventory groups you can target

| Group | Meaning |
|---|---|
| `k3scluster` | every k3s node (`k3smaster` + `k3sworkers`) |
| `k3smaster` | the control-plane node(s) |
| `k3sworkers` | worker nodes |
| `nodes` | `vm` + `mini` + `rpi` groups |
| `rpi` | Raspberry Pi hosts (currently one, also a k3s worker) |
| `vm` | the VM-class worker |
| `mini` | Mac mini-class hosts |
| `all` | literally everything in inventory |

## Ad-hoc one-liners

```bash
# Ping every host (connectivity + Python interpreter check)
ansible k3scluster -m ping

# Uptime across the whole cluster
ansible k3scluster -a "uptime"

# Same, but readable one-host-per-line summary
ansible k3scluster -m shell -a "uptime" -o

# Disk usage on every node
ansible k3scluster -a "df -h /"

# Memory usage
ansible k3scluster -a "free -h"

# Kernel + OS version
ansible k3scluster -a "uname -a"

# Check k3s service status everywhere
ansible k3scluster -m systemd -a "name=k3s" --become  # on the master
ansible k3sworkers -m systemd -a "name=k3s-node" --become

# Reboot every node one at a time (NOT parallel — see the warning below)
ansible k3scluster -b -m ansible.builtin.reboot --forks=1

# Reboot just the workers
ansible k3sworkers -b -m ansible.builtin.reboot

# Run an arbitrary shell command as root on one specific host
ansible <hostname> -b -m shell -a "journalctl -u k3s -n 100 --no-pager"

# Gather full Ansible facts for one host (useful for debugging inventory vars)
ansible <hostname> -m setup
```

> ⚠️ **Never reboot the whole `k3scluster` group in parallel.** Longhorn's
> `instance-manager` pods and their PodDisruptionBudgets can make a fully-parallel drain
> or reboot fail outright. Reboot sequentially (`--forks=1`, or one host at a time), or
> drain nodes first — see the [kubectl cheat sheet](kubectl-cheatsheet.md#draining-nodes-one-by-one)
> for the drain-first version of this same operation. **Better yet, for an actual OS/kernel
> patch cycle, use the playbook below instead of hand-rolling drain+reboot** — plain
> `kubectl drain` hangs on this cluster's Longhorn/CNPG PDBs, see
> [Known Issues](known-issues.md#node-drain-blockers).

## Full cluster update (OS + kernel + k3s)

One cordon/drain/uncordon pass per node covering OS packages, the kernel, and the k3s
binary version together — see [docs/cluster-update-playbook.md](https://github.com/Coollision/homelab-config/blob/master/docs/cluster-update-playbook.md)
for the full design (why it's one pass, the Longhorn/CNPG drain-blocker handling, node
ordering). **Safe by default** — `node_update_dry_run` defaults to `true`, so a plain run only
reports what would change; nothing is touched until you pass `-e node_update_dry_run=false`.

```bash
cd ansible

# Report only (safe, default) — what packages/kernel/k3s would change, and any
# drain blockers found, on every node
ansible-playbook playbooks/cluster_update.yml

# Try it for real on one node first (recommended — pick the least critical node)
ansible-playbook playbooks/cluster_update.yml -e node_update_dry_run=false --limit <node-name>

# Real run, everything, whole cluster (master first, then workers — see docs)
ansible-playbook playbooks/cluster_update.yml -e node_update_dry_run=false

# OS/kernel packages only, skip the k3s version bump
ansible-playbook playbooks/cluster_update.yml -e node_update_dry_run=false -e node_update_upgrade_k3s=false

# k3s binary only, skip OS packages
ansible-playbook playbooks/cluster_update.yml -e node_update_dry_run=false -e node_update_patch_os=false

# Follow progress live: run it in the foreground in your own terminal (recommended for
# a real run) instead of asking Claude to background it — you get the full task-by-task
# ansible output as it happens. Add -v (or -vv) for full kubectl/apt command output.
ansible-playbook playbooks/cluster_update.yml -e node_update_dry_run=false -v
```

> ℹ️ Real runs of `cluster_update.yml`, `distro_upgrade.yml`, `vlan_trunk_setup.yml`,
> `vlan_trunk_teardown.yml` and `uplink_name.yml` take a cluster-side lock
> (ConfigMap `kube-system/homelab-node-maintenance-lock`), so they never run at once;
> `node_setup.yml` and `k3s_setup.yml` only check it (they refuse when the API is reachable and the
> lock exists). It is released only on success; after a failure check the nodes, then re-run with
> `-e maint_lock_break=true`. A Debian release upgrade is a separate playbook
> (`playbooks/distro_upgrade.yml --limit <node-name>`, dry-run by default); k3s bumps are never
> done inside its drain.

> ℹ️ After the uncordon and the settle wait, each node also gets `crictl rmi --prune` (unused images,
> the usual DiskPressure cause); switch off with `-e node_update_image_prune=false`.

> ⚠️ A stuck drain (bare pod with no controller) is refused, not forced, and the node is
> uncordoned again on failure, but only if this run cordoned it (a node that was already cordoned
> stays cordoned). Only pass `-e node_update_force_drain=true` once you've
> confirmed that pod is fine to lose permanently — see
> [Known Issues](known-issues.md#node-drain-blockers).

## Playbooks

```bash
# Full OS-level provisioning for all nodes (idempotent — safe to re-run)
ansible-playbook playbooks/node_setup.yml

# k3s install/join, in the correct order (pre → VLAN trunk → master → workers)
ansible-playbook playbooks/k3s_setup.yml

# Just the VLAN trunk interfaces, without touching k3s
ansible-playbook playbooks/vlan_trunk_setup.yml

# Add the ssh_public_key (from the private group_vars) to every host's authorized_keys.
# Other keys are kept; add -e setup_ssh_key_exclusive=true to drop them (rotation)
ansible-playbook playbooks/setup-ssh-keys.yml

# Inject/refresh DNS records for the cluster — ONLY once Technitium is already
# up and serving as the cluster's DNS, per the bootstrap order
ansible-playbook playbooks/dns.yml

# Limit any playbook to one host or group with --limit
ansible-playbook playbooks/node_setup.yml --limit <hostname>

# Dry-run any playbook (check mode — shows what WOULD change, changes nothing)
ansible-playbook playbooks/node_setup.yml --check --diff

# Full teardown of a node (removes k3s entirely — destructive). Both guards are mandatory:
# an explicit --limit AND the cluster name (k3s_cluster_name, default "k3s")
ansible-playbook playbooks/k3s_teardown.yml --limit <hostname> -e confirm_teardown=<cluster-name>

# Remove one node's DNS records (A/AAAA); --limit required
ansible-playbook playbooks/dns_teardown.yml --limit <hostname>
```

## Debugging playbook runs

```bash
# Verbose output (add more -v for more detail, up to -vvvv)
ansible-playbook playbooks/node_setup.yml -vv

# Step through a playbook task-by-task, confirming each one
ansible-playbook playbooks/node_setup.yml --step

# Start partway through a playbook (skip tasks already known-good)
ansible-playbook playbooks/node_setup.yml --start-at-task="<task name>"

# List every host a playbook would target, without running anything
ansible-playbook playbooks/node_setup.yml --list-hosts
```
