# Secrets — Vault

[← Back to Home](../Home.md) · Platform: [Databases](databases.md) · [GitOps structure](../00-setup/gitops-structure.md)

HashiCorp Vault (`system/secrets/vault`, namespace `secrets`) is the **root of trust for
every secret in this repo.** No password, API key, or credential is ever committed to
Git — instead, `values.yaml` files across the whole repo are full of placeholders like:

```yaml
password: <secret:kv/data/smarthome/n8n~password>
```

At render time, ArgoCD's `argocd-lovely-plugin` Config Management Plugin runs each
chart's `helm template`/`kustomize build` output through a sidecar called **`avp`**
(`ghcr.io/crumbhole/lovely-vault-ver`), which resolves every `<secret:...>` placeholder
against Vault's KV v2 store before the manifest is ever applied. This means: **if Vault
is down or sealed, essentially nothing in the cluster can be freshly rendered or synced**
— it's a hard dependency for the entire GitOps pipeline, not just for apps that obviously
need a secret.

## Auto-unseal, without resident credentials

Vault keeps its **Shamir seal** (5 shares, threshold 3) — no KMS/transit auto-unseal.
Instead, a sidecar container named `unsealer` (`curlimages/curl`) shares the Vault pod and
polls `/v1/sys/seal-status` every 30 seconds. When it sees Vault sealed, it fetches the
unseal keys **on demand** from a Kubernetes Secret via the K8s API (using the pod's own
ServiceAccount token, RBAC-scoped to `get` *only* that one Secret), submits them, and
never persists them anywhere — not in its env, not on disk.

The three unseal keys live in a Secret (`vault-unseal-keys`) that is **manually created
and never committed to Git.** If it's ever lost, it has to be recreated from the original
`vault operator init` output:

```bash
kubectl create secret generic vault-unseal-keys -n secrets \
  --from-literal=VAULT_UNSEAL_KEY_1=... \
  --from-literal=VAULT_UNSEAL_KEY_2=... \
  --from-literal=VAULT_UNSEAL_KEY_3=...
```

Two gotchas worth knowing before touching this again:
1. **`envFrom` is unreliable in the `secrets` namespace** — Kubernetes' automatic
   service-link env vars for the `vault`/`vaultwarden` Services collide with `VAULT_*`
   names and silently corrupt anything injected via `envFrom`. Always use explicit
   `valueFrom` here.
2. **The Kubernetes API returns pretty-printed JSON** (a space after every colon) — a
   naive compact-JSON grep will silently match nothing.

The Vault StatefulSet uses `OnDelete` update strategy, so a Renovate image bump does
**not** roll the pod automatically — after a version bump you must
`kubectl delete pod -n secrets vault-0` yourself; the sidecar re-unseals it on the way
back up.

## Argo ↔ Vault trust

Vault's own `readme.md` documents the bootstrap: `vault operator init` → unseal →
`vault login` → enable the Kubernetes auth method → create policy `argocd-policy` (read
`kv/data/*`) → bind it to role `argocd-role` scoped to the `argocd` namespace's
ServiceAccount → enable KV v2 at path `kv`. This role is what `argocd/values.yaml`
references (`vault.role: argocd-role`) to let the `avp` sidecar authenticate.

**A known landmine for future Vault upgrades:** the `lovely-vault-ver` plugin has a
JWT-audience incompatibility with Vault ≥1.21 — the current workaround is a Vault role
with **no** `audience` field set, which only works cleanly with the Vault 1.20.4-era auth
flow. Don't blindly bump Vault's major version without checking this first.

## History

Vault's auto-unseal design went through three iterations in a single day
(2026-07-04): a CronJob submitting Shamir keys → refactored into an in-pod sidecar →
refactored again to fetch keys on-demand instead of holding them resident → then a
same-day fix for the pretty-printed-JSON parsing gotcha. The end state (described above)
was a deliberate choice to avoid any resident credential, not the first design tried.
