# TODO — Ansible housekeeping

[← Back to Home](../Home.md) · Related: [Ansible](../00-setup/ansible.md) · [Distro upgrade](../00-setup/distro-upgrade.md)

> **Status (2026-10-30):** small cleanups noticed during the October update day. None of them block anything today;
> delete this page once they are done.

- [ ] **VLAN links carry IPv6 default routes (UniFi side cleanup).** The router advertises itself as the IPv6 default
      gateway on the IoT and Intern VLANs as well as the server VLAN, so a node with VLAN sub-interfaces gets three
      equal-metric IPv6 default routes. The kernel picked the IoT one after a reboot and outbound IPv6 (e.g. image
      pulls from quay.io) timed out for 20+ minutes on the node with the trunk (2026-10-30). Node-side fix is in the
      `vlan_trunk` role (`vlan_trunk_ra_default_route: false`, ifupdown path only, applied at runtime and via
      `post-up`; applied to the mini-PC trunk nodes on 2026-10-30, where all three defaults had the same metric and the
      IoT one black-holed outbound IPv6; the Pi is not exposed because NetworkManager gives its uplink route a lower
      metric). Proper fix: stop the router from advertising a default route
      (or lower its preference) on the IoT and Intern VLANs in UniFi, then re-check `ip -6 route show default` on both
      trunk nodes. The networkd and NetworkManager branches of the role do not handle this yet.
- [ ] **Keep Prometheus off the Raspberry Pi worker.** A long-range query (30 days of speedtest or UPS data) plus
      scrape load is too much for the Pi: on 2026-10-30 Prometheus was moved to a faster node by cordoning the Pi by
      hand. Make it permanent in the monitoring values (node affinity or anti-affinity on the Pi's arch/hostname label)
      so a drain of another node cannot reschedule it there.
- [ ] **Decide whether etcd needs an off-node backup (control-plane rebuild path is untested).** The `k3s_master`
      config only uses the k3s defaults for etcd snapshots (local disk of the control-plane node, default schedule and
      retention, no off-node copy). The documented recovery from a lost control plane is a full new cluster provision
      (`k3s_setup`) followed by a Longhorn restore; that path has been written down but never rehearsed. Open question:
      is an etcd snapshot worth having at all? Manifests come back from Git via ArgoCD and data from Longhorn backups,
      so the unique state in etcd is mostly Secrets that were created by hand and are not in Git. Either rehearse the
      rebuild once and list what is lost, or set an explicit snapshot schedule/retention plus an off-node target.
