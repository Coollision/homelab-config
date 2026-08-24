# Known Issues & Troubleshooting

[← Back to Home](../Home.md) · See also: [History & Evolution](history.md) · [kubectl cheat sheet](kubectl-cheatsheet.md)

Every entry here is a real incident this cluster has hit, not a hypothetical. Organized
by area — use your browser's find-in-page for a symptom keyword if you're not sure which
section applies.

---

## ArgoCD / GitOps

### ArgoCD repo-server OOM {#argocd-repo-server-oom}

**Symptom:** the ArgoCD repo-server's rendering sidecar gets OOMKilled repeatedly, and it
seems to correlate with certain charts more than others but not consistently.

**Root cause (two, stacked):** (1) the rendering sidecar's CPU limit was low enough that
heavy chart renders stretched toward the plugin timeout and piled up concurrently; (2)
the real memory hog was **parsing a classic Helm chart-repository index file** for a
Bitnami-style chart repo — that index lists nearly every version of every chart the repo
has ever published, and parsing it into memory peaked over 500Mi, while an OCI-registry
pull of the exact same chart peaked at a fraction of that. Because Helm shares one repo
config per rendering pod, *any* render that touched that repository poisoned every later
render in the same pod, causing OOMKills on completely unrelated charts.

**Fix:** move any chart dependency still using a classic Helm repository URL to its OCI
registry equivalent instead (same chart, byte-identical render, but no giant index to
parse), raise the rendering sidecar's CPU limit instead of throttling it, and cap ArgoCD's
own reconcile concurrency (`controller.status.processors`/`controller.operation.processors`)
since the sidecar's render concurrency isn't bounded by ArgoCD's native parallelism knob
at all. **A cache would not have helped** — the cost is the CPU/memory of parsing the
index every time, not the network cost of downloading it.

### ArgoCD's manifest cache doesn't always bust on `refresh=hard` {#argocd-manifest-cache-doesnt-bust}

**Symptom:** you change a chart's `values.yaml`, push, ArgoCD reports "successfully
synced" — but `kubectl get <resource> -o yaml` still shows the *old* values, and the
Application shows the resource as unchanged.

**Cause:** the rendering plugin caches rendered manifests, and a `refresh=hard`
annotation doesn't reliably invalidate that cache.

**Fix, in escalating order:**
1. `kubectl -n argocd annotate application <app> argocd.argoproj.io/refresh=hard --overwrite`
2. If that doesn't work: `kubectl rollout restart deploy/argocd-repo-server -n argocd`,
   wait for the rollout, then re-sync.
3. As an urgent workaround only: patch the live resource directly — ArgoCD will
   re-converge once its cache eventually catches up on its own.

This pattern showed up repeatedly during the Sablier scale-to-zero rollout too — after
almost every push, expect to need a repo-server bounce before the change actually lands.

### Moving a workload between namespaces {#moving-a-workload-between-namespaces}

**The non-obvious fact:** because an Application's identity is just its directory's
basename (see [GitOps structure](../00-setup/gitops-structure.md)), `git mv`-ing a chart
from `workload/<old-ns>/<app>` to `workload/<new-ns>/<app>` does **not** delete and
recreate the Application — the ApplicationSet controller sees it as an in-place mutation
of the same Application's source path and destination namespace. This has real
consequences for any such migration:

- **Pause automated sync first** (`syncPolicy: none` on the Application), *before*
  scaling anything down manually — otherwise `selfHeal` reverts your manual scale-down
  within seconds and the app restarts against half-migrated state.
- Sync the moved app **without prune** at first, so the old namespace's resources stick
  around as a live rollback path; only prune once you're confident.
- **Service names may change** if the chart is also being re-templated through a shared
  library for the first time (e.g. a `-web` suffix becoming a `-service` suffix). Any
  *other* app that hardcodes the old hostname in its own runtime config (not GitOps —
  its own database/settings file) will silently keep pointing at the old, soon-to-be-
  deleted Service until someone manually repoints it.
- **Longhorn volumes with `protect: true` block deletion** — remove that label before
  trying to delete an old volume during cleanup.
- **A completed data-copy Job's PVC keeps a protection finalizer** — delete the Job/pod
  *before* trying to delete its PVC, or the delete hangs forever.
