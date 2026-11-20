# cert-push — one certificate source for Traefik, the NAS and the UniFi console

[← Back to Home](../Home.md) · Platform: [Certificates](certificates.md) · [Secrets — Vault](secrets-vault.md) · [Networking](networking.md) · [Observability](observability.md)

**cert-manager is the only thing that talks to Let's Encrypt.** A daily in-cluster job (`cert-push`) takes the certificate
cert-manager already issues for Traefik and pushes it to the Synology NAS (DSM web API) and to the UniFi console (HTTPS API).
After every run it connects to the devices and checks which certificate they actually serve, and the Job fails (and an alert
fires) when anything is off. It can be run by hand at any time. A side effect is that port 80 no longer has to be reachable from
the internet for certificate renewal.

## Why this exists {#cert-push-why}

- DSM's built-in Let's Encrypt client only supports **HTTP-01**, so it needs port 80 open to the internet. It also renews all
  names of a multi-name certificate in one go, so **one unreachable name fails the whole renewal**. This already let a multi-name
  certificate expire once (a Cloudflare-proxied name answered 522 on port 80, the failure is not logged, the only way to see it
  is `syno-letsencrypt renew-all -vv`). See [known issues](../05-operations/known-issues.md).
- The UniFi console served a Let's Encrypt wildcard that had been **uploaded by hand** and was renewed by nothing.
- The cluster already solved the hard part: cert-manager issues a wildcard (apex + `*.<internal-domain>`) with a Cloudflare
  **DNS-01** challenge, which needs no inbound ports. The only missing piece was delivery to the two devices.

## How it works {#cert-push-how}

```text
cloudflare-issuer (DNS-01)
   └─ Certificate wildcard (ns default, also feeds Traefik)
          │  Secret mounted read-only into the job (same namespace, so no RBAC)
          ▼
   CronJob cert-push (daily 04:17)        CronJob cert-push-force (suspended, created by hand, FORCE=1)
     preflight  source key matches cert, renewal not overdue (computed from tls.crt dates)
     per target (nas, unifi), one session each:
       probe    which certificate does the device serve right now? (TLS handshake to the device IP, name via SNI)
       compare  served == source for every probe?  yes -> "up-to-date", no login, done
       push     login -> import/create -> activate -> logout (dedicated account, credentials from Vault)
       verify   probe again (retries ~2 min), served must equal the source, else exit 1
```

The job is stateless: what is on the device is the state. Each device is compared in the way that suits it.

