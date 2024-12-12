# homelab-config

Setup of my homelab

## Seting up this homelab with argocd

### Install argo from scratch

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
