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

### ArgoCD application-controller gets OOMKilled during big syncs {#argocd-application-controller-oom}

**Symptom:** a sync of the big `monitoring` chart sits in *Running* for minutes with no per-resource progress
(about 13 minutes was seen once after the 2026-11-17 batch; 381 s on 2026-11-22), and the
`argocd-application-controller-0` pod restarts. On 2026-11-22 it was `OOMKilled` (exit 137) six minutes into that sync.

**Evidence (2026-11-22):** about 8 controller restarts in the previous 14 days (Prometheus
`kube_pod_container_status_restarts_total`). Working set peaks at roughly 1070 MB against a **1Gi limit** (it idles
around 530 Mi). The repo-server and its rendering sidecar had 0 restarts, so this is not the
[repo-server OOM](#argocd-repo-server-oom) above.

**Status:** open, not fixed. Likely fix: raise the controller memory limit in the `argocd/` chart values (watch
that the nodes with little free memory can take it, see [node memory asymmetry](#node-memory-asymmetry)). Re-check
the restart counter after any change.

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

### Snapshot cleanup stuck on a stale "rebuilding" replica {#longhorn-stuck-snapshot-cleanup}

**Symptom:** a volume holds far more snapshots than the ~25 that snapshot-2h (24 kept) plus
backups should leave, and the instance-manager log is full of `Failed to delete snapshot`
warnings. The Longhorn UI shows nothing wrong: the volume is `healthy` and every replica is `RW`.
This is **not** the cause of high instance-manager CPU: the loop ran for days while that pod sat at
its ~0.2-core baseline, and clearing it did not lower a later ~1-core load.

**Root cause:** one replica process kept an internal "rebuilding" flag after a rebuild that Longhorn
itself considered finished. Every snapshot removal on that volume was then rejected with
`cannot remove a snapshot because tcp://<pod-ip>:<port> is rebuilding` and retried in a loop
(thousands of times per day), while new snapshots kept piling up. Nothing in the CRs reflects
the flag, so robustness-based alerts stay silent.

**Likely trigger:** replicas killed mid-rebuild/purge by node and instance-manager restarts. The
first rejected removal was on the day of the k3s `KillMode` incident (see
[Node-stack gotchas](#node-stack-gotchas)), when every k3s restart killed all containers on the
node and the volume was rebuilt several times in a few hours. The exact step that left the flag
set is not proven. `KillMode=process` is now enforced by the ansible k3s units (live on the
nodes), so a plain k3s restart no longer takes Longhorn down; restarts that still do (reboots,
drains, instance-manager upgrades) can in principle cause this again, which is why the alerts exist.

**Diagnose:**

```bash
# which pod IP:port is blamed? (look at instance-manager logs on the loaded node)
kubectl -n storage logs <instance-manager-pod> --since=30m | grep "Failed to delete snapshot"
# map the port back to a replica
kubectl -n storage get replicas.longhorn.io -o json \
  | jq -r '.items[] | select(.status.port==<port>) | [.metadata.name,.spec.volumeName,.spec.nodeID]|@tsv'
# snapshots per volume
kubectl -n storage get snapshots.longhorn.io -o json | jq -r '.items[].spec.volume' | sort | uniq -c | sort -rn | head
```

**Fix:** delete that one replica CR (`kubectl -n storage delete replicas.longhorn.io <name>`; needs
another healthy replica, check first). Longhorn rebuilds it clean and cleanup catches up on its
own. To purge immediately instead of waiting for 04:30, POST `snapshotPurge` to the volume via the
Longhorn API from inside a `longhorn-manager` pod
(`/v1/volumes/<vol>?action=snapshotPurge`). Right after a rebuild you will see a few
`Failed to get change time and checksum ... .checksum: no such file` warnings; they are harmless.

**Alerts:** `LonghornVolumeTooManySnapshots` (>40 snapshots for 1h) and
`LonghornInstanceManagerHighCPU` (>0.8 cores for 1h) in `templates/prometheusrules.yaml`.

### `snapshot-2h` job fails for a day after racing `snapshot-cleanup` {#longhorn-snapshot-2h-cleanup-race}

**Symptom:** `KubeJobFailed` for a `storage/snapshot-2h-<id>` Job. The Job's pod log (in Loki, the pod is gone
from the cluster) says `timeouted waiting for the the snapshot snapshot-<uuid> of volume <vol> to be ready`,
repeated by every run for ~24 h (3 retries × ~33 min each), always naming the **same** snapshot UUID. All other
volumes snapshot fine and nothing looks wrong in the Longhorn UI.

**Root cause:** `snapshot-2h` (every even hour) and `snapshot-cleanup` (`snapshot-delete`, retain 0) fired in the
same second. `snapshot-2h` created a snapshot for the volume 1.3 s before the cleanup job started deleting
snapshots on that volume, and the new snapshot never became ready. Every later `snapshot-2h` run then waited on
that stuck snapshot first and timed out, until the next `snapshot-cleanup` run deleted it ("Cleaned up snapshot
CR ..."), after which snapshots resumed. The race itself is shown by timestamps; why the snapshot stays unready is
not proven. The alert lingered because the CronJob keeps one failed Job (`failedJobsHistoryLimit: 1`).

**Fix:** `snapshot-cleanup` now runs at :30 so it never overlaps the :00 `snapshot-2h` run. To recover sooner,
delete the stuck snapshot CR (`kubectl -n storage delete snapshots.longhorn.io <name>`) and then the failed Job.

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

### Longhorn RWX mounts hang when the share-manager runs on the MTU-9000 node {#rwx-share-manager-mtu-blackhole}

**Symptom:** a pod on a Longhorn RWX volume (seen with `zigbee2mqtt`) sits in
`ContainerCreating` with `MountVolume.MountDevice failed ... DeadlineExceeded`, or starts
and then blocks reading its data. The Longhorn volume is `attached`/`healthy`, the
`share-manager-<volume>` pod is `Running`, `nc` to the Service port works, and ganesha logs
no new client activity. Per node: `mount.nfs` / app processes stuck in `D` state with
`rpc_wait_bit_killable` in `/proc/<pid>/stack`, `ls` on the mount times out, and
`ss -tni` shows `advmss:1448 pmtu:1500` for the socket to the Service IP.

**Root cause (high confidence, reproduced with DF pings):** the VM node's virtual NIC
reports MTU 9000, so flannel there runs at 8950 (`FLANNEL_MTU` in `/run/flannel/subnet.env`),
while every other node runs 1500/1450. Packets larger than 1450 bytes between that node and
the others are **dropped silently** (no ICMP "fragmentation needed"). Host-network clients
(kubelet/CSI NFS mounts) advertise a 1448-byte segment size, so small RPCs
(connect, session setup, first mount call) work, but the first large reply from a
share-manager that runs on the VM node is lost and retransmitted forever. TCP stays
`ESTABLISHED` and every later RPC waits behind it. Pod-network clients advertise a smaller
segment size and are not affected.

**Trigger:** the RWX share-manager was scheduled on the VM node (it follows whichever
workload attaches the volume first; an app that mounts the RWX volumes there, such as the
`vscode-server` pod, decides this). The restart of the share-manager at the time of the
incident was a side event, not the cause: mounts were already failing before it. The exact
deleter of that pod is unknown (k3s has no API audit log).

**Diagnosis, in this order:**

1. Where does the share-manager run? `kubectl -n storage get pod share-manager-<volume> -o wide`.
2. Compare MTUs: `ip -o link | grep -E 'uplink0|flannel.1|cni0'` on every node.
3. DF-ping from the suspect node to a pod on another node: sizes up to 1422 pass, `-s 1472`
   gets 100% loss with no error when the MTU is wrong (`ping -M do -s <size> <pod-ip>`).
4. Check mounts and stuck processes per node (`ps -eo pid,stat,args | awk '$2 ~ /^D/'`,
   `sudo timeout 6 ls <mount>`).

**Symptom relief (does not fix the cause):** moving the share-manager off the VM node cured
it here. Scale the app to 0 (pause ArgoCD auto-sync first, selfHeal undoes the scale), let
Longhorn stop the share-manager, then on every node lazy-unmount all mounts of the Service IP
(`sudo umount -f -l <path>`), `kill -9` the hung `D`-state processes, run
`sudo ss -K dst <service-ip> dport = 2049`, confirm `/proc/fs/nfsfs/servers` no longer lists
the server, and scale back up. Per-mount patching without moving the share-manager only
works until the next large reply.

**Permanent fix (applied 2026-11-24):**

- Ansible: `networkd_migrate_mtu` (default 1500) is rendered as `MTUBytes=` into the uplink
  `.network` file, so every node set up with the role gets the same MTU
  (`ansible/roles/networkd_migrate`). The Pi node is managed by NetworkManager and
  already runs 1500 (no change needed).
- Applied by hand to avoid a drain/reboot: `MTUBytes=1500` added to the uplink `.network`
  file on the networkd nodes (no reload, takes effect at next boot, backup kept as
  `10-uplink.network.bak-mtu`), and on the VM node live with
  `ip link set dev uplink0 mtu 1500`, `flannel.1` and `cni0` to 1450.
- Check afterwards: DF pings from the VM node to a pod on another node now pass up to 1422
  and fail locally with `Message too long` above that, instead of vanishing silently.
- The k3s service on that node (`k3s-node`) was restarted without downtime: flannel now
  reports `FLANNEL_MTU=1450` by itself and all pods kept running.
- Alert `NodeMtuMismatch` (`system/monitoring/monitoring/templates/prometheusrules.yaml`,
  group `network-mtu.alerts`): fires after 10 min when `flannel.1`, `cni0` or `uplink0` has a
  different MTU on different nodes (`max` vs `min` of `node_network_mtu_bytes` per device),
  no hard-coded value. Check it is quiet with that query in Prometheus.
- **Also caused:** the `cert-push` job failed on 2026-11-20 and 2026-11-24, only when scheduled on the VM node (TLS handshake to the
  gateway dies with `unexpected eof`, NAS logged `dropped over-mtu packet` at the same second). See
  [cert-push troubleshooting](../01-platform/cert-push.md#cert-push-mtu). The VM's tap interface on the NAS must also stay at 1500.
- **Still open:** pod `veth` interfaces on the VM node stay at 8950 until their pods are
  recreated. Pods there now get proper "too big" feedback, so it is no longer a black hole,
  but recreate them at the next maintenance window. Also worth doing: keep workloads that
  mount RWX volumes off the node that should not host share-managers.

**Related hazard seen the same evening:** with a node cordoned, the descheduler's
`RemovePodsViolatingNodeTaints` keeps evicting 3 pods per 2 min from it, including Longhorn
CSI/manager pods and share-managers (`storage` is only excluded under `LowNodeUtilization`).

Do **not** debug this with in-tree `nfs:` volumes in a probe pod: they fall back to the
portmapper (port 111, not exposed by the Service) and hang for an unrelated reason.

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

### Arr apps start on SQLite the first time and only use Postgres after one more restart {#arr-first-start-sqlite}

A new Radarr/Sonarr/Lidarr/Prowlarr deployment with `arrXmlPostgres` first starts on its default **SQLite**: the init
container that writes the Postgres settings into `config.xml` finds no file yet and skips ("config.xml not found yet,
skipping Postgres bootstrap"). The app then creates `config.xml`, and the settings are only applied on the **next**
start. Fix: restart the pod once (`kubectl -n arr-stack delete pod <app>-0`) and check `/api/v1/system/status` for
`databaseType: postgreSQL`. Nothing is lost, because a fresh app has no data yet; do it before configuring the app.

### The Sonos Roam keeps dropping out of a group {#roam-drops-in-bridged-groups}

Symptom: audio stops for a moment every few seconds on the Roam while the group plays. The Roam's Wi-Fi is fine
(5 GHz, strong signal, days of uptime) and it never goes `unavailable`. Cause: once a group was formed with a
non-Sonos player (the web player), Music Assistant drives **every** Sonos in it through its AirPlay/Sendspin
bridge, and that output stays selected for as long as the players stay grouped, even after the web player left.
The Roam is far slower to start an AirPlay 2 stream than the wired or mains-powered Sonos (it lags 1.4-3 s every
time, `device could not be audible`), so Music Assistant keeps cold-restarting it. Fix: stop playback, ungroup
(`players/cmd/ungroup_many`) so the active output protocol clears when the players are idle, then regroup the
Sonos players **natively** (`players/cmd/set_members` on the leader). Keep the web player out of Sonos groups (or
at least the Roam). A phone or browser player (Sendspin) can join such a group, but the picker only lists it on its own device while its
setting **Hide this player in the user interface** is on (the default, together with `private`): turn that off in the
player's settings to see it from every device. Joining a Sonos group makes Music Assistant bridge **all** members
(and it re-adds the Roam if you remove it), so a group with a phone is the case where the Roam drops. Music Assistant
also auto-registers every AirPlay receiver it finds, for example a Mac, and
retries to join it with `403` errors in the log; disable such entries under the player settings.

### Music Assistant on Kubernetes: Sonos plays nothing unless the published IP is pinned {#music-assistant-published-ip}

Music Assistant officially requires host networking and lists Kubernetes as unsupported. In this cluster
it reaches the Sonos players (VLAN 5) through a Multus macvlan leg. Its "auto" detection of the address it
hands to players takes the source address of the **default route**, which in the pod is `eth0` (the cluster
IP), so a player is told to fetch the stream from an address it cannot reach and playback silently fails.
Fix: **Settings → Core → Streams (advanced) → Published IP address** = the `net1` address, and give that stub
a UniFi alias plus fixed IP so it does not drift. The same two-interface trap breaks the Spotify "Use the
Spotify app" pairing (advertised on the wrong IP): use the browser login. Details:
[Music](../03-media-stack/music.md).

### Grouping a Sonos with the web player drops the Sonos: `cliairplay did not connect` {#music-assistant-airplay-bridge-routing}

Playing one stream on Sonos **and** the Music Assistant web player makes Music Assistant bridge each Sonos
over AirPlay/Sendspin. That needs the speaker to connect *back* to Music Assistant, but with the `sbr`
chained plugin the pod reaches VLAN 5 from its cluster IP (NAT'd), so the announced address is unreachable and
the Sonos leaves the group after about 30 seconds (`Woonkamer gave up again within 30s of being re-joined`).
Sonos-only groups use native grouping and are unaffected, which is why it looks intermittent. The chart's
`setup-vlan5-route` init container adds an on-link route for the VLAN 5 subnet via `net1`. Adding that route
live to a running pod makes every existing Sonos connection drop once (all players log `Disconnected from
player API`, and a portable like the Roam can be removed from the group); the pod recovers by itself.

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

### An hourly burst of refused reverse lookups is Home Assistant scanning its own pod subnet {#ha-dhcp-discovery-ptr-burst}

**Symptom:** the DNS cluster logs ~250 `REFUSED` PTR queries within one second, once an hour at the
same second every hour, for addresses in a single node's pod range, nearly none of them live pods.
They arrive from a *node* address, not from the pod, because CoreDNS forwards them and the
overlay SNATs them on the way out. About 7 % of all queries on the primary.

**Root cause:** Home Assistant's `dhcp` integration runs a network watcher every 60 minutes. It
uses the `aiodiscover` library, which ARP-scans and then looks up a PTR record for **every host
address of the pod's default-route interface** (the pod subnet, not the LAN), asking each
nameserver in turn about what is still unresolved. The hourly timer starts when HA reports
"started", so its minute and second are fixed by when the last slow integration finished
starting, and a HA restart moves it. Confirmed with a packet capture on the node (the pod, 253
distinct names plus an ARP sweep inside one second) and by reading the integration source.

**Fix applied (2026-11-06):** the Home Assistant chart routes the library's probe address over the
pod's VLAN 5 interface, so the hourly sweep now covers the LAN `/24` (which the DNS cluster answers
properly) instead of the pod range. See the Home Assistant page. **Other options considered:** harmless, so leaving it is valid. To stop the refusals, give the DNS cluster an
authoritative empty reverse zone for the pod range so the lookups return `NXDOMAIN` instead of
`REFUSED` (aiodiscover treats a refusing nameserver as failed and retries). Changing HA itself
(removing `dhcp` from the default config) also stops the sweep but affects DHCP device discovery
and is not recommended. **How it was found:** an identical hour-after-hour timestamp means a
timer, not an event; a small read-only packet capture on the node attributed the queries to the
pod. Rule out restarts and evictions first, they do not produce an hourly rhythm.

### UniFi/network-controller API: two different APIs, and only one can write {#unifi-network-controller-api}

If scripting against a UniFi-style network controller: its documented/modern API is
read-only for client records (no alias, no fixed-IP field) — writing a client alias or a
fixed IP reservation requires the older, undocumented legacy API instead. Reserving an
address still held by another client fails until that client is explicitly forgotten
first, and a forgotten client's record can reappear on any residual traffic from it.


### UniFi Matter/Thread monitoring has an undocumented API {#unifi-matter-api}

The official Network API spec (checked v10.6 and v11.0) has **no** Matter or Thread endpoint, but the web UI uses one
that also accepts the normal API key through the cloud console proxy (no browser cookie or CSRF token needed).
Base path: `<console-proxy>/proxy/network/v2/api/site/default/matter/nodes`.

| Call | What it does |
|---|---|
| `GET .../matter/nodes` | List enrolled nodes: `node_id` (UniFi's own number, starts at 1000000), `device_name`, `name` (custom, null if unset), `display_name`, `status`, `channel`, `error_code`, `setup_code` (treat as a secret) |
| `POST .../matter/nodes` `{"setup_code": "<11-digit manual code>"}` | Enrol a device. Returns `202` at once; poll the list: `COMMISSIONING` then `PROVISIONING` then `COMMISSIONED` (about 30 s for a sleepy sensor) |
| `PUT .../matter/nodes/<node_id>` `{"name": "<text>"}` | Rename. `{"name": null}` **clears** a custom name, so it is only a harmless probe on unnamed nodes; to read a node's details without changing anything, send its current name back. The response is the full client record, including `matter.basic_info.unique_id` |

`DELETE .../matter/nodes/<node_id>` removes a node (`204`); used to clean up `FAILED` / `COMMISSIONING_REJECTED` leftovers.

Gotchas: `GET`, `PATCH` and `POST` on a single node return `405`, `OPTIONS` returns `404`; the code is only valid while the device's
commissioning window is open (300 s from matter-server); **the join key between UniFi and matter-server is the Matter `UniqueID`**
(UniFi `basic_info.unique_id` = matter-server attribute `0/40/18`) **for most devices, but not for IKEA TIMMERFLOTTE temperature
sensors: they report a different UniqueID per fabric** (UniFi shows a 32-character hash, matter-server a 16-character value), so the
join never matches. A UniFi node record also lists the device's `ipv6_address` values, which equal matter-server's
`0/51/0` interface IPv6 list; those are the fallback key (ignore the RLOC address `...:0:ff:fe00:xxxx`, it changes with the mesh
topology). Home Assistant identifies some devices as `serial_<n>`, which equals matter-server attribute `0/40/15`, and others as
`deviceid_<fabric>-<node id as 16 hex>-MatterNodeDevice`. A code comes from matter-server's WebSocket command `open_commissioning_window`
(`setup_manual_code` in the result); matter-server serves only a WebSocket on its port, no REST. UniFi monitors Thread devices only.
**Enrolments are queued by UniFi**: two `POST`s in a row both return `202` with status `QUEUED`, and the second device is not a Matter node yet (a `PUT` on it returns `404 MatterNodeNotFound`) until the first has finished, so enrol strictly one device at a time and name it only after its own commissioning (about 40 s for a sleepy sensor). Pilot result: one door sensor enrolled cleanly (fabric count 2 to 3 of 5, no border-router or matter-server warnings).

### IPTV multicast never crosses the ISP box's internet bridge {#iptv-multicast-never-crosses-the-isp-box-bridge}

With VDSL and the ISP's Internet Box bridging the internet session to the UDM, the UDM's IGMP
proxy can send perfect joins upstream (any IGMP version) and **still receive zero multicast**:
the box only delivers TV multicast to devices on **its own LAN side**. The symptom is subtle —
live TV *starts* but stutters, because the decoder keeps joining/leaving and falls back to the
ISP's short unicast fast-channel-change stream; replay/recordings (plain unicast) are perfect.
Fix: a second UDM WAN (failover-only) cabled to a box LAN port, IGMP proxy upstream on that WAN.
Prove it first with a packet capture on the UDM, not with switch counters — the UDM's own WAN
port doesn't populate its multicast counters at all (always 0). Full story:
[Routed IPTV](../01-platform/iptv.md).

### IGMP snooping breaks live TV when a switch querier competes with the IGMP proxy {#igmp-snooping-competing-querier}

On the TV VLAN, IGMP snooping with the switches configured as querier made live TV drop out
within a minute or two. The UDM's IGMP proxy already acts as querier on that VLAN; a second
querier on the switches fights it. Snooping **with an empty switch-querier list** (fast-leave
off) works on every decoder and stops flooding each TV's channel to the other. Test snooping
changes for at least 2–3 minutes per decoder — the failure only shows at the first membership
refresh.

### Multicast over a UniFi wireless-mesh link saturates the channel {#multicast-over-wifi-mesh}

A wired device behind a **meshed** (cable-less) AP receiving a multicast stream (IPTV) makes the
mesh link send it as broadcast at the lowest basic rate: one ~7 Mbit/s stream ≈ 90% of the 5 GHz
airtime. A better mesh parent and the SSID "minimum data rate" don't help — the mesh backhaul
ignores SSID settings. What fixed it: the UDM's Linux bridge flag `mcast_to_unicast` on the
VLAN's bridge ports (unicast copy per IGMP listener → full PHY rate, airtime ~22%). UniFi resets
that flag on every reprovision/reboot, so it's kept alive by a watchdog with a Home Assistant
dead man's switch — see [Routed IPTV → the watchdog](../01-platform/iptv.md#keeping-it-on-the-watchdog).

---

### NAS reverse-proxy host works by IPv4 / curl but shows the DSM "does not exist" page in a browser {#nas-reverse-proxy-ipv6-acl}

**Symptom:** a NAS reverse-proxy hostname (the registry mirror) answers over the IP and in `curl`, but a browser gets the Synology
"does not exist" page.
- **Cause:** the hostname has an AAAA record and the reverse-proxy rule listens on `[::]:443`, so browsers connect over IPv6.
  The rule's *Access Control Profile* (the "allow local" profile) listed only IPv4 subnets followed by `deny all`, so every IPv6
  client got a 403, which DSM renders as that error page. `curl` defaults to IPv4 here, so it passed.
- **Fix:** add the IPv6 ULA `/64` of each VLAN that should reach it to the profile (Control Panel, Login Portal, Advanced, Access
  Control Profile), before `deny all`. Use the stable ULA prefixes, not the ISP-delegated public ones (those can change). Only
  VLANs with IPv6 enabled on the gateway need an entry. Verify with `curl -6 -k https://<host>/` and `curl -4 …`; the generated
  rule lives in `/usr/local/etc/nginx/conf.d/.acl.<id>.conf` on the NAS. Dropping the AAAA record is the alternative.

## Databases

### A forgotten psql session blocks vacuum and pins a Longhorn instance-manager {#stuck-psql-session-vacuum}

**Symptom:** the Longhorn `instance-manager` on the node hosting the Home Assistant database sits at
~1 core (baseline ~0.2) with no log output, node iowait/system/softirq are high, and one iSCSI device
reads tens of MB/s. `LonghornInstanceManagerHighCPU` fires.

**Root cause:** an ad-hoc `psql ... COPY ... TO STDOUT` (started through `kubectl exec`) lost its
client and stayed `active / ClientWrite` for days. Its open snapshot stopped vacuum from removing any
row deleted after it started, so after the nightly recorder purge (millions of deleted `states` rows)
autovacuum rescanned the whole 9 GB table every minute without reclaiming anything (298 runs, 5 M
dead tuples). Those reads go through the Longhorn engine, which is where the CPU showed up.

**Diagnose:**

```bash
# long-running sessions pinning the xmin horizon
kubectl -n smarthome exec <pg-pod> -c postgres -- psql -U postgres -c \
  "select pid, application_name, state, wait_event, now()-xact_start as age, age(backend_xmin) from pg_stat_activity where backend_xmin is not null order by age(backend_xmin) desc"
# dead tuples / vacuum churn on the recorder tables
kubectl -n smarthome exec <pg-pod> -c postgres -- psql -U postgres -d homeassistant -c \
  "select relname, n_live_tup, n_dead_tup, autovacuum_count from pg_stat_user_tables order by n_dead_tup desc limit 5"
```

**Fix:** `select pg_terminate_backend(<pid>)` for the stale session, then a plain
`VACUUM (VERBOSE) public.states` (not `FULL`: it does not block reads/writes). That removed ~5 M dead
rows and the instance-manager dropped back to ~0.2 cores within minutes. Prevention: run ad-hoc exports
with `statement_timeout`/`timeout` or write to a file inside the pod instead of streaming to a client that can disappear.
The `PostgresLongRunningTransaction` alert (>1h) now flags such a session, and the HA cluster's
autovacuum scale factors are 0.05 (default 0.2) so vacuum runs earlier and in smaller passes.

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

### SNMP scrape of a remote NAS times out {#snmp-remote-nas-timeout}

**Symptom:** `snmp-exporter` logs `request timeout (after 3 retries)` for one NAS while another NAS
(and SNMP from a laptop, and even `snmpwalk` on small subtrees) works fine; TCP to the NAS is fine,
firewall zone policies already allow the traffic, a UDP SNMP discovery probe gets a reply.

**Cause:** the exporter asks for 25 rows per GETBULK. For the hrStorage subtree (long mount paths)
the NAS replies with a UDP datagram bigger than the path MTU; the resulting IP fragments never
arrive over the site-to-site link (ICMP larger than ~1400 bytes also fails end to end), so the
request times out every time — deterministic, independent of the timeout value. 10 rows per
request (or fewer) always fits.

**Fix in place:** `max_repetitions: 10` on the `synology` module. If a *new* module times out
mysteriously on one device only, test the same walk with `snmpbulkwalk -Cr25` vs `-Cr10` from a pod
before blaming the firewall.

### Local `helm template` can lie about what ArgoCD will actually render {#helm-4-local-render-gap}

ArgoCD's rendering plugin is pinned to Helm 3, so a newer major Helm locally can differ
from what renders in cluster. As of Helm 4.2.x the only known gap is how a `null` override
of a subchart default is merged (e.g. clearing a subchart's default `resources`): the
value may survive locally but not in ArgoCD, or vice versa. If a local `helm template`
doesn't show a change you expect, compare against `helm3` before concluding the template
is broken.

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
and the pod scales to zero **out from under a still-open client session**, killing it.

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
[docs/cluster-update-playbook.md](https://github.com/Coollision/homelab-config/blob/master/docs/cluster-update-playbook.md)), which drains
with `--disable-eviction` — bypassing every PDB directly (Longhorn's, CNPG's, anything
else) rather than negotiating with each — safe only because the playbook's own
pre-flight check already refuses to run at all if any volume is degraded, guaranteeing
full replica redundancy exists elsewhere first. It still sets CNPG's
`nodeMaintenanceWindow` for any single-instance cluster pinned to that node (so CNPG
reuses the PVC instead of treating the pod's disappearance as a failure), reverting it
afterward regardless of success or failure. A **bare pod with no controller** (no
Deployment/StatefulSet/Job owner) is a third, different case the playbook explicitly
hard-fails on before touching the node, rather than auto-resolving — `node_update_force_drain=true`
overrides this and deletes it for good.

### Network-stack and k3s restart gotchas (networkd move) {#node-stack-gotchas}

Three separate causes surfaced while moving the nodes to systemd-networkd ([Networking](../01-platform/networking.md#network-stack)).

**k3s will not start after a reboot: "this server is not a member of the etcd cluster".**
- **Cause:** k3s picks its node/etcd IP once, at start. Under ifupdown, `networking.service` blocked until DHCP returned an IPv4
  address. `systemd-networkd-wait-online` counts a SLAAC IPv6 address as online, so k3s started first, took the IPv6, and the
  existing etcd member (registered on the IPv4) refused it.
- **Fix:** `RequiredForOnline=routable` + `RequiredFamilyForOnline=ipv4` on the uplink so `network-online.target` waits for the
  IPv4, and `node-ip` pinned in the k3s config. A one-off recovery is `systemctl restart k3s` once the IPv4 is up.

**DNS is dead right after installing `systemd-resolved`.**
- **Cause:** the Debian package's postinst replaces `/etc/resolv.conf` with a symlink to the stub at once, while resolved is still
  stopped and has no upstreams.
- **Fix:** install it only inside the cutover (after the backup is taken and the revert timer is armed), and rewrite a plain
  `resolv.conf` by hand if it ever happens outside it.

**A k3s restart killed every container on the node (Longhorn engines died, volumes faulted).**
- **Cause:** the k3s unit template had `KillMode=process` on its own line, but an old `{% endif %}` swallowed the newline (Ansible
  `trim_blocks`), gluing it onto `ExecStart` as a stray argument. systemd ignored it and used `control-group`, so restarting
  `k3s` / `k3s-node` killed all pods on the node. Reboots and `cluster_update` were unaffected because they drain first.
- **Fix:** the template is split correctly, the live units were re-templated, and the k3s roles assert a standalone
  `KillMode=process` line.
- **Rule now:** with `KillMode=process` a plain `systemctl restart k3s|k3s-node` only restarts the k3s process; containerd shims
  and their containers (Longhorn instance-managers included) keep running, so **no cordon/drain is needed for a k3s-only
  restart** (config, `registries.yaml`, k3s binary). Verified 2026-11-03: all nodes report `KillMode=process`, and on the
  server node a container shim started before the last k3s restart is still alive. Still drain for reboots, kernel/OS
  upgrades and anything that touches iscsid/network. Before relying on it, check `systemctl show k3s-node -p KillMode`
  (`k3s` on servers); if it says `control-group`, the unit has drifted and a restart kills every pod. Restart one node at a
  time and wait for Ready plus healthy Longhorn volumes before the next.

### A VM worker node hangs with root-disk I/O errors: ping works, SSH resets {#vm-node-disk-io-hang}

One of the workers is a KVM guest on the NAS (Synology Virtual Machine Manager), not a physical box. Its virtual disks are
file-backed LUNs on a single NVMe volume on the NAS, exported to the guest over vhost-scsi. **Seen once, 2026-11-23, first time in
years. Trigger not proven; this entry records what was found and what to check next.**

**Symptom**
- Node `NotReady`, kubelet silent since ~04:00. Alerts: `KubeNodeNotReady`, `KubeNodeUnreachable`, `KubeletInstanceUnreachable`,
  `TargetDown`.
- The node still answers ping and accepts a TCP connection on port 22, but sshd resets the connection during key exchange
  (`kex_exchange_identification: Connection reset by peer`). The guest cannot read its own files, so sshd cannot start a session.
- Node-exporter metrics were healthy right up to the stop (memory, load, iowait 0, disk space free). Nothing trended.

**How to confirm it is this and not a dead VM**
- Take the guest console screenshot from the NAS (`virsh screenshot <guest-uuid>`; the output is a PNG). It showed
  `systemd-journald: Failed to write entry ... Input/output error` repeating. Loki shows the kubelet failing to open
  `/var/log/pods/...` with `input/output error` right after its last healthy line.
- `virsh domblkerror` and `virsh domstate` look clean (`running`, no errors) because the disks are vhost-scsi LUNs, not libvirt
  block devices. A clean answer there does not rule this out.
- NAS kernel log: a burst of `vhost_scsi_get_tag failed -12` (about ten lines in ~200 ms), ~7 minutes before the guest's last
  healthy log. The wall-clock mapping of dmesg time can drift by minutes.

**Fix (the recovery that worked)**
- Force power-cycle the guest through VMM's own API rather than `virsh destroy` (which can hang on the vhost fd):
  `synowebapi --exec api=SYNO.Virtualization.API.Guest.Action version=1 method=poweroff guest_id=\"<uuid>\"`, then `method=poweron`.
  Quote the UUID as a JSON string or it is parsed as a number and the call fails with error 401.
- The node was `Ready` ~30 seconds after power-on, no disk errors since boot, all pods `Running`. Longhorn then rebuilt the
  replicas that lived on the node for roughly 15+ minutes (`degraded` -> `healthy`).

**What the cluster did (as designed)**
- Pods on the dead node sat in `Terminating` until it returned (a dead node cannot confirm deletion). Replacements started on
  the other nodes after the default 5-minute not-ready toleration, with no manual action.
- Single-replica workloads were down roughly 6-7 minutes in total. The monitoring stack (Prometheus, Loki, Grafana) lived on the
  node too: Loki was back after ~18 minutes, Grafana ~25, and there is a ~20 minute gap in the metrics. Alertmanager ran
  elsewhere, so alerts still fired.
- Afterwards the Longhorn recurring-snapshot job reported one failure (`KubeJobFailed`) from the outage; expect that.

**Root cause: leading theory, medium confidence (independent review agreed)**
The NVMe volume stalled on writes. Commands piled up in the backend until the vhost-scsi per-session **tag pool** ran out
(`-12` is "no tag", not "no memory"). vhost answers each refused command with `BAD_TARGET`, which the guest does not retry, so
about ten failed writes were enough for ext4 to abort its journal. From then on every write returned EIO and the node died.

Supporting: right after the reboot, with only Longhorn rebuild traffic (~50 MB/s), the NVMe showed ~850 ms average write
latency with hundreds of I/Os in flight. That points at a weak sustained-write ceiling on a consumer DRAM-less drive on a
volume that is 75% full.

**Ruled out or unlikely**
- NAS RAID / volume health (all arrays clean), kernel storage errors (none), NVMe SMART (0 media errors, 0 error-log entries,
  no thermal time, 35 °C).
- The scheduled HDD SMART long tests at 03:00: the HDDs are a different block path from the VM volume.
- btrfs snapshot pruning on the VM volume: 2 snapshot drops in the kernel ring vs. 131 and 66 on the other volumes. The LUN files
  are also NOCOW (`C` attribute), so CoW fragmentation is not the story.
- Plain RAM shortage: the guest's RAM is pinned (~6.3 GB) and swap was in use on the NAS, but that does not produce `-12`.
- No NAS job could be tied to the exact minute; the only log line near the time is the 03:00 SMART scheduler. HyperBackup was
  not in the Nov 23 window.

**Is the drive worn out?** No. Drive health is fine; this is a performance question, not an end-of-life one.
- `Percentage Used: 23%` is endurance consumed, so ~77% is left. Lifetime writes are ~88 TB against a 300 TBW rating; at the
  recent rate of roughly 90 GB/day that is on the order of six years of headroom. Spare 100%, media errors 0.
- The 75% is *filesystem fullness* on the volume, not wear. It matters because a DRAM-less drive leans on free space for its
  write cache and garbage collection, so a fuller drive stalls sooner under sustained writes.
- TRIM does not reach the drive: the volume is mounted without `discard`, and the VM's vdisks have `unmap` off, so deletions inside
  the guest never tell the SSD which blocks are free. This is a plausible contributor, not a proven one.
- **The other NVMe in the NAS is the more worn one** (DSM remaining life 36%, ~244 TB written, vs. 77% and ~88 TB for the VM
  drive). Both were installed together (36,118 power-on hours, ~4.1 years). It is not the VM volume and showed no errors. Its
  lifetime write rate averages ~160 GB/day but only ~40 GB/day since the NAS last booted, so projected end of rated life is
  anywhere from ~1 year (if the 300 TBW rating is the limit and the old rate returns) to ~9 years (recent rate, drive's own
  scale). Plan a replacement within 1-3 years and re-check every 4 months
  ([Recurring checks](../06-todo/recurring-checks.md#nas-nvme-check)). The VM drive is ~7-14 years out.
- **Why the other drive's write rate fell (likely, not proven):** its volume used to hold the cluster's old NFS storage
  (a `k3s-volumes` share, ~115 GB still there, untouched since April-August 2026) and an iSCSI LUN directory that is now
  empty. Git shows the move to Longhorn ran from 2026-03-15 (monitoring first, a heavy writer) through the app migrations
  of 2026-04-02/03 and the databases in June. If the ~40 GB/day since the last NAS boot held since mid-March, the earlier
  average was ~185 GB/day. The NAS cannot attribute writes per volume, so this is inferred from timing, not measured. The
  same writes now land, doubled by two replicas, on the VM drive.
- **Measured latency, 2026-11-23 (30 s sample, quiet system):** VM drive 22 MB/s writes at ~41 ms average write latency; the other
  drive 2 MB/s of tiny writes at ~72 ms. Lifetime average write latency since NAS boot is ~3.8 ms and ~5.2 ms, which is high
  for NVMe and the same on both drives, so treat it as a platform trait as much as a drive fault.

**Next steps (nothing is changed yet; prevention is deliberately held back until it happens a second time)**
1. Record an NVMe baseline (done 2026-11-23, see [Recurring checks](../06-todo/recurring-checks.md#nas-nvme-check)) and
   re-check every 4 months: the NAS's `synonvme --smart-info` for both drives (Percentage Used, media
   errors, error-log entries, unsafe shutdowns).
2. Sample `/proc/diskstats` on the NAS for a few minutes during a quiet period and compute average write latency. Normal at idle
   but ~850 ms under rebuild means a load-dependent ceiling; high at idle means the drive or its link is the problem.
3. ~~Check the PCIe link.~~ Done: both drives are capable of 8 GT/s x4 but run at 5 GT/s x1. It is identical on both, so it is the
   slot's limit, not a fault. It caps throughput at roughly 400-500 MB/s, which is not what stalled here (~50 MB/s load).
4. In a maintenance window with the VM stopped, write a large scratch file to the volume with `dd oflag=direct` and watch for
   the throughput cliff when the SLC cache is exhausted. Do not run this while the VM is up.
5. Reclaim space. The guest uses only ~89 GB of its 300 GB disk, but the NAS-side file for it has ~283 GiB allocated, because the
   vdisks have `unmap` off and the guest reports no discard support (`lsblk -D` shows 0B), so freed blocks are never returned. That
   is ~190 GB reclaimable, which would take the VM volume from 75% to ~30% full. Plan: wait until Longhorn is fully healthy, drain
   and gracefully shut down the node (see the node-drain entry above), enable **Unmap** on the VM's virtual disks in VMM (VM >
   Edit > Virtual Disk > edit the big disk > Unmap; the checkbox is greyed out while the VM runs, so it must be off. Do it in
   the UI: the API `set` replaces the whole VM definition and is easy to get wrong), power on, confirm `lsblk -D` shows a discard granularity,
   then run `fstrim -v` on `/` and on the Longhorn data mount, one at a time, in a quiet window (not ~03:00). The guest's weekly
   `fstrim.timer` has never run a useful trim yet, so the first run is large; a big discard burst on a stressed DRAM-less SSD can
   itself stall, so do not skip the quiet window. Afterwards re-check the volume's free space and the drive's latency.

   **Done 2026-11-23.** Unmap enabled, node drained and shut down gracefully, trimmed after Longhorn was healthy. The VM volume
   went from 324 GB (74%) to 121 GB (28%) used, about 200 GB returned. The first trim (33 GiB on the root filesystem, in one go)
   reproduced the stall signature on the NAS drive: write latency 272 ms, then 624 ms, then 1.5 s, with up to ~2,100 I/Os queued,
   recovering within ~40 s with no tag exhaustion and no guest errors. So the Longhorn disk was trimmed in 6 GiB chunks (~8 minutes,
   ~173 GiB). Caveat: the NAS-side latency monitor for the chunked run produced no data (its abort thresholds were not active), so
   the chunk latencies are unknown; afterwards the guest and NAS logs were clean, no new `vhost_scsi` errors, Longhorn healthy.
   **Keep in mind:** the guest's weekly `fstrim.timer` (Mondays, shortly after midnight) now actually discards. If a week's
   deletions are large it can cause the same kind of stall, so watch the first runs and, if needed, replace the timer with a
   chunked, throttled trim.

**If it happens again, capture before power-cycling**
- Guest console screenshot; NAS `dmesg | grep vhost_scsi`; two `/proc/diskstats` samples 10 s apart (write latency per drive);
  the NAS memory (`free`) and any `D`-state threads (`ps -eo stat,comm,wchan | awk '$1 ~ /^D/'`).
- Then power-cycle through the VMM API, and only then consider the prevention options: raise the guest SCSI disk timeout from 30 s
  to 120-180 s via a udev rule (so a short stall does not offline the disk), lower the guest's RAM, move heavy NAS jobs away from
  the early hours, and alert on `vhost_scsi` lines in the NAS kernel log.

## Smart home / Zigbee

### Ember rejects a Z-Stack backup with more link keys than the NCP's key table {#ember-key-table-too-small}

**Symptom:** Zigbee2MQTT exits at start with `[BACKUP] Current key table of 20 is too small to import backup of 58!` right
after `[INIT TC] Forming from backup`. No network is formed on the adapter; the pod restarts in a loop.

**Root cause:** herdsman's `importLinkKeys` requires every per-device link key in `coordinator_backup.json` to fit the NCP's key
table. The size is a firmware build setting (20 on the Sonoff Dongle Max with EmberZNet 9.1.0.0; community MG24 builds use 12) and
cannot be raised from Z2M (`stack_config.json` has no such key). A Z-Stack backup carries one derived link key per paired device,
so a converted backup of a large network easily exceeds the table. This is about stored per-device keys, not the number of devices
on the network.

**Fix:** trim `devices` in the *converted* backup to at most the table size (keep the devices that are hardest to re-pair, and leave
at least one slot free), then restart. The log then shows `Imported N keys` and the restore continues. Devices without a key entry
kept working on the network key; of 58 devices, 2 needed a manual re-pair afterwards. Keep the full converted backup. No
larger-table firmware for this adapter was found (Sonoff's firmware repo only covers the ZBDongle-E, the community builder has no
Dongle Max target).

### Z2M loses the Dongle Max seconds after the first start (ASH ack timeout) {#z2m-max-ack-timeout}

**Symptom:** about 2 s after a successful start, the log shows `Received ERROR from adapter, with code=ERROR_EXCEEDED_MAXIMUM_ACK_TIMEOUT_COUNT`,
`ASH_NCP_FATAL_ERROR`, `Adapter disconnected, stopping`, and the pod restarts. The NCP retransmits the same frames (`frmTx` repeated).

**Root cause (not proven):** the host's ACKs do not reach the NCP over the Max's TCP serial bridge. Similar reports upstream:
[Z2M #33574](https://github.com/Koenkk/zigbee2mqtt/issues/33574) (Dongle Max over TCP, disconnects on 2.14.3, stable on 2.14.2) and
the open Sonoff firmware repo issue about the serial-over-TCP bridge dropping downlink to sleepy end devices. It happened once, on
the first start after the network restore; the pod has been stable since.

**Fix / watch:** the pod restart cleared it. If it recurs: pin Z2M to 2.14.2, check the Max serial-over-network settings, and fall back
to the USB stick if needed (rollback steps on the [Zigbee page](../02-smarthome/zigbee.md)).