| | NAS (DSM) | UniFi console |
| --- | --- | --- |
| API | DSM web API: `SYNO.API.Auth`, `SYNO.Core.Certificate` (import, list) | UniFi OS: `/api/auth/login`, `/api/userCertificates` (undocumented, the console's own UI API) |
| Managed object | one DSM certificate whose description is `cert-push <first 12 hex of sha256>` | one user certificate named `cert-push <first 8 hex of sha1>` |
| New certificate | **import under the same ID**: DSM keeps every service binding and restarts what is bound to it. First run has no ID and creates an unbound certificate. `as_default` is **only sent when the certificate already is the default**: DSM treats a present `as_default` as true whatever its value, so sending `false` made the first import the default (2026-11-15, see below). The job fails if an import ever changes the default | **create a new certificate, activate it, verify, only then clean up** (the console rejects duplicate names, so the fingerprint is part of the name) |
| Failure handling | import refusals and a missing certificate after import fail the run | if the console does not serve the new certificate after activation, the previous active one is re-activated and the run fails. Nothing is deleted on a failed verify (the rollback copy only exists during the run) |
| Cleanup | none, the same certificate is updated in place | after a **verified** push every other managed (`cert-push …`) certificate is deleted, so exactly **one** pushed certificate exists. Certificates with other names (for example a hand-uploaded one) are never touched |

What the job checks every run, even when nothing needs pushing (the Job exits non-zero on any of these):

- a probed name/port is unreachable or serves a certificate that is not the managed one;
- the **served** certificate has less than 14 days left;
- the **source** certificate's renewal is overdue by more than 3 days. Overdue is computed from the certificate's own validity:
  renewal is due when the remaining time drops below one third of the lifetime (`notAfter − notBefore`), so shorter Let's Encrypt
  lifetimes do not break it.

Every run prints one `SUMMARY` line per target (served fingerprint, served expiry, source expiry, planned renewal, action taken).

## What is deployed {#cert-push-components}

| Piece | Where |
| --- | --- |
| Chart | `system/default/cert-push` (namespace `default`, because the wildcard Secret is there and a pod can only mount Secrets from its own namespace) |
| Script | `system/default/cert-push/files/push.sh`, rendered into a ConfigMap (changes are reviewed in this repo) |
| Image | `ghcr.io/coollision/cert-push`, built from the orphan branch `orphan/cert-push-image-standalone` (own workflow, weekly rebuild, build fails on fixable HIGH/CRITICAL findings). Alpine + `bash`, `curl`, `openssl`, `jq`, `ca-certificates`, uid 65532, no `apk`, about 21 MB. Pinned by digest in the chart, Renovate bumps it |
| Credentials | Vault `kv/system/default/cert-push`, rendered into Secret `cert-push-creds` (see [Vault keys](#cert-push-vault)) |
| Alerts | `certificates.alerts` group in `system/monitoring/monitoring/templates/prometheusrules.yaml`: `CertPushStale`, `CertPushNeverRan`, plus the stock `KubeJobFailed` |

Design choices that look odd but are deliberate:

- **One shared certificate**, not one per device. The existing wildcard already covers every name (apex + wildcard). One issuance, one
  renewal to watch, no extra names in the public CT logs. The cost is that the same private key sits on Traefik, the NAS and the
  UniFi console. If any of them is compromised the certificate is revoked and re-issued everywhere anyway, followed by a root-cause
  check.
- **Push, not pull**, so the cluster can verify and alert on the result. Rejected: SSH file drop, a NAS-side Task Scheduler pull
  (no cluster-side verification), UniFi API keys (the certificate routes do not accept them).
- **No SSH anywhere.** UniFi through its HTTPS API, NAS through the local DSM web API.
- **One dedicated account per device**, used only by this job, nothing in git. For UniFi it is admin-equivalent: it gives
  separate revocation and an audit trail, **not** a smaller blast radius.
- **Own image instead of a public one.** The pod holds admin credentials, so a public swiss-army image was vetted and rejected:
  `nicolaka/netshoot:v0.16` had 542 HIGH/CRITICAL findings (21 critical, 540 fixable), runs as root and ships about 460 binaries
  (Trivy, 2026-11-15). The own image had none. The script is not baked in, only the tools.
- **Hardened pod**: non-root, read-only root filesystem, no ServiceAccount token (the job never talks to the Kubernetes API), all
  capabilities dropped, secret mounts `0440` with `fsGroup` (a `0400` file owned by root is unreadable for the non-root user),
  `/tmp` is a memory-backed emptyDir (cookie jar and the key copy used for upload never touch a disk).
- **Egress NetworkPolicy** to the two device addresses only (`/32` each, any port, so the probe list can grow without touching
  the policy). There is **no DNS rule**: the script reaches the devices by IP (`curl --resolve`, `openssl s_client -connect <ip>`), so
  the pod resolves nothing. The `podSelector` is label-scoped because namespace `default` holds other workloads.
- **No retry loop on login.** `backoffLimit: 0` and one attempt per run: DSM's auto-block counts failed logins and blocks the
  source IP (a k3s node address, not the pod).
- **UniFi calls retry only when nothing connected** (curl exit 6/7/35: DNS, connect or TLS handshake failure), 3 attempts with
  30s/60s back-off. No request reached the console, so it cannot double-submit or count as a failed login. Any HTTP status is
  never retried, and the NAS has no retry at all. Added after the 2026-11-20 run failed on `unexpected eof` during the console's
  TLS handshake (the next day was fine).
- **A failed probe is logged** (`probe says not up to date: <name:port>(unreachable|serves-other:<fp>)`) before the job falls
  through to a login, so the log says why it pushed.
- **Failed Job history is 1.** The stock `KubeJobFailed` rule fires while a failed Job exists, so a stale failure must not linger.
  It clears after the next successful run.
- **Probes go to the device address, not DNS**: some names resolve to public/Cloudflare addresses that the policy blocks and that
  would not prove what the NAS serves.

## Operating it {#cert-push-operations}

| What | How |
| --- | --- |
| See the last run | `kubectl -n default logs job/<newest cert-push job>`; the `SUMMARY` lines say what each device serves |
| Run now (normal logic) | `kubectl -n default create job --from=cronjob/cert-push now-$(date +%s)` |
| Push even if up to date | `kubectl -n default create job --from=cronjob/cert-push-force force-$(date +%s)` (or ArgoCD → app `cert-push` → CronJob `cert-push-force` → **Create Job**, which ignores `suspend`) |
| Force a new certificate | `cmctl renew -n default <wildcard-certificate>`; without `cmctl`, patch the Certificate's status subresource to add the `Issuing=True` condition (what `cmctl` does). This also re-issues Traefik's certificate. Then run the job |
| Probe only, no login | set `dryRun: true` in `values.yaml` |
| Break-glass | when the device currently serves a certificate the job cannot verify (expired or wrong), create a one-off Job by hand with `INSECURE=1` set on its command line. It is never a default and never in the chart |
| Clean up test Jobs | `kubectl -n default delete job <name>` (Jobs created by hand are not garbage-collected sooner than `ttlSecondsAfterFinished`, 7 days) |

Changing a Vault value reaches the pod on the next ArgoCD sync (the Secret is rendered at sync time), so hard-refresh the app
after patching. ArgoCD can also serve a stale render for a while, see the `gitops-debug` skill.

### Vault keys and commands {#cert-push-vault}

Path `kv/system/default/cert-push`. The chart reads exactly these keys, a missing key breaks the render. `vault kv put` **replaces**
the whole secret, use `vault kv patch` to change single keys. Passwords are read silently so they stay out of shell history.

| Key | Meaning |
| --- | --- |
| `nas-ip`, `unifi-ip` | device LAN addresses (also used in the NetworkPolicy) |
| `nas-host`, `unifi-host` | a name covered by the certificate, used for the API TLS check and SNI |
| `nas-probes` | space-separated `name:port` list of every DSM name/port that should serve the certificate; `none` while nothing is bound yet |
| `unifi-probes` | same for the console; `none` means `<unifi-host>:443` |
| `nas-username`, `nas-password`, `unifi-username`, `unifi-password` | the two dedicated accounts |

```bash
export VAULT_ADDR=<vault-url>   # and `vault login` as usual
read -rsp 'NAS password: ' NAS_PW; echo
read -rsp 'UniFi password: ' UNIFI_PW; echo
vault kv put -mount=kv system/default/cert-push \
  nas-ip=<nas-lan-ip>   nas-host=<name-covered-by-the-cert>   nas-probes=none \
  nas-username=<nas-account>     nas-password="$NAS_PW" \
  unifi-ip=<udm-lan-ip> unifi-host=<name-covered-by-the-cert> unifi-probes=none \
  unifi-username=<unifi-account> unifi-password="$UNIFI_PW"
unset NAS_PW UNIFI_PW
vault kv get -mount=kv -format=json system/default/cert-push | jq '.data.data | keys'   # key names only, never values
```

As services move to the new NAS certificate:
`vault kv patch -mount=kv system/default/cert-push nas-probes="<name>:443 <name>:5001"`.

### Device accounts and permissions {#cert-push-accounts}

- **NAS**: a dedicated account in the administrators group, **2FA off** (the API login cannot answer a second factor), no extra
  application privileges. DSM 7.2+ can enforce 2FA for administrators or adaptive MFA, which would break the login: check
  Security → Account. Put **every k3s node IP** on DSM's auto-block allow list (the job can run on any node, and a block could also
  cut SMB/NFS from that node).
- **UniFi**: a local-only account. The certificate API belongs to UniFi OS itself, not the Network app. Observed on 2026-11-15:
  with only `network.management` the certificate list returns **403**, adding `system.management.user` did not help, and it worked
  once the role also carried the other Control Plane permissions (access, protect, drive, talk and so on, plus
  `system.management.location`). There is no certificate-only role, so treat the account as admin-equivalent.

## Verified live (2026-11-15) {#cert-push-verified}

- NAS: all five original DSM certificates were intact afterwards (same IDs, expiries and service bindings, the desktop port still served the previous default's certificate). The first run created the new certificate **but flagged it as the default** (`as_default=false` is read as true); the previous default kept its services, only the flag moved, and it has to be set back by hand (Security → Certificate → select it → Set as default certificate). Fixed in the script, the job now fails on it. The first run created the certificate, unbound; a later run **imported under the same ID** (update in place) and the next run
  reported it already imported, so renewal does not create duplicates. Existing DSM certificates were not touched.
- UniFi: certificate created, activated and served (older managed certificates are removed after a verified push, one pushed certificate remains); a second run was a no-op; after a forced renewal of the wildcard the job pushed
  the new certificate and an independent TLS probe showed the console **and** Traefik serving the new fingerprint, equal to the
  Secret.
- Failure paths (CSRF refresh, two-certificate chain split, verify failure → rollback → exit 1) were tested against a mock of both
  APIs before the first real run.

Not verified yet: the NAS verify probes and the "bindings survive an import under the same ID" claim (nothing is bound to the new
certificate yet), the UniFi rollback and expired-certificate cleanup against the real console, and a full natural renewal cycle.

## Risks {#cert-push-risks}

- **Admin-equivalent credentials in the cluster.** Mitigations: one account per device, Vault only, mounted read-only into a
  non-root pod, egress policy to one host each, a script that calls a handful of fixed endpoints.
- **Undocumented APIs.** The UniFi certificate routes and the DSM import call can change with a firmware/DSM update. The daily
  verification turns that into a failed Job and an alert, not a silent expiry. Re-run the job by hand after every console or DSM update.
- **DSM auto-block** blocks the source IP. One attempt per run, no retry loop, allow-list the node IPs.
- **A UI blip on UniFi** while the new certificate activates, and DSM restarts the services bound to the certificate on import.
  Pushes only happen when the certificate changed (about every 60 days), at the job's time.
- **Locked-out services if an import breaks.** Both devices allow several certificates side by side. Recovery: **UniFi** →
  upload or re-activate a certificate by hand in the console UI (accept the browser warning). The job keeps no old copy after a
  verified push, so keep one manually uploaded certificate in the console as a break-glass copy if you want one; **DSM** → Security → Certificate →
  Settings, assign the old certificate to the affected service. Neither needs the cluster.
- **Other clients may pin or re-trust the certificate**: KMIP and Hyper Backup Vault peers on other machines, Drive clients.
- **Shared private key**: see the design choices above.
- Ports 6690 (Drive client) and Plex stay open regardless; this only removes the dependency on port 80.

## Remaining work {#cert-push-remaining}

NAS migration is done by hand, service by service:

1. Bind the new `cert-push …` certificate to one low-risk service first (the registry reverse proxy; its current certificate
   expires **2026-12-08**), verify what is served from a LAN client and from outside, then Plex, then the default certificate's
   services one at a time with the DSM desktop last. Confirm after the next renewal that every binding survived the import under
   the same ID. Fallback: if this has not landed by about 2026-12-01, renew the registry certificate in DSM by hand.
2. Add the bound names to `nas-probes` as each one moves.
3. **Port 80 is closed (2026-11-15):** the gateway forward "NAS HTTPS + DSM" went from `443,5001,80` to `443,5001` (no other
   NAT rule used port 80). From now on DSM's own HTTP-01 renewal cannot work, so every service must be on the pushed certificate
   before the old certificates expire. Still on the old default at the time: one reverse-proxy entry (certificate expires
   2027-02-09), move it first. Then delete the old DSM Let's Encrypt certificates (default, Plex, registry, all unbound) and their
   scheduled renewal; keep QuickConnect and the self-signed one. Closing port 80 breaks nothing at once, the cost is an expired
   certificate on anything left behind.
4. Drop the 522 / multi-name renewal note from [known issues](../05-operations/known-issues.md) if it is still there.
5. Decide whether to keep the hand-uploaded UniFi certificate as a manual break-glass copy (the job never touches it, it is inactive).
6. Homepage card with tiles (days until expiry, days until planned renewal, age of last push) and a link to the CronJob in ArgoCD.
   Homepage has no safe button (a `customapi` POST would fire on every page load), so the click happens in ArgoCD.
7. Rehearse the recovery steps above once.
