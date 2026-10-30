# TODO — Ansible housekeeping

[← Back to Home](../Home.md) · Related: [Ansible](../00-setup/ansible.md) · [Distro upgrade](../00-setup/distro-upgrade.md)

> **Status (2026-10-30):** small cleanups noticed during the October update day. None of them block anything today;
> delete this page once they are done.

- [ ] **Decide whether etcd needs an off-node backup (control-plane rebuild path is untested).** The `k3s_master`
      config only uses the k3s defaults for etcd snapshots (local disk of the control-plane node, default schedule and
      retention, no off-node copy). The documented recovery from a lost control plane is a full new cluster provision
      (`k3s_setup`) followed by a Longhorn restore; that path has been written down but never rehearsed. Open question:
      is an etcd snapshot worth having at all? Manifests come back from Git via ArgoCD and data from Longhorn backups,
      so the unique state in etcd is mostly Secrets that were created by hand and are not in Git. Either rehearse the
      rebuild once and list what is lost, or set an explicit snapshot schedule/retention plus an off-node target.
