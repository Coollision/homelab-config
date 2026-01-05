# Vault - Secret Storage

Vault is deployed as part of the bootstrap sequence and provides secret management for ArgoCD via the lovely-vault-ver plugin.

## New Install / Disaster Recovery Setup

### Step 1: Initialize Vault

After Vault pod is running, exec into it and initialize:

```bash
kubectl exec -it -n secrets vault-0 -- vault operator init
```

Save the unseal keys and root token securely (not in Git).

### Step 2: Unseal Vault

Using the unseal keys from Step 1:

```bash
kubectl exec -it -n secrets vault-0 -- vault operator unseal
```

Repeat the unseal command 3 times with different keys (K of N threshold).

### Step 3: Login to Vault

```bash
kubectl exec -it -n secrets vault-0 -- vault login
```

Enter the root token from Step 1.

### Step 4: Enable Kubernetes Auth Method

```bash
kubectl exec -it -n secrets vault-0 -- vault auth enable kubernetes
```

### Step 5: Configure Kubernetes Auth

```bash
kubectl exec -it -n secrets vault-0 -- vault write auth/kubernetes/config \
    kubernetes_host=https://kubernetes.default.svc/
```

### Step 6: Create ArgoCD Policy

```bash
kubectl exec -it -n secrets vault-0 -- vault policy write argocd-policy - << 'EOF'
path "kv/data/*" {
  capabilities = ["read"]
}
path "kv/metadata/*" {
  capabilities = ["list"]
}
EOF
```

### Step 7: Create Kubernetes Auth Role for ArgoCD

```bash
kubectl exec -it -n secrets vault-0 -- vault write auth/kubernetes/role/argocd-role \
  bound_service_account_names=argocd-role \
  bound_service_account_namespaces=argocd \
  policies=argocd-policy \
  ttl=1h
```

### Step 8: Create Secret Engine (KV v2)

```bash
kubectl exec -it -n secrets vault-0 -- vault secrets enable -version=2 -path=kv kv
```

### Step 9: Verify ArgoCD Authentication

```bash
# Check that repo-server can authenticate
kubectl logs -n argocd -l app.kubernetes.io/name=argocd-repo-server | grep -E "(Successfully authenticated|invalid audience|permission denied)"
```

Look for "Successfully authenticated" messages confirming Vault auth is working.

## Important Implementation Notes

### Service Account Binding

**Critical:** The Vault role must reference the actual K8s service account name:

- `bound_service_account_names=argocd-role` (the actual K8s service account)
- NOT `argocd-repo-server` (the role name in Vault)
- `bound_service_account_namespaces=argocd` (must match where ArgoCD is deployed)

### lovely-vault-ver Plugin Compatibility

**Known Issue:** `ghcr.io/crumbhole/lovely-vault-ver:1.2.2` has JWT audience validation issues with Vault 1.21+. K3s generates JWT tokens with multiple audiences that can cause "invalid audience" errors.

**Current Workaround (Vault 1.20.4):**

- The ArgoCD Vault role is created **WITHOUT** an explicit `audience` field
- This allows lovely-vault-ver to work correctly
- Vault will warn: "Role argocd-role does not have an audience. In Vault v1.21+, specifying an audience on roles will be required."

**Future Vault Upgrades (1.21+):**

When upgrading Vault past 1.21, you must choose one of:

1. Upgrade lovely-vault-ver to a version that handles multiple audiences
2. Switch to the official ArgoCD Vault Plugin (AVP) maintained by HashiCorp
3. Explicitly configure audience matching: `audience="https://kubernetes.default.svc.cluster.local,k3s"`

### Restoring from Backup

If restoring Vault data from a backup:

1. Delete existing Vault pod to force recreation
2. Once new pod is running, unseal it (Step 2 above)
3. Restore your backed-up secrets to the KV store
4. Restart ArgoCD repo-server pods for authentication to take effect

```bash
kubectl delete pod -n secrets vault-0
kubectl rollout restart deployment -n argocd argocd-repo-server
```

## Troubleshooting

### Verify ArgoCD Authentication Works

Check repo-server logs for successful Vault authentication:

```bash
kubectl logs -n argocd -l app.kubernetes.io/name=argocd-repo-server | grep -E "(Successfully authenticated|invalid audience|permission denied)"
```

Look for "Successfully authenticated" messages. If you see errors, verify:

1. Vault is unsealed: `kubectl get pod -n secrets`
2. Kubernetes auth is configured: `kubectl exec -it -n secrets vault-0 -- vault auth list`
3. ArgoCD policy exists: `kubectl exec -it -n secrets vault-0 -- vault policy list`
4. ArgoCD role is configured: `kubectl exec -it -n secrets vault-0 -- vault read auth/kubernetes/role/argocd-role`

### Common Issues

**"invalid audience" errors:**

- Remove the `audience` field from the Vault role (lovely-vault-ver incompatibility)
- Run Step 7 again without audience parameter

**"permission denied" errors:**

- Verify ArgoCD policy exists: `kubectl exec -it -n secrets vault-0 -- vault policy list | grep argocd-policy`
- Check policy permissions: `kubectl exec -it -n secrets vault-0 -- vault policy read argocd-policy`

**"could not authenticate" errors:**

- Verify service account name matches exactly: `kubectl get sa -n argocd argocd-role`
- Check role binding: `kubectl exec -it -n secrets vault-0 -- vault read auth/kubernetes/role/argocd-role`

## ⚠️ Important Notes

**Service Account Name:** The Vault role MUST use `argocd-role` (the actual K8s service account), not `argocd-repo-server`.

**Plugin Compatibility:** lovely-vault-ver 1.2.2 doesn't support Vault 1.21+ audience validation. The workaround is to create roles WITHOUT an audience field. When Vault is upgraded past 1.21, you'll need either a newer plugin version or the official AVP.

**Secrets Location:** Store secrets at `kv/data/path/to/secret` in Vault. Reference in ArgoCD values with: `<secret:kv/path/to/secret~key>`
