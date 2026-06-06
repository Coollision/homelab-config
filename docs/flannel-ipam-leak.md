# Flannel IPAM lease leak after hard reboot

**TODO: automate this or fix the root cause.**

## What happens

After a hard reboot or crash, pods that terminated without a clean CNI teardown
leave stale IP lease files behind in `/var/lib/cni/networks/cbr0/` on each node.
Each node only has a `/24` (254 IPs). These leaks fill it up, causing new pods to
fail networking setup with:

```text
flannel: failed to allocate for range 0: no IP addresses available in range set
```

**Symptoms:**

- Many pods stuck in `ContainerCreating` after a reboot
- `kubectl describe pod <name>` shows flannel IP exhaustion errors
- `kubectl get nodes` shows all nodes `Ready` (the node itself is fine)
- Cascades into Longhorn CSI not registering → PVC-backed pods also stuck

This was observed on 2026-06-05 after a cluster restart. All three nodes were
affected: green had 219 stale leases, blue had 191, silver had 65.

## Fix (manual)

Run for each node. Replace `NODE_NAME` (kubectl node name), `IP_PREFIX` (the
node's pod CIDR prefix, see table below), and `NODE_HOST` (SSH hostname).

```bash
ACTIVE=$(kubectl get pods -A -o json \
  | jq -r --arg node "NODE_NAME" --arg prefix "IP_PREFIX." \
    '.items[] | select(.spec.nodeName==$node) | select(.status.podIP | startswith($prefix)) | .status.podIP' \
  | tr '\n' ' ')

ssh homelab@NODE_HOST "
  KEEP='$ACTIVE'
  DELETED=0
  for f in \$(sudo ls /var/lib/cni/networks/cbr0/); do
    [[ \"\$f\" == \"lock\" ]] && continue
    if echo \" \$KEEP \" | grep -q \" \$f \"; then :
    else sudo rm /var/lib/cni/networks/cbr0/\"\$f\"; DELETED=\$((DELETED+1)); fi
  done
  echo \"Deleted \$DELETED stale leases, remaining: \$(sudo ls /var/lib/cni/networks/cbr0/ | wc -l)\"
"
```

Node reference:

| kubectl name  | SSH host         | IP prefix |
|---------------|------------------|-----------|
| master-green  | node-green.pi    | 10.42.0   |
| worker-blue   | node-blue.pi     | 10.42.1   |
| worker-silver | node-silver.pi   | 10.42.2   |

No restart needed — flannel picks up the freed IPs immediately and pending pods
start getting assigned addresses within seconds.

## Root cause

Flannel does not clean up IPAM leases if the CNI `DEL` call is skipped (hard
reboot, OOMKill of the container runtime, etc.). The lease files persist on disk
and flannel counts them as in-use on next startup.

## Long-term fixes to consider

- **Larger pod CIDR per node:** change `--cluster-cidr` to e.g. `10.42.0.0/16`
  with `--node-cidr-mask-size=23` to give each node 510 IPs instead of 254.
  Set in `ansible/roles/k3s_master/templates/config.j2`.
- **Boot-time reconciliation DaemonSet:** a privileged init container or systemd
  service that on node startup removes any lease file whose IP has no corresponding
  running container (check via `crictl ps`).
