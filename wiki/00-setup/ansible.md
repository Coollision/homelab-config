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
| `node_common` | all nodes | hostname, locale/timezone, aliases, `apt upgrade`, baseline packages (`nfs-common`, `open-iscsi`, `cryptsetup`, …), inotify limits, NFS kernel modules, includes `journald_config` |
| `rpi_common` | Pis only | Pi-specific tweaks, temp/voltage aliases via `vcgencmd`, disables swap, tunes UDP buffers (in `/etc/sysctl.d`, as is the inotify limit set by `node_common`: Debian 13 ignores `/etc/sysctl.conf`). The `cmdline.txt` `replace` flag really replaces existing keys now (it used to be add-only), so check a Pi with `--check --diff` before a real run |
| `setup_ssh_key` | all nodes (own playbook) | adds `ssh_public_key` to `authorized_keys`; asserts the key is non-empty, replacing all other keys is opt-in (`-e setup_ssh_key_exclusive=true`), no sshd restart. (The old `python` and `sudoers` roles were removed; the preseed installs `python3` and `sudo` and grants the sudo rights) |
| `journald_config` | all nodes | templates `journald.conf` with retention caps, vacuums logs |
| `uplink_name` | all cluster nodes | pins the primary NIC to `uplink0` (a `.link` file matched by permanent MAC) and rewrites flannel, the VLAN trunk parent and the DHCP stanza / NetworkManager profile to match; the old config stays as a fallback until verified. Staged per node, applied by a reboot (`cluster_update.yml -e node_update_force_reboot=true`); phases `stage` / `verify` / `cleanup`. Runbook: [uplink name](../06-todo/uplink-name.md) |
| `vlan_trunk` / `vlan_trunk_teardown` | nodes needing VLAN legs | builds VLAN sub-interfaces (systemd-networkd or ifupdown, auto-detected), plus, only when `vlan_trunk_host_ipv6` is true, the host-level IPv6 RA-acceptance sysctl for Thread (`vlan_trunk_enable_thread_ipv6`); by default (`vlan_trunk_host_ipv6: false`) the node takes no IPv6 on the VLAN sub-interfaces (`accept_ra=0`, `autoconf=0`, global addresses flushed; only pods use the VLANs via macvlan; ifupdown via `post-up`, NetworkManager via `ipv6.method disabled` on the VLAN connections, not needed on systemd-networkd); the per-interface IPv6 sysctls are `post-up` lines on ifupdown hosts. `vlan_trunk_teardown` also handles NetworkManager hosts. Both playbooks take the maintenance lock (see below) |
| `k3s_pre` | all cluster nodes | downloads the k3s binary pinned by `k3s_version` (asserted; no default in the repo, see below), loads `dm_crypt` (needed by Longhorn), templates the private registry mirror config and the single k3s `config.yaml` (`roles/k3s_pre/templates/config.yaml.j2`, shared by primary, secondary and agent; workers accept an optional `k3s_node_taint`) |
| `k3s_master` | control-plane node(s) | mounts the Longhorn disk, installs k3s server (primary gets no token; additional masters fetch the primary's join token); removes the old etcd defrag timer from existing masters (no longer installed). Longhorn disk (`longhorn_disk_device`): a `/dev/disk/by-id` or `by-partuuid` path is preferred, mounted by UUID, and only formatted with `longhorn_disk_allow_format: true` |
| `k3s_worker` | worker nodes | fetches the join token from the primary, installs `k3s-node.service` |
| `k3s_teardown` | — | full reset: stops services, kills lingering containerd shims, unmounts, deletes data dirs, CNI/flannel links, iptables rules and `/etc/rancher/node`. Needs `--limit` and `-e confirm_teardown=<cluster name>` |
| `node_update` / `cluster_wait_stable` | all cluster nodes | one cordon → drain → uncordon pass per node covering OS packages and the k3s binary bump; on nodes with an Intel iGPU it also installs i915 firmware, pulls the backports kernel if the installed one lacks the GPU's PCI ID (bookworm/x86_64 only; on any other release the play fails and the fix is decided by hand), and asserts the render node, GuC/HuC firmware and a container `vainfo` before uncordon (`node_update_intel_gpu_autofix`); `cluster_wait_stable` is the barrier between nodes (pods settled, every Longhorn volume Healthy) |
| `dns` / `dns_teardown` | — | RFC 2136 dynamic DNS updates (TSIG-signed) against Technitium — node A/AAAA records, `k3s.<zone>`, the ingress wildcard. `dns_teardown` (via `dns_teardown.yml --limit <node>`) removes per-node records; `k3s_teardown.yml` only rewrites the control-plane record to the remaining masters (masters only) |

## Playbooks

```text
ansible/playbooks/
  node_setup.yml         # node_common / rpi_common; soft maintenance-lock check, apt upgrade only while k3s is not yet running
  cluster_update.yml      # rolling OS + k3s upgrade, dry-run by default (-e node_update_dry_run=false to apply)
  distro_upgrade.yml      # in-place Debian release upgrade, one node per run (--limit), dry-run by default; shares a cluster-side lock with cluster_update.yml (runbook: distro-upgrade.md)
  uplink_name.yml         # pin the primary NIC name to uplink0, one node per run (--limit), then reboot via cluster_update.yml -e node_update_force_reboot=true (design: ../06-todo/uplink-name.md)
  gpu_check.yml           # no-change iGPU smoke test (render node, GuC/HuC firmware, container vainfo), run after kernel updates
  k3s_setup.yml           # k3s_pre → vlan_trunk → k3s_master → k3s_worker; soft lock check, refuses a k3s downgrade (-e k3s_allow_downgrade=true), restarts k3s only via handlers (binary/config changed), workers serial: 1
  dns.yml                 # DNS record injection (run AFTER Technitium is live)
  setup-ssh-keys.yml      # add the ssh key (-e setup_ssh_key_exclusive=true to drop the others)
  vlan_trunk_setup.yml / vlan_trunk_teardown.yml
  dns_teardown.yml        # remove one node's DNS records (--limit required)
  k3s_teardown.yml        # destructive; --limit AND -e confirm_teardown=<cluster name>
```

## Running things

See the [Ansible cheat sheet](../05-operations/ansible-cheatsheet.md) for copy-pasteable
ad-hoc commands (uptime checks, rolling reboots, re-running a single role against one
host, etc). Debian major upgrades: [Distro upgrade runbook](distro-upgrade.md).

## k3s version, config and metrics {#k3s-config}

- `k3s_version` is the single version variable. It lives in the private `group_vars` (no default
  in the repo); `k3s_pre` asserts it. `cluster_update.yml` upgrades to it, `k3s_setup.yml`
  installs it on a replacement node. `k3s_setup.yml` refuses to downgrade unless
  `-e k3s_allow_downgrade=true`.
- The metrics endpoints (kube-proxy, controller-manager, scheduler, etcd) bind to the node IP
  instead of `0.0.0.0` (override with `k3s_metrics_bind_address`). After rolling this out,
  verify the Prometheus scrape targets for those four are still up.

## Maintenance lock {#maintenance-lock}

`cluster_update.yml`, `distro_upgrade.yml`, `vlan_trunk_setup.yml`, `vlan_trunk_teardown.yml` and
`uplink_name.yml` take the cluster-side lock (ConfigMap `kube-system/homelab-node-maintenance-lock`);
only an `AlreadyExists` error counts as "held". `node_setup.yml` and `k3s_setup.yml` do a soft check:
they refuse only when the API is reachable and the lock exists. Break a stale lock with
`-e maint_lock_break=true`.

## Role variable names {#role-variables}

Every operator-facing role variable is prefixed with its role name (`node_update_dry_run`,
`node_update_distro_*`, `cluster_wait_stable_stabilize_*`, `setup_ssh_key_exclusive`,
`uplink_name_phase`, ...). An old unprefixed name in the private inventory or an `-e` flag is
silently ignored (the role default wins), so grep the private inventory for leftovers.
