# TODO — Zot: one pull-through mirror for every registry

[← Back to Home](../Home.md) · Parent: [Registry mirror](registry-mirror.md) · Rejected alternative: [Spegel](../07-rejected/spegel-embedded-registry.md) · Related: [Ansible](../00-setup/ansible.md), [k3s tuning](k3s-tuning.md)

> **Status (2026-11-03):** plan only, nothing built. Details below come from Zot's published docs and k3s
> community write-ups, not from a test on this cluster; everything marked *verify* must be checked first.

## Goal

One small, always-on service on the NAS that caches every image the cluster pulls, from **all** upstreams
(docker.io, ghcr.io, quay.io, registry.k8s.io, ecr-public, registry.gitlab.com), so that:

- a pull after a node failure comes from the LAN, not the internet (the ~85 s Home Assistant ghcr pull becomes a
  LAN copy);
- cluster restarts and new nodes keep working when the internet or an upstream is down, for every image that was
  pulled at least once;
- registry tokens live in one place instead of in plaintext on every node;
- docker.io rate limits stop mattering.

Non-goals: a UI, quotas, vulnerability scanning, replacing kube-fledged on its own (see
[the kube-fledged question](registry-mirror.md)), pushing our own images.

## Why Zot

Zot is a single static OCI registry binary. Its `sync` extension can pull through on demand from several
upstreams in one process, each mapped to its own path prefix. The alternatives either mirror only one upstream
(`registry:2`, so five containers) or are far heavier (Harbor, Nexus).

## Target architecture

```
kubelet/containerd ──► registries.yaml mirror (per upstream, same host, rewrite prefix)
                          │  https://<mirror-host>/ghcr/<repo>
                          ▼
                    reverse proxy (existing, TLS)
                          ▼
                    Zot on the NAS  ── onDemand sync ──►  ghcr.io / quay.io / docker.io / ...
                          │
                    local disk (NAS volume)
```

| Upstream | Zot path prefix | Notes |
|---|---|---|
| docker.io | `/docker` | replaces today's separate `registry:2` |
| ghcr.io | `/ghcr` | needs the token currently written to the nodes |
| quay.io | `/quay` | anonymous |
| registry.k8s.io | `/k8s` | anonymous; also serves `k8s.gcr.io` redirects |
| public.ecr.aws | `/ecr` | anonymous |
| registry.gitlab.com | `/gitlab` | needs the GitLab token |

## Zot configuration sketch

Concept only; field names from Zot's mirroring docs, *verify* against the pinned version.

```json
{
  "storage": { "rootDirectory": "/var/lib/zot", "gc": true },
  "http": { "address": "0.0.0.0", "port": "5000", "compat": ["docker2s2"] },
  "extensions": {
    "sync": {
      "enable": true,
      "credentialsFile": "/etc/zot/credentials.json",
      "registries": [
        { "urls": ["https://ghcr.io"],
          "content": [{ "prefix": "**", "destination": "/ghcr" }],
          "onDemand": true, "tlsVerify": true,
          "maxRetries": 3, "retryDelay": "5s" }
      ]
    }
  }
}
```

One `registries` entry per upstream in the table above. `credentialsFile` holds the ghcr and GitLab logins
(JSON keyed by registry host); it is a Secret on the NAS, never in git.

## Client side (k3s) changes

Today `ansible/roles/k3s_pre/templates/registries.yaml.j2` renders only `endpoints` and `auth` per registry.
It has **no `rewrite` support**, so the plan needs a small template change:

- Extend the template to emit an optional `rewrite:` map per registry.
- In the `k3s_registries` group var, point each upstream at the mirror host with a rewrite so the request gets the
  right Zot prefix, and keep the original upstream as a second endpoint so pulls fall back if the mirror is down:
  - endpoints: mirror first, upstream second
  - rewrite: `"^(.*)$": "ghcr/$1"` (per upstream prefix)
- Remove the `auth` blocks from the nodes once Zot holds the credentials, then rotate those tokens.

*Verify*: the exact k3s key names (`endpoint` vs `endpoints`), that `rewrite` applies only to the mirror endpoint
and not to the fallback, and that library images on docker.io (`library/alpine`) map to the right path.

