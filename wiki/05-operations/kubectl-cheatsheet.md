# kubectl / cluster cheat sheet

[← Back to Home](../Home.md) · See also: [Ansible cheat sheet](ansible-cheatsheet.md) · [Known Issues](known-issues.md)

`kubectl` runs directly from the operator's laptop against this cluster's kubeconfig —
never through an SSH hop into a node (see
[Known Issues](known-issues.md#kubectl-runs-locally)).

## Node maintenance

### Draining all nodes one by one {#draining-nodes-one-by-one}

Never drain the whole cluster in parallel — Longhorn's `instance-manager`
PodDisruptionBudgets can make a parallel drain fail outright:

```bash
for node in $(kubectl get nodes -o name); do
  kubectl drain "$node" --ignore-daemonsets --delete-emptydir-data --force
done
```

Only if you're intentionally shutting the whole cluster down and need to bypass
PodDisruptionBudgets as a last resort:

```bash
for node in $(kubectl get nodes -o name); do
  kubectl drain "$node" --ignore-daemonsets --delete-emptydir-data --force --disable-eviction
done
```

### Uncordon everything at once

```bash
kubectl get nodes -o name | xargs -n 1 -P 0 -I {} kubectl uncordon {}
```

### Reboot every node via Ansible instead of draining manually

```bash
cd ansible && ansible k3scluster -b -m ansible.builtin.reboot --forks=1
```

## Cleaning up crashy pods

Restart only pods that look safe to auto-recreate (running, controller-owned, restarted
at least once) — standalone pods are skipped on purpose since recreating them may lose
data:

```bash
kubectl get pods -A -o json \
  | jq -r '
    .items[]
    | select(.status.phase == "Running")
    | select(.metadata.annotations["kubernetes.io/config.mirror"] | not)
    | select(any(.status.containerStatuses[]?; .restartCount >= 1))
    | . as $pod
    | (($pod.metadata.ownerReferences // []) | map(select(.controller == true)) | first) as $owner
    | select($owner != null)
    | select($owner.kind == "ReplicaSet" or $owner.kind == "StatefulSet" or $owner.kind == "DaemonSet" or $owner.kind == "Job")
    | [$pod.metadata.namespace, $pod.metadata.name, $owner.kind, $owner.name] | @tsv
  ' \
  | while IFS=$'\t' read -r ns pod owner_kind owner_name; do
      echo "Deleting $ns/$pod (owner: $owner_kind/$owner_name)"
      kubectl -n "$ns" delete pod "$pod"
    done
```

## ArgoCD

```bash
# Force-refresh an Application's rendered manifest (first thing to try if
# a synced app doesn't reflect a values.yaml change — see Known Issues)
kubectl -n argocd annotate application <app> argocd.argoproj.io/refresh=hard --overwrite

# If that's not enough, bounce the repo-server to fully clear its render cache
kubectl rollout restart deploy/argocd-repo-server -n argocd

# List every Application and its sync/health status at a glance
kubectl get applications -n argocd -o custom-columns=\
NAME:.metadata.name,SYNC:.status.sync.status,HEALTH:.status.health.status

# Strip finalizers from every Application (last resort when one is stuck deleting)
kubectl get applications -n argocd -o name | xargs -I{} \
  kubectl patch {} -n argocd --type merge -p '{"metadata":{"finalizers":null}}'

# Pause automated sync on one app (needed BEFORE any manual scale-down —
# selfHeal reverts a manual scale in seconds otherwise)
kubectl patch application <app> -n argocd --type merge \
  -p '{"spec":{"syncPolicy":null}}'

# Nudge an ApplicationSet to re-reconcile immediately instead of waiting ~3 min
kubectl annotate applicationset <name> -n argocd force-reconcile=$(date +%s) --overwrite
```

## Longhorn

```bash
# Never kubectl-patch a Volume's spec directly — it's GitOps-managed and
# selfHeal reverts it in ~4s. Change replicas: in the app's values.yaml instead.

# Unblock deleting a protected volume (only when you actually mean to delete it)
kubectl label volume <vol> -n <storage-ns> protect-

# Check a volume's attach state / replica health
kubectl get volumes.longhorn.io -n <storage-ns> <vol> -o yaml
```

## Sablier scale-to-zero

```bash
# Custom repo script: shows asleep/awake state + time-awake + time-to-sleep per app
./scripts/sablier-status.sh

# Regenerate the ArgoCD ignoreDifferences block for scale-to-zero apps after
# adding/removing a scaleToZero.enabled flag in a chart
./scripts/gen-scale-to-zero-ignores.sh

# Manually wake an app right now (send it a real request through its ingress —
# there's no kubectl-level "wake" command; Sablier only reacts to real traffic)
curl -sk https://<app-host>/ >/dev/null
```

## Node image cleanup (disk pressure)

```bash
# From the laptop, targeting a node via SSH for the one host-level step:
ssh <ssh-target-for-node> 'sudo k3s crictl images'      # see what's resident
ssh <ssh-target-for-node> 'sudo k3s crictl rmi --prune' # remove unused images

# df lags behind the prune — re-check a few seconds later, not immediately
ssh <ssh-target-for-node> 'df -h /'
```

## Quick health sweep

```bash
# Anything not Running/Completed, across the whole cluster
kubectl get pods -A --field-selector=status.phase!=Running,status.phase!=Succeeded

# Recent warning events, newest first
kubectl get events -A --field-selector type=Warning --sort-by=.lastTimestamp

# Node conditions (DiskPressure, MemoryPressure, etc.)
kubectl get nodes -o json | jq -r '.items[] | .metadata.name as $n | .status.conditions[] | select(.status=="True" and .type!="Ready") | "\($n): \(.type)"'
```
