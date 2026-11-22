# Flannel IPAM lease leak after hard reboot

**Status (2026-11-22): root cause known, waiting on an upstream Multus fix; no automation on purpose.** After a reboot,
check for stale leases and run the manual fix below if there are many. Last cleanup: 2026-11-22 (138 stale leases freed
across four nodes, no pod affected).

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
| <node-1>      | <node-1-host>   | 10.42.0   |
| <node-2>      | <node-2-host>   | 10.42.1   |
| <node-3>      | <node-3-host>   | 10.42.2   |

No restart needed — flannel picks up the freed IPs immediately and pending pods
start getting assigned addresses within seconds.

## Root cause

Found 2026-11-22. The leaks happen at **node boot**, not during normal operation: on all four nodes every stale lease
was created before the node's last boot and none during the following weeks of uptime.

The CNI chain is Multus in thick mode, whose shim calls a daemon (a pod) over `/run/multus/multus.sock`. At boot
containerd tears down the pre-reboot pod sandboxes and sends CNI `DEL` for each, but the Multus daemon pod is not up yet,
so the shim logs `CmdDel (shim): CheckAPIReadyNow: Daemon not reachable over socketfile` and still reports success. The
runtime never retries, and the host-local IPAM lease (released only by a successful `DEL`) stays behind. The number of
those errors in the containerd log at boot matches the number of stale leases. A drain before the reboot avoids most of it,
but not DaemonSet pods, crashes or power loss.

Upstream: the Multus shim can report a successful `DEL` when the daemon is unavailable (pull request #1571 in
k8snetworkplumbingwg/multus-cni, open on 2026-11-22, makes the shim wait up to 60 s for the daemon and return the error so
the runtime retries). It is not in a release yet (v4.3.2 does not have it). We run v4.3.1, now pinned as `v4.3.1-thick`
(it was the floating `stable-thick` tag until 2026-11-22, which had silently moved on to 4.3.2 without being deployed and
was invisible to Renovate), so Renovate will propose new Multus releases.

**Follow-up:** when a Multus release contains that fix, bump the image (pin it), reboot one drained node and check that no
stale leases appear. Until then, with about 40 pods per node and 254 addresses, the leak is harmless.

## Long-term fixes to consider

- **Larger pod CIDR per node:** change `--cluster-cidr` to e.g. `10.42.0.0/16`
  with `--node-cidr-mask-size=23` to give each node 510 IPs instead of 254.
  Set in `ansible/roles/k3s_pre/templates/config.yaml.j2`.
- **Boot-time reconciliation DaemonSet:** a privileged init container or systemd
  service that on node startup removes any lease file whose IP has no corresponding
  running container (check via `crictl ps`).
