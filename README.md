# homelab-config

Setup of my homelab

## Seting up this homelab with argocd

### Install argo from scratch

note; always run in the folder an helm dependency update , otherwise the charts will fully work and to funky things

#### we depend on storage so make sure that is installed first

(make sure to replace the secrets with your own values, you can use the vault values later)

run `helm install storage system/kube-system/storage -n kube-system`

#### we then depend on vault so make sure that is installed first

run `helm install vault system/secrets/vault -n secrets --create-namespace`

make sure its unsealed and ready to go (see the vault readme for more info)

#### install argo

since we are managing argo with argo we need to comment out both the \*-app files first

run `helm install argocd argocd -n argocd --create-namespace`

uncomment the \*-app files

run `helm upgrade argocd argocd -n argocd`

argo should be up and running now and managing itself

if it all seems to lock, traefik might be causing a issue, it depends on servicemonitor, and montioring is not yet installed, while monitoring wants an ingress, so traefik is not yet installed, so we have a chicken and egg problem, so you need to do a commit and disable traefik monitoring first

for more info on argo see the argo readme

## Handy commands

### Drain all nodes one by one

Handy before planned downtime.

Draining every node in parallel can fail on PodDisruptionBudgets, for example with Longhorn `instance-manager` pods. Drain the cluster sequentially instead:

```bash
for node in $(kubectl get nodes -o name); do
	kubectl drain "$node" --ignore-daemonsets --delete-emptydir-data --force
done
```

If you are intentionally shutting down the whole cluster and need to bypass PodDisruptionBudgets, only use this as a last resort:

```bash
for node in $(kubectl get nodes -o name); do
	kubectl drain "$node" --ignore-daemonsets --delete-emptydir-data --force --disable-eviction
done
```

### Uncordon all nodes at the same time

```bash
kubectl get nodes -o name | xargs -n 1 -P 0 -I {} kubectl uncordon {}
```

### Reboot all cluster nodes with Ansible

Run this from the repository root to reboot every node in the `k3scluster` inventory group:

```bash
cd ansible && ansible k3scluster -b -m ansible.builtin.reboot
```

### Restart all pods with restart count higher than 2

This only deletes pods that look safe to recreate automatically:

- restart count higher than or equal to `1`
- currently `Running`
- controlled by a `ReplicaSet`, `StatefulSet`, `DaemonSet`, or `Job`
- not a static mirror pod

Standalone pods are skipped on purpose.

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
			kubectl delete pod -n "$ns" "$pod"
		done
```
