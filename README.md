# homelab-config

Setup of my homelab

## Seting up this homelab with argocd

### Install argo from scratch

#### we depend on vault so make sure that is installed first

run `helm install vault system/secrets/vault -n secrets --create-namespace`

make sure its unsealed and ready to go (see the vault readme for more info)

#### install argo

since we are managing argo with argo we need to comment out both the \*-app files first

run `helm install argocd argocd -n argocd --create-namespace`

uncomment the \*-app files

run `helm upgrade argocd argocd -n argocd`

argo should be up and running now and managing itself

for more info on argo see the argo readme