- If the storage class is Retain-reclaim, a deleted scratch/temporary PVC leaves an
  **orphaned PV behind** — clean it up manually, it won't happen automatically.
- A database pod can get stuck `Terminating` when its Cluster CR is deleted — a forced
  pod delete (`--force --grace-period=0`) is sometimes needed to unblock it.

### Verify where a service actually runs before acting on it {#verify-where-a-service-actually-runs-before-acting-on-it}

**The trap:** a service with a familiar name showing up in `kubectl get svc` is *not*
proof it's the instance you actually care about. A matching name plus a working API key
is not enough — verify by content (an item count, a known record) before you port-forward
into it or make changes based on what you see there. This has genuinely caused wasted
investigation time against the wrong instance in this environment before.

---

## Storage

### Longhorn volumes are GitOps-managed — don't `kubectl patch` them {#longhorn-volumes-are-gitops-managed}

**Symptom:** you patch a Longhorn `Volume`'s replica count (or any other spec field)
live, it seems to work, and then a few seconds later it silently reverts.

**Cause:** Longhorn `Volume` CRs in this cluster are rendered declaratively by the shared
Helm libraries and are `selfHeal`-managed by ArgoCD — a live patch gets reverted in
roughly 4 seconds.

**Fix:** change the `replicas:` field (or whatever you're touching) in the owning app's
`values.yaml`, commit, and let ArgoCD apply it. There is no supported live-patch path
for these resources.

### Resizing a Longhorn PVC {#resizing-a-longhorn-pvc}

**You cannot shrink a Longhorn PVC in place** — Kubernetes/Longhorn only support growing
a volume live. To shrink one, the only path is: pause the app, scale to zero, copy data
to a scratch volume, delete the old PVC/PV/Volume, recreate a smaller one (same name, so
the chart's git-rendered definition binds to it), copy the data back, re-enable the app.
Watch for: a finished copy Job's pod keeps a PVC-protection finalizer on its source PVC
(delete the pod first); a Retain-reclaim storage class leaves an orphaned PV behind after
a scratch PVC is deleted (clean it up by hand); a `protect: true` label blocks the volume
delete entirely until removed.

### The SMB operator's hard-mount trap {#smb-operator-hard-mount-trap}

**Symptom:** a pod using an SMB-shared (RWX-over-Longhorn) volume gets stuck in
`Terminating` forever and blocks its own replacement — and this can cascade to *every*
share the SMB gateway serves, not just the one that triggered it.

**Root cause:** a Longhorn RWX volume detaches — and its backing NFS export
disappears — the instant its last consumer scales to zero (which Sablier does routinely
for enrolled apps). If the SMB gateway pod has that now-vanished NFS export **hard**
mounted, the mount goes into an uninterruptible-sleep state that not even `SIGKILL` can
clear. One real incident took down all shares served by this operator for roughly 30
hours before the fix landed, and ArgoCD had no visibility into the outage the whole time
— it only tracks the *operator*, not the Deployment the operator itself manages at
runtime, so the app read `Synced`/`Healthy` throughout.

**Fix, and the constraint to never reintroduce:** NFS endpoints must live in a ConfigMap
an in-pod reconciler script reads live — **never in the pod spec itself** — and mounts
must use `soft`, never `hard`. Two related traps: never put a timestamp in the pod
*template's* annotations (it rolls the pod on every single reconcile), and never mount
that ConfigMap with `subPath` (a `subPath`-mounted ConfigMap is snapshotted once at pod
start and never updates again).

### Node memory asymmetry and Longhorn's per-node cost {#node-memory-asymmetry}

The cluster's nodes are not identical in available RAM, and the smallest one fills up
first — it's the real memory bottleneck to watch, not the biggest node. Because Longhorn
is configured with no soft anti-affinity, every 3-replica volume puts exactly one replica
on **every** node including the control-plane node, whose Longhorn instance-manager
process footprint therefore adds up meaningfully even though it does no Kubernetes
control-plane-adjacent work. **Longhorn has no memory *request* setting for its
instance-manager process at all** — only a CPU-percentage guarantee — so this memory use
is completely invisible to the scheduler unless you separately reserve headroom for it at
the node/kubelet level. The only real lever to reduce it is fewer replicas on non-critical
volumes (2 instead of 3) — most of that memory is legitimate per-replica working set, not
overhead you can tune away.

---

## Networking / DNS

### Multus `dhcpHostname` is a dead end {#multus-dhcphostname-is-a-dead-end}

**Symptom:** you set a hostname on a Multus MACVLAN attachment expecting it to show up on
the network controller (e.g. UniFi), and it never does — the device shows up
mis-identified by fingerprint instead (a pod's virtual interface has been fingerprinted
as a random piece of consumer electronics before).

**Fact, not theory:** the `dhcpHostname` field renders correctly into the network
attachment definition and the DHCP option is genuinely sent — but nothing on the
receiving end has ever actually registered a hostname from it, for reasons that would
need a packet capture to fully explain. Treat it as documentation-only.

**Actual fix:** the naming step that works is setting an **alias** on the network
controller's client-list entry via its API (not its friendly public API — the legacy
internal one is the only one that can write client aliases) — alias outranks fingerprint
and DHCP hostname in every network controller UI. Pin a fixed MAC and DHCP reservation
alongside it.

### mDNS reflector must not re-publish {#mdns-reflector-must-not-re-publish}

**Symptom:** after any Matter/Thread device address change, devices never successfully
re-register no matter what gets restarted — SRP registration attempts fail with a
"duplicate" error indefinitely.

**Root cause:** an mDNS bridge between VLANs that **re-publishes** records under its own
identity (a real Avahi responder configured as a "reflector") causes the Thread border
router's own SRP advertisements to echo back to it over the wire, which it then
interprets as a genuine name conflict and rejects. This is silent, permanent, and has
nothing to do with SRP lease timing — no amount of restarting the border router fixes it,
because the rejection happens on every single re-registration attempt going forward.

**Fix:** use a pure L2 packet repeater that forwards bytes verbatim and never generates
its own response — a conflict becomes structurally impossible. See
[Matter & Thread](../02-smarthome/matter-thread.md) for the full incident writeup; this
was the single biggest root cause behind "mass sensor unavailable" incidents on this
mesh.

### The Matter/Thread mesh is fragile by design {#the-matter-thread-mesh-is-fragile-by-design}

See the dedicated [Matter & Thread](../02-smarthome/matter-thread.md) page for the full
playbook. Short version: a small number of mains-powered router devices carry the entire
mesh; every other device is a battery-powered sleepy end device that can never route.
**Restarting the border router is almost never the right first move** — it wipes an
in-memory registry that then takes up to its configured lease time to rebuild, so a
"fix" attempt can genuinely make an outage longer. Try the Matter controller (matter-
server) restart first; it's cheaper and fixes more cases.

### ESPHome's VLAN40 mDNS leg needs the `sbr` chained plugin, and enough memory {#esphome-vlan40-mdns-oom}

Two independent, unrelated-looking failure modes on the same app: (1) without the `sbr`
source-based-routing chained CNI plugin on its IoT-VLAN MACVLAN leg, the VLAN's own
DHCP-assigned default route hijacks the pod's egress and firmware builds fail trying to
resolve package-registry hostnames — looks like a DNS bug, is actually a routing bug; (2)
a dashboard rewrite in a past release roughly doubled idle memory and made compile-time
memory spikes much larger, so a memory limit that used to be generous became too small
and caused an OOMKill on startup that presented in the browser as "WebSocket not
connected" with no obvious cause. Check for an OOMKill first if this symptom recurs after
a version bump.

### Thread and DNS: surviving an ISP prefix rotation {#thread-and-dns-surviving-isp-prefix-rotation}

Anything that hardcodes a globally-routable IPv6 address derived from the ISP's delegated
prefix is fragile — that prefix *will* rotate eventually, silently breaking whatever
depended on the old address with no obvious error. Two concrete fixes already applied
because of this: Home Assistant's route to the Thread mesh uses the border router's
**link-local** address, not its global one; and the DNS cluster's own configuration had
stale global-address entries purged and replaced with stable, ISP-independent
addressing. If something that depends on IPv6 connectivity to an IoT device mysteriously
breaks after an unrelated network event, suspect a prefix rotation before anything else.

### Technitium DNS cluster {#technitium-dns-cluster}

The in-cluster DNS server runs as a 3-node cluster (two in-cluster instances plus one
off-cluster instance as a manual break-glass), and its configuration has accumulated a
long list of non-obvious, hard-won rules:

- All cluster-internal communication happens over each node's dedicated VLAN address,
  **never** a Kubernetes ClusterIP or node IP — this is the only model that lets the
  off-cluster instance participate at all, since it can't route to Kubernetes-internal
  addresses.
- Pods need explicit host routes out of their VLAN interface to reach their peers,
  because the source-based-routing CNI plugin would otherwise send that traffic out the
  cluster overlay instead — and every peer's ACLs reject traffic that doesn't originate
  from an expected address.
- Certificate trust between cluster nodes is validated via DNS records (DANE/TLSA), not
  system certificate roots — a broken zone transfer breaks *that* validation too, which
  can look like a completely unrelated certificate error.
- A DNS "catalog" zone's notify/transfer-ACL settings are **auto-derived** from the
  cluster's own node-address configuration and get silently regenerated the moment
  anything touches that config — editing them by hand only fixes the symptom, not the
  cause, and they'll drift back.
- Zone counts differing between cluster nodes is usually *not* drift — compare SOA serial
  numbers instead; a differing zone count is more often a legacy zone type the API simply
  refuses to delete on one node, fixed by a software update rather than a config change.
- Forwarder-type reverse-lookup zones default to denying zone transfer, which can leave
  secondary copies of them frozen at an old snapshot indefinitely without any error
  surfacing — worth checking explicitly if a reverse-lookup zone seems stuck in the past.

### UniFi/network-controller API: two different APIs, and only one can write {#unifi-network-controller-api}

If scripting against a UniFi-style network controller: its documented/modern API is
read-only for client records (no alias, no fixed-IP field) — writing a client alias or a
fixed IP reservation requires the older, undocumented legacy API instead. Reserving an
address still held by another client fails until that client is explicitly forgotten
first, and a forgotten client's record can reappear on any residual traffic from it.

---

## Databases

### The ArgoCD repo-server OOM — see [above](#argocd-repo-server-oom)

This is filed under both sections because it's simultaneously a GitOps incident and the
direct trigger for one of the two Postgres migrations onto CloudNativePG.

### HA recorder → CloudNativePG migration {#ha-recorder-to-cnpg-migration}

Home Assistant's recorder was migrated off its previously-bundled MariaDB onto the shared
smarthome CloudNativePG cluster. Lessons worth keeping if a similar live database
migration ever happens again:

- **ArgoCD's `selfHeal` reverts a manual `kubectl scale` within seconds** if the app is
  still on automated sync — pause automated sync on the Application *before* scaling
  anything down for a migration, not after.
- **A bulk row copy does not advance the target database's auto-increment sequences** —
  every table's sequence needs to be explicitly reset to `max(primary key)` after a bulk
  load, or the first native write after cutover collides with an existing row.
  This was *the* critical step that would have silently corrupted the cutover if missed.
- **A generic ETL/copy tool may silently skip database views**, copying only base tables
  — any view-backed data needs to be materialized as a real table separately.
  Granting the ability to bypass foreign-key constraints during a bulk load requires a
  superuser-scoped grant that should be revoked again immediately afterward.
- A naive bulk-load configuration can **OOM the loading process itself** — tune batch
  size/prefetch down and give the loader pod a generous memory limit rather than
  assuming a bulk copy is "just I/O."
- **Let the target application build its own schema first** (via a disposable instance
  pointed at an empty database) so schema versions match exactly at cutover — this avoids
  the target app trying to run its own migration on data that's already in a different
  shape than it expects.

Separately, a real production data-integrity lesson: don't carry over performance
tuning from a previous database engine on the assumption it will transfer — one such
tuning change was tried here, showed no reproducible benefit on the live, contended
workload, and was reverted; only a change that fixed a specific, reproducible query plan
was kept permanently.

### No Thanos in use {#no-thanos-in-use}

The monitoring stack here does **not** run Thanos. Any release notes for a monitoring
stack version bump that mention Thanos-related breaking changes are not relevant to this
cluster and shouldn't be treated as a blocker — confirm with a quick check for any
Thanos-related custom resources before assuming otherwise, but by default, ignore those
notes.

---

## Monitoring / Grafana

### Grafana dashboards silently don't render {#grafana-dashboards-silently-dont-render}

**Symptom:** a dashboard ConfigMap exists, the sidecar that's supposed to load it appears
to be running fine, and the dashboard still never shows up in Grafana — with no error
visible anywhere obvious.

**Two independent causes found here, both worth checking:**
1. Grafana's dashboard-sidecar namespace search scope wasn't actually set to search all
   namespaces (only the alerts sidecar had that set) — any dashboard shipped from outside
   the monitoring namespace was invisible until this was fixed.
2. A separate, still-open gap: a template gate checking whether Grafana/its default
   dashboards are "enabled" reads a value that is never actually set to true anywhere in
   this repo's values — so a whole set of bundled dashboards render nothing, silently,
   and this has been flagged but not yet fixed as of this writing.

If you add a new dashboard and it doesn't appear, check both of these before assuming
your ConfigMap or JSON is wrong.

### Local `helm template` can lie about what ArgoCD will actually render {#helm-4-local-render-gap}

If you're on a newer major Helm version locally than what ArgoCD uses to render in
cluster, be aware: newer Helm versions changed how a subchart's own default values get
merged into the parent chart's values tree, so a template that depends on a subchart
default being coalesced up can render as empty locally while rendering correctly in
ArgoCD (or vice versa). If a local `helm template` run doesn't show a change you expect,
try explicitly setting the value you'd expect to be inherited before concluding the
template itself is broken.

### The Grafana v2 dashboard export format needs a wrapper {#grafana-v2-dashboard-export-format}

**Symptom:** you export a dashboard from a recent Grafana version's UI, drop the JSON
into the dashboards folder, and the provisioning sidecar rejects it on a recurring timer
with an error about a "v2 format" API — the dashboard never appears.

**Cause:** newer Grafana versions' UI export defaults to a newer schema that plain file
provisioning does not accept as a bare object — it needs to be wrapped in a small
Kubernetes-style resource envelope (`apiVersion`/`kind`/`metadata`/`spec`) around the
exported content. Once wrapped this way, both old-style and new-style dashboard files
coexist fine in the same folder, and Grafana's own save dialog starts returning the
already-wrapped format going forward, so this only needs doing once per dashboard. Keep
the original dashboard identifier as the wrapper's resource name if you want existing
deep-links to that dashboard to keep working.

---

## Sablier / scale-to-zero

### General gotchas from the rollout {#sablier-scale-to-zero-gotchas}

- **Sablier's periodic app-discovery loop lists several different Kubernetes resource
  types in one combined call, and a permissions error on any single one silently breaks
  discovery for every enrolled app, every cycle** — not just the type that errored. Keep
  every RBAC permission this loop needs granted, even for resource types you don't
  actually use with this feature, or the whole scale-to-zero mechanism quietly stops
  noticing newly-enrolled apps.
- **Sablier's session tracking is in-memory only** — restarting the Sablier controller
  itself (e.g. for a version bump) forgets every currently-tracked "awake" session, and
  its default behavior on startup is to then treat any already-running instance it
  rediscovers as "orphaned" and stop it. Expect a brief wave of unexpected scale-downs
  right after a Sablier version bump.
- **Apps whose upstream Helm chart has no label-injection knob** need a hand-written
  Kustomize label patch to enroll in scale-to-zero at all — this has been needed for a
  vendored UI chart and for the cluster-access MCP server, both of which hardcode labels
  their upstream chart gives no override for.

### MCP servers vs. scale-to-zero {#mcp-servers-vs-scale-to-zero}

**The conflict:** idle-timeout scale-to-zero resets its idle clock on new incoming HTTP
requests — but a long-lived session-based protocol (like an MCP server's persistent
connection) can sit open and quiet without sending a new request, so the clock expires
and the pod scales to zero **out from under an still-open client session**, killing it.

**This is confirmed in practice, not theoretical** — an MCP server enrolled in
scale-to-zero here genuinely does this: it works, but needs a short manual reconnect
after being woken from a cold scale-down. The accepted trade-off for a low-footprint app
was to keep it enrolled anyway rather than revert, since the memory saved was judged
worth an occasional manual reconnect. If you're deciding whether to enroll a new
MCP/session-protocol server: flag this trade-off explicitly rather than assuming it's a
simple win like a stateless dashboard would be. The fix menu, if it ever becomes
annoying: bump the session duration way up so wake cycles are rarer, or just don't
enroll that app.

---

## Node / cluster operations

### Node naming split {#node-naming-split}

The name Kubernetes uses for a node (as seen in `kubectl get nodes`) is **not** the same
string as that machine's DNS hostname/SSH target. Always use the Ansible
inventory/DNS hostname for SSH or host-level DNS lookups, and the `kubectl get nodes`
name only inside `kubectl` commands — mixing them up gets you an NXDOMAIN or a failed SSH
connection that looks like a broken host, when the host is actually fine.

### `kubectl` runs from the laptop, not from inside a node {#kubectl-runs-locally}

The cluster's kubeconfig is already configured on the operator's own machine — there's no
need, and no working path, to `ssh` into a node and run `kubectl`/`k3s kubectl` from
there (the SSH user doesn't have a usable kubeconfig on the nodes). SSH into a node only
for genuinely host-level things (checking a network interface, reading a raw config
file, packet-capturing a specific VLAN) — everything Kubernetes-related goes through
`kubectl` run locally.

### Node disk pressure from accumulated container images {#node-image-disk-churn}

**Symptom:** a node alerts on disk pressure / insufficient image filesystem space, and it
recurs periodically without an obvious single cause.

**Cause:** automated dependency-update tooling pulls a fresh image tag on every version
bump, and the old tag is rarely referenced by anything running afterward — the
container runtime's own image garbage collection only runs once usage crosses a high
threshold and stops the moment it drops back below it, so it leaves most stale layers
behind rather than aggressively cleaning up.

**Fix:** prune unreferenced images on the affected node directly via the container
runtime's own image-prune command (safe — it only removes images with zero running
containers, the same logic the automatic garbage collector uses, just run to completion
manually). **Don't trust `df` immediately after a prune** — the underlying filesystem
deletes the freed space asynchronously, so the real space reclaimed can land noticeably
later than the prune command returns.

### Draining a node here will hang without help — Longhorn + single-instance CNPG {#node-drain-blockers}

**Symptom:** `kubectl drain` on any node just sits there until it hits `--timeout`, then
fails, even though nothing looks obviously wrong.

**Cause:** two independent PodDisruptionBudgets sit at `disruptionsAllowed: 0` on every
node, for different reasons:

- **Longhorn `instance-manager` pods** are owned by a custom `InstanceManager` resource,
  not a DaemonSet — so `--ignore-daemonsets` does **not** skip them. Under the default
  `node-drain-policy=block-if-contains-last-replica` setting, their PDB blocks eviction
  outright.
- **Single-instance CloudNativePG clusters** (`service-db-pg`, `smarthome-db-pg` — see
  [Databases](../01-platform/databases.md)) have no replica to fail over to, so their
  `minAvailable: 1` PDB blocks forever, not just until a timeout. The supported unblock
  is `spec.nodeMaintenanceWindow: {inProgress: true, reusePVC: true}` on the `Cluster`
  CR, which tells CNPG to allow a controlled restart on another node instead of refusing.

**Fix:** don't hand-drain nodes here — use `ansible-playbook playbooks/cluster_update.yml`
(see [Ansible cheat sheet](ansible-cheatsheet.md#full-cluster-update-os--kernel--k3s) and
[docs/cluster-update-playbook.md](../../docs/cluster-update-playbook.md)), which drains
with `--disable-eviction` — bypassing every PDB directly (Longhorn's, CNPG's, anything
else) rather than negotiating with each — safe only because the playbook's own
pre-flight check already refuses to run at all if any volume is degraded, guaranteeing
full replica redundancy exists elsewhere first. It still sets CNPG's
`nodeMaintenanceWindow` for any single-instance cluster pinned to that node (so CNPG
reuses the PVC instead of treating the pod's disappearance as a failure), reverting it
afterward regardless of success or failure. A **bare pod with no controller** (no
Deployment/StatefulSet/Job owner) is a third, different case the playbook explicitly
hard-fails on before touching the node, rather than auto-resolving — `force_drain=true`
overrides this and deletes it for good.
