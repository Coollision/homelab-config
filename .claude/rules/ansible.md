---
paths:
  - "ansible/**"
  - "node-setup/**"
---

# Ansible / node setup

- Real inventory is private; edit `inventory_example/` for structure changes, never copy real hosts into tracked files.
- Idempotent roles only; run with `--check --diff` first and show the result before a real run.
- k3s node names differ from DNS/SSH names; confirm which you mean before targeting a host.
- Host changes that affect the cluster (reboots, k3s upgrade) follow `docs/cluster-update-playbook.md`: one cordon/drain/uncordon cycle per node (Longhorn/CNPG PDBs can block drains, see wiki known-issues `#node-drain-blockers`).