## Phases

1. **Spike on a laptop or the NAS, no cluster impact.**
   - [ ] Run Zot with one entry (ghcr) and pull the Home Assistant image through it; time cold vs warm.
   - [ ] Measure idle RAM and RAM during the 660 MB pull.
   - [ ] Confirm what the client sees when upstream is unreachable and the image is **not** cached, and when it
         **is** cached (the offline case).
   - [ ] Confirm retention: pulled images must stay indefinitely, so check `gc`/retention defaults and set them
         explicitly (a mirror that evicts is useless offline).
2. **Deploy properly on the NAS.**
   - [ ] Container with a pinned image tag (Renovate-managed), data volume sized for the real working set
         (measure with a `crictl images` total across nodes first), restart policy, healthcheck.
   - [ ] Reverse-proxy host and TLS (cert must be trusted by the nodes; same trust path as the current mirror).
   - [ ] Credentials file stored on the NAS, backed up with the NAS config, not in git.
3. **Add all upstreams, still unused.** One entry per upstream; test each by hand with `crictl pull` against the
   mirror URL from a node.
4. **Switch one upstream on one node.**
   - [ ] Ansible change for the template (`rewrite`), `--check --diff` on one node, **without printing the diff**
         (tokens). Drain the node, apply, uncordon, wait for Longhorn healthy.
   - [ ] Cold pull on the drained node, compare to the direct-ghcr baseline (~85 s for HA).
   - [ ] Stop Zot on purpose and confirm the fallback to the real upstream works for that registry.
5. **Roll out to all nodes**, one at a time, same drain / Longhorn-healthy routine.
6. **Clean up.**
   - [ ] Remove the old `registry:2` docker.io container once docker.io goes through Zot.
   - [ ] Remove the node-side tokens and rotate the ghcr/GitLab tokens.
   - [ ] Pre-warm: pull the critical images once through the mirror (Home Assistant, Longhorn, ArgoCD, Traefik,
         Vault, CNPG, Zot's own image) so an offline start works.
   - [ ] Update `wiki/00-setup/ansible.md` and the housekeeping page; delete this TODO and
         [Registry mirror](registry-mirror.md), fold the result into a platform page.

## Risks and open questions

| Risk / question | Mitigation or check |
|---|---|
| NAS becomes a dependency for every pull | upstream stays as second endpoint; test the fallback explicitly |
| Mirror down **and** internet down | only previously cached images work, so pre-warm the critical set |
| Cache grows without bound | measure working set; set retention; monitor volume usage with an alert |
| Image digests or media types change through the proxy | pin a Zot version that preserves upstream digests (v2.1.22+ per docs); keep `docker2s2` compat if needed, *verify* |
| Rewrite rules mis-map `library/` images or multi-segment repos | test every upstream with a real pull before touching nodes |
| Restarting k3s on a node kills all its pods | drain one node at a time, wait for Longhorn healthy |
| Chicken-and-egg: Zot's own image is pulled from upstream | it runs on the NAS, outside the cluster; pin and keep a local copy |
| Slow first pull (NAS fetches, then serves) | expected; first pull through the mirror is not faster than direct |
| Zot memory on the NAS | measure in the spike; set a container memory limit |
| NAS reachable from nodes only on one VLAN | confirm routing/firewall from every node subnet before rollout |

## Decision gates

- After the spike: if idle RAM is high or the offline behaviour is bad, fall back to one `registry:2` per upstream
  (listed in [Registry mirror](registry-mirror.md)).
- After phase 4: if a warm mirror pull of the HA image is within ~15 s, decide about removing kube-fledged
  (same page, "Is kube-fledged still worth keeping?").

## Rough effort and downtime

- Spike plus deploy: an evening. Rollout: 3-4 nodes, each ~5-10 min of drain, k3s restart and Longhorn settle,
  so 30-60 min total, done one node at a time. Pods on a drained node reschedule; apps with a single replica
  and an RWO volume are down for the move. Rollback is the same procedure with the original `registries.yaml`.
