# Vault the place to store secrets

## initial setup

if you have not setup vault before you can use the following commands to get it up and running

```bash

<!-- TODO: Cleanup this readme -->
```bash


vault operator init

vault operator unseal

vault auth enable kubernetes

vault write auth/kubernetes/config token_reviewer_jwt="$(cat /var/run/secrets/kubernetes.io/serviceaccount/token)" kubernetes_host="https://$KUBERNETES_PORT_443_TCP_ADDR:443" kubernetes_ca_cert=@/var/run/secrets/kubernetes.io/serviceaccount/ca.crt issuer=https://kubernetes.default.svc


vault policy write argocd-vault-plugin-policy - << EOF
path "kv/data/*" {
 capabilities = ["read" ]
}
EOF

vault write auth/kubernetes/role/argocd-role bound_service_account_names=argocd-role bound_service_account_namespaces=argocd policies=argocd-vault-plugin-policy ttl=60m

```