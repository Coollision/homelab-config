# Ansible: node + k3s provisioning

Turns a preseeded Debian / Raspberry Pi OS box into a joined k3s node. Full docs:
[`wiki/00-setup/ansible.md`](../wiki/00-setup/ansible.md),
[`wiki/00-setup/bootstrap.md`](../wiki/00-setup/bootstrap.md),
[`wiki/05-operations/ansible-cheatsheet.md`](../wiki/05-operations/ansible-cheatsheet.md),
upgrades: [`docs/cluster-update-playbook.md`](../docs/cluster-update-playbook.md).

## Layout

| Path | What |
|---|---|
| `playbooks/` | entry points (`node_setup`, `k3s_setup`, `dns`, `cluster_update`, `distro_upgrade`, teardowns, ...) |
| `roles/` | one role per concern; `k3s_pre/templates/config.yaml.j2` is the single k3s config template for servers and agents |
| `inventory_example/` | structure of the private inventory (documentation IPs, placeholder domain) |
| `inventory/` | the real inventory, **gitignored** (public repo) |
| `requirements.yml` | pinned collections |

## Run

```sh
cd ansible
ansible-galaxy collection install -r requirements.yml
cp -r inventory_example inventory   # first time only, then fill in real values
ansible-playbook playbooks/k3s_setup.yml --check --diff --limit <node>
ansible-playbook playbooks/k3s_setup.yml --limit <node>
```

Destructive playbooks need explicit guards:

```sh
ansible-playbook playbooks/k3s_teardown.yml --limit <node> -e confirm_teardown=<k3s_cluster_name>
ansible-playbook playbooks/dns_teardown.yml --limit <node>
```

## Required private inventory vars

| Var | Where | Notes |
|---|---|---|
| `k3s_version` | `group_vars/k3scluster.yml` | e.g. `v1.36.3+k3s1`; no default. `k3s_setup` refuses a downgrade unless `-e k3s_allow_downgrade=true` |
| `k3s_primary_master`, `k3s_master_domain_name`, `k3s_cluster_name` | `group_vars/k3smaster.yml` | |
| `host_ip_address` (+ optional `host_ipv6_address`), `k3s_node_name` | `host_vars/<host>.yml` | node IP is also the metrics bind address |
| `longhorn_disk_device` | `host_vars` (masters with a Longhorn disk) | use `/dev/disk/by-id/...` or `/dev/disk/by-partuuid/...`; mounted by UUID |
| `longhorn_disk_allow_format` | `host_vars` | default `false`; must be `true` to format an empty disk |
| `ssh_public_key` | `group_vars/nodes.yml` | required by `setup-ssh-keys.yml`; `setup_ssh_key_exclusive: true` to drop other keys |
| `dns_key_name`, `dns_key_secret`, `dns_server_ip`, `dns_zone_name` | `group_vars` | no defaults; keep the TSIG secret out of git |
| `k3s_registries` | `group_vars/k3scluster.yml` | `{}` for none |

Optional per host: `k3s_node_label`, `k3s_node_taint` (servers and agents),
`k3s_node_status_update_frequency`, `k3s_kube_reserved`, `k3s_system_reserved`, `k3s_max_pods`.
