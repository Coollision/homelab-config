# TODO — Broaden the container registry mirror

[← Back to Home](../Home.md) · Related: [Cluster housekeeping](../01-platform/cluster-housekeeping.md) · [Ansible](../00-setup/ansible.md)

> **Status (2026-11-03):** direction chosen (NAS mirror per upstream), nothing built. Delete this page once a mirror covers the registries below.

## Today

- k3s nodes get `/etc/rancher/k3s/registries.yaml` from `ansible/roles/k3s_pre/templates/registries.yaml.j2`,
  driven by the `k3s_registries` variable.
- `docker.io` is mirrored to a pull-through cache on the NAS (a plain `registry:2`, which can proxy only **one**
  upstream).
- `ghcr.io` and `registry.gitlab.com` go straight to the internet with a token written into each node's file.
  `registry.k8s.io`, `quay.io` and `ecr-public.aws.com` are not configured at all.
- Why it matters: big ghcr pulls are the slow ones (Home Assistant's ~660 MB image took ~85 s to pull on one node).

## Decision (2026-11-03)

A **local pull-through mirror on the NAS** is the way to go. It is the only option that survives a node dying
(the image is not on the dead node) and keeps pulls working when the internet is down or slow. The k3s embedded
registry (Spegel) was considered and rejected, see
[Rejected: Spegel](../07-rejected/spegel-embedded-registry.md).

## Options

- [ ] **Zot, one container for all upstreams (preferred, [full plan](zot-registry-mirror.md)).** A single OCI registry binary whose `sync` extension
      does on-demand pull-through for several upstreams at once. Each upstream gets a `registries` entry with
      `onDemand: true` and a `content.destination` path prefix (e.g. `/ghcr`, `/quay`); upstream logins live in one
      `credentialsFile`. `registries.yaml` then points every upstream at the same host and adds a `rewrite`
      (`"^(.*)$": "ghcr/$1"`) so k3s requests the right prefix. Covers first pulls and failover.
- [ ] **One `registry:2` pull-through container per upstream** (ghcr, registry.k8s.io, quay, ecr-public, gitlab),
      next to the existing docker.io one. Roughly 20-40 MB RAM each, behind the existing reverse proxy.
      Works, but `registry:2` proxies only one upstream, hence five containers.
- [ ] **Harbor with proxy-cache projects.** One host, UI, quotas, but runs Postgres, Redis and several services
      (GBs of RAM). Not worth it for five upstreams.

Open questions:

- [ ] Zot specifics to verify before building: RAM at idle and under a big pull, retention/GC settings so cached
      images are not evicted (matters for the offline case), docker2s2 compat flag (`http.compat`) for upstreams
      serving Docker schema 2, and what a client sees when every upstream is down and the image is not cached.
- [ ] Run it on the NAS, not in the cluster: a mirror inside k3s would have to pull its own image from upstream,
      and cluster pulls would depend on the cluster being healthy.
- [ ] Offline behaviour: a pull-through cache only serves what it has already fetched. Decide whether the
      images that matter (HA, Longhorn, ArgoCD, Traefik, Vault) must be pre-warmed on the mirror.
- [ ] NAS is now a dependency for pulls: confirm k3s falls back to upstream per registry when the mirror is
      down, and that the mirror's storage has headroom.

## Rollout notes

- Changing `registries.yaml` restarts k3s on that node, which kills all pods on it (`KillMode=control-group`).
  Drain one node at a time and wait for Longhorn to be healthy in between.
- k3s falls back to the original upstream if a mirror is down; verify per registry.
- Rotate the ghcr/GitLab tokens when they leave the nodes.
- Never print the diff of `registries.yaml`: it contains the registry tokens.
- Verify with a cold pull on a drained node and compare against the direct ghcr time.

## Is kube-fledged still worth keeping?

Open question. It pre-pulls the Home Assistant image onto 3 nodes so failover skips the ~85 s ghcr pull. That
is a small part of a real node failure (default not-ready eviction ~5 min, plus Longhorn RWO detach/attach), and
it costs a controller, a webhook, a wait job, node labels and permanent `Failed` status noise. With the NAS
mirror in place a failover pull is a LAN copy, so the gain shrinks further.

- [ ] After the ghcr mirror is live, time a cold HA pull through it. If it is within ~15 s, remove kube-fledged
      (ImageCache in shared-lib `_imagePreSync.yaml`, `system/kube-system/kube-fledged`, `kubefledged.io/cache`
      node labels in ansible host_vars, wiki) and skip the fork below.

## Fork kube-fledged to make the image-delete timeout configurable

Root cause of the `Failed` / `DeadlineExceeded` noise (checked against upstream `master`, 2026-11-03):
`pkg/images/image_manager.go` builds the delete Job in `newImageDeleteJob` with
`crictl --runtime-endpoint=... --image-endpoint=... rmi <image>` (two spots, one per runtime branch) and **no
`--timeout`**, so crictl's default of 2 s applies. Containerd on these nodes needs longer to remove an image.

Plan:

- [ ] Fork `senthilrch/kube-fledged`; add a controller flag `--image-delete-timeout` (default keeps today's
      behaviour) and pass it as `crictl --timeout=<n>s` in both `rmi` commands. Wire it through the chart as
      `args.controllerImageDeleteTimeout`.
- [ ] Add/extend a unit test in `pkg/images/image_manager_test.go` for the generated Job args.
- [ ] Build the controller image (multi-arch amd64 + arm64, one worker is a Pi) and publish it to a registry the
      cluster can pull; point the chart's controller image at it.
- [ ] Open the PR upstream; drop the fork once a release includes the flag. If kube-fledged is dropped (see above),
      skip all of this.

Cheaper stop-gap: ignore the `Failed` status (cleanup of the old tag only; kubelet GC removes it later).
