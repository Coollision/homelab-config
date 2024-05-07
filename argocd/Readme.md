# argo setup

## Install argo from scratch => see main readme

## argo access

when first installing argo you need to get the initial password and set it to something you like

you can use this script to do that:

```bash
domain=example.com
newRootPass=examplePass

initPass=$(kubectl -n argocd get secret argocd-initial-admin-secret -o jsonpath="{.data.password}" | base64 -d)
argocd login argocd.$domain --username admin --password $initPass
argocd account update-password --current-password $initPass --new-password ${newRootPass}
```
