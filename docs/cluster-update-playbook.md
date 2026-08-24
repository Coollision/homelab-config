# `cluster_update.yml` — combined OS/kernel + k3s upgrade playbook

**Goal:** patch OS packages, the kernel, and the k3s binary version across all three
nodes with exactly **one** cordon/drain/uncordon cycle per node — not two separate
drains for "OS update" and "k3s update" — and without a plain `kubectl drain` hanging
on this cluster's Longhorn/CNPG PodDisruptionBudgets (see
[Known Issues](../wiki/05-operations/known-issues.md#node-drain-blockers)).

Lives at `ansible/playbooks/cluster_update.yml` + roles `node_update` and
`cluster_wait_stable`. Usage/commands: [Ansible cheat sheet](../wiki/05-operations/ansible-cheatsheet.md#full-cluster-update-os--kernel--k3s).

## Why one drain, not two

The obvious shape is two playbooks — "patch OS" then "bump k3s" — each with its own
drain. That's what this started as. It means every node gets evicted and rescheduled
**twice** per maintenance cycle for no reason: the same packages are touched, the same
Longhorn/CNPG drain dance has to happen twice, and it roughly doubles pod-reschedule
churn for zero benefit. Merged into one role: cordon → drain once → OS `dist-upgrade` →
k3s binary swap → reboot (if the kernel changed) or a plain service restart (if only
k3s changed) → wait Ready → uncordon → wait for the cluster to actually settle.

**Node order is asymmetric on purpose.** With a single control-plane node there's no HA
upside to doing workers first anymore now that both concerns share a pass — and k3s's
own upgrade guidance is servers before agents (kubelet should never run newer than the
apiserver). So: **master first, then workers**, `serial: 1`, `any_errors_fatal: true`.

## The three real drain blockers found on this cluster

Discovered empirically (not from docs) by running the report phase against the live
cluster — every node had all three simultaneously:

1. **Longhorn `instance-manager` pods** are owned by a custom `InstanceManager`
   resource, not a DaemonSet, so `--ignore-daemonsets` does nothing for them. Under
   this cluster's `node-drain-policy=block-if-contains-last-replica` setting their PDB
   sits at `disruptionsAllowed: 0` on every node, unconditionally, healthy volumes or
   not.
2. **Single-instance CNPG clusters** (`arr-stack/service-db-pg`,
   `smarthome/smarthome-db-pg`) have no replica to fail over to, so their
   `minAvailable: 1` PDB doesn't just stall until timeout like a normal PDB — it blocks
   forever, because there's structurally nothing CNPG can do without another instance.
3. **Bare pods with no controller** — none currently exist on this cluster, but the
   playbook checks for them every run and refuses to touch a node that has one, rather
   than silently deleting a pod nothing will recreate.

### The fix (current): bypass PDBs directly with `--disable-eviction`

The playbook drains with `kubectl drain --ignore-daemonsets --delete-emptydir-data
--disable-eviction`. `--disable-eviction` skips the Eviction API and deletes pods
directly instead — so it never negotiates with a PDB in the first place, Longhorn's
instance-manager included. Before draining, the only thing it still does is:

```text
cordon (kubectl)
  ↓
enable CNPG nodeMaintenanceWindow on any single-instance cluster pinned here
  (cluster.postgresql.cnpg.io spec.nodeMaintenanceWindow: {inProgress: true, reusePVC: true})
  ↓
short grace pause, then drain --disable-eviction
```

That CNPG step isn't about bypassing a PDB (`--disable-eviction` already does that) —
it tells CNPG's own reconciliation to reuse the existing PVC when it notices the
primary pod is gone, instead of treating it as a failure needing a fresh bootstrap.

**This is safe here, specifically, only because of a check earlier in the same
run:** "Refuse to proceed if storage is already degraded" already guarantees every
volume has full healthy replica redundancy elsewhere before the node is ever touched.
Losing one replica for a short reboot is then a non-event — the same on-disk replica
data reattaches (incremental resync) once the node comes back, rather than triggering
anything.

### What isn't auto-resolved, deliberately

A bare pod with no controller now gets an explicit ansible-level hard-fail before
touching the node — `--disable-eviction` means drain itself no longer refuses one on
its own the way the Eviction API used to. `force_drain=true` overrides this and adds
`--force` to the drain, which deletes it for good since nothing will recreate it. That
has to be a deliberate, informed choice each time, not a default.

### Superseded approach, and why it got replaced

The first working version of this drain instead set two Longhorn Node CR flags before
draining — `allowScheduling: false` then `evictionRequested: true` — to get past
Longhorn's `instance-manager` PDB (`node-drain-policy=block-if-contains-last-replica`),
reverting both in `always:` regardless of success/failure. Two real bugs surfaced
running that version against the live cluster:

- **Case-sensitivity false-positive.** The pre-flight check compared
  `.status.robustness != "Healthy"` — but Longhorn reports it lowercase (`healthy`,
  `degraded`, `unknown`). Every volume matched as "unhealthy" and the first real run
  aborted immediately. Fixed to check for `degraded`/`faulted` explicitly
  (case-normalized), treating `unknown` as fine (normal for a Sablier-scaled-to-zero
  app's detached volume).
- **Wrong order for the two flags.** `evictionRequested=true` was rejected outright —
  `"need to disable scheduling on node ... for node eviction"` — unless
  `allowScheduling` was already `false` on that same Longhorn Node CR first. A plain
  `kubectl cordon` does **not** set that; it's a separate Longhorn-tracked flag.

Both bugs are fixed and that version worked — but `evictionRequested=true` doesn't mean
"pause," it means "migrate away permanently": Longhorn schedules a brand-new replica
elsewhere and fully rebuilds it, then retires the old one, and may schedule another
one back once `allowScheduling` flips true again. That's real, unnecessary data
movement for what's just a reboot — confirmed live (20 volumes went degraded from a
single reboot). `--disable-eviction` replaced it outright rather than layering on top,
since our own pre-flight already provides the safety guarantee eviction would have.

## `/var/run/reboot-required` doesn't exist on this OS image

That file is only ever created by `update-notifier-common`'s kernel `postinst` hook —
and that package isn't installed on this minimal preseeded Debian (confirmed: empty
`/etc/kernel/postinst.d/` apart from `initramfs-tools`/`update-grub`). The original
reboot check (`ansible.builtin.stat` on that path) therefore **always** reported false,
even right after installing a brand-new kernel — every node ran the whole night on
kernels a month-plus stale while the playbook kept reporting "no reboot needed."

Fixed by comparing the running kernel against the newest actually-*installed* kernel
package instead — reliable with just `dpkg`+`uname`, no notifier package required:

```bash
running="$(uname -r)"
latest="$(dpkg -l 'linux-image-*' | awk '$1=="ii" && $2 ~ /^linux-image-[0-9]/ {print $2}' \
  | sed 's/^linux-image-//' | sort -V | tail -1)"
[ "$running" != "$latest" ]  # true → reboot needed
```

**A related gap this surfaced:** a node needing *only* a reboot (OS packages and k3s
both already current from an earlier run) has `will_patch_os` and `will_upgrade_k3s`
both false — the old "nothing to do" gate would skip it **forever**, since apt has
nothing left to offer. Fixed by computing the reboot-pending check separately, up
front in the reporting phase, and folding it into that same gate
(`not (will_patch_os or will_upgrade_k3s or will_reboot)`).

## A pipefail + grep exit-code trap in the pod-settle check

`cluster_wait_stable`'s "wait for pods to settle" check used to end with
`grep -vE '^(Running|Succeeded)$' | wc -l`. `grep -v` exits `1` when it finds **zero**
matching lines — which is exactly the success case here (zero unsettled pods). Under
`set -o pipefail`, that makes the whole pipeline report failure at the *exact moment*
it succeeds: confirmed live, `stdout` was the correct value `"0"` while `rc` was `1`,
and the task hard-failed and rolled back a drain that had already fully succeeded.
Rewritten with `jq` instead (same pattern as the Longhorn check next to it), which has
no such exit-code quirk for a plain `select()`.

## Waiting for Longhorn to fully heal between nodes

`cluster_wait_stable` is a hard barrier at the end of every node's pass, not a courtesy
check — the next node in the `serial: 1` loop cannot start until it clears. It waits on
two independent things, with separate budgets:

1. Pods scheduled on that node have settled (`stabilize_retries`/`stabilize_delay`,
   default up to 3 min) — fast, since it's just waiting for the scheduler to place
   already-evicted pods elsewhere.
2. **No Longhorn volume cluster-wide is `degraded`/`faulted`** (`longhorn_heal_retries`/
   `longhorn_heal_delay`, default up to 30 min) — much slower, and given its own
   budget deliberately. A single node reboot on this 3-node cluster degrades **every**
   3-replica volume that had a copy on that node (confirmed live: 20 volumes went
   degraded after one worker's reboot). Letting the next node start before those finish
   rebuilding would mean two nodes' worth of rebuild I/O competing on the remaining
   nodes at once — worse for recovery time, not better. Bump the retries/delay
   (`-e longhorn_heal_retries=... -e longhorn_heal_delay=...`) if a real run needs
   longer than 30 minutes rather than letting nodes overlap.

On a timeout, the task's failure output includes the actual list of still-non-Healthy
volumes (namespace/name/robustness) rather than a bare "task failed" — that list is
generated fresh on the same failing call, since the shell command emits the volume list
itself, not just a count.

## Rollback behavior

Every node's cordon → drain → update → uncordon sequence is one `block:`. On any
failure:

- `rescue:` gathers a `kubectl get pods -o wide` snapshot of what's still on the node
  and uncordons it again immediately, so a stuck drain doesn't leave the node stranded
  out of the scheduler while someone investigates.
- `always:` unconditionally reverts the Longhorn `allowScheduling`/`evictionRequested`
  flags and any CNPG `nodeMaintenanceWindow` it flipped on — whether the block
  succeeded or the rescue ran.

`any_errors_fatal: true` at the play level means a failure on one node halts the whole
run rather than proceeding to hammer the next node while the cluster is in a known-bad
state.

## Safety model

- `dry_run` defaults to `true`. Every report/detection task (package list, k3s version
  diff, drain-blocker scan) always runs; every mutating task is gated on
  `not (dry_run | bool)`. A plain run is a report.
- A node with nothing pending (OS already current **and** k3s already on
  `k3s_target_version`) is skipped entirely — never cordoned for a no-op.
- `patch_os` / `upgrade_k3s` toggle each concern independently, but the node is still
  only drained once regardless of which are enabled.

## After a real run

Bump `k3s_pre_version` in `ansible/playbooks/k3s_setup.yml` to match
`k3s_target_version` in `ansible/roles/node_update/defaults/main.yml`, so a future
`node_setup`/`k3s_setup` run on a replacement node installs the same version the
cluster is actually running. The playbook's final play prints a reminder for this on
any non-dry-run invocation.

## `drain_timeout` needs to be generous too, not just the post-reboot heal wait

Confirmed live on `master-green`: the eviction-request handling above is real and does
work, but Longhorn can take longer than a short window to actually finish evacuating an
instance-manager — one node's drain hit the (then-)300s `--timeout` and rolled back
cleanly via rescue, and a follow-up check maybe 30 seconds later showed that exact
instance-manager pod **fully gone** (consolidated away by Longhorn, its PDB no longer
even existing). `kubectl drain` already retries evictions every 5s on its own,
respecting PDBs, for the whole `--timeout` window — it just needed more of it. Bumped
the default `drain_timeout` from 300s to 1200s (20 min) for the same reason as
`longhorn_heal_*`: on this hardware, "give Longhorn more time" beats any cleverer
polling logic. A rolled-back drain is always safe here (see Rollback behavior above) —
worth remembering before assuming a timeout means something is actually broken.

One red herring while debugging this: `zigbee2mqtt` (StatefulSet, hard `nodeAffinity`
on `feature.node.kubernetes.io/iot-zigbee-coordinator=true` — only satisfied by
whichever node has the USB Zigbee coordinator plugged in, see
[Zigbee](../wiki/02-smarthome/zigbee.md)) was suspected as the blocker since it's
hardware-pinned to a single node with nowhere else to schedule during that node's
drain. It wasn't — it evicted cleanly and rescheduled back once the node was
uncordoned again, with no repeated errors about it in the drain output. Worth
double-checking directly (`kubectl get pods -o wide | grep <workload>`) rather than
assuming, since a hardware-pinned singleton **is** a real category of thing that could
block a drain outright if it ever picks up its own PDB.

## Operational note: a control-machine glitch can break the automatic rollback too

During live testing, one run hit `"The module ansible.builtin.command was not found in
configured module paths"` partway through the post-update wait — a one-off glitch in
the local ansible-playbook process itself (not a cluster problem, not a logic bug in
this role). The catch: that error hit the `rescue:`/`always:` cleanup too, since they
also use `ansible.builtin.command` — a task-level `failed_when: false` only protects
against the module *returning* non-zero, not against the automation engine failing to
*load* the module at all. When that happens, nothing further in that run can be trusted
to have executed, cleanup included.

**If a run fails with a module-resolution error (rather than a normal task failure with
a clear message):** don't trust the log — check the cluster directly before re-running:

```bash
kubectl get node <name>                                                    # cordoned?
kubectl get nodes.longhorn.io <name> -n storage -o jsonpath='{.spec}'       # allowScheduling / evictionRequested
kubectl get cluster.postgresql.cnpg.io <cluster> -n <ns> -o jsonpath='{.spec.nodeMaintenanceWindow}'
```

Manually uncordon / patch `allowScheduling: true` / `evictionRequested: false` /
`nodeMaintenanceWindow.inProgress: false` if any are left in the mutated state, then
re-run with `--limit <name>` once you've confirmed the node's own work (OS/kernel/k3s)
actually completed — the module-loader glitch happened after the real update had
already succeeded in the one case observed so far.

## Possible future work

- The Longhorn eviction request doesn't actually wait for replicas to relocate before
  draining — on this 3-node cluster, several volumes use `numberOfReplicas: 3`, which
  makes full evacuation off a single node structurally impossible (nowhere to put the
  third copy). The short grace pause just lets Longhorn's PDB controller recompute
  `disruptionsAllowed`, not lets it finish rebuilding elsewhere. Accepted trade-off:
  those volumes run briefly degraded (2/3 replicas) during that node's maintenance
  window and self-heal once it's back — normal, not dangerous, as long as nodes are
  never updated in parallel (`serial: 1` guarantees that here).
- `cnpg.io/nodeMaintenanceWindow` triggers a CNPG admission warning suggesting
  `.spec.enablePDB` instead — worth revisiting if CNPG deprecates the maintenance
  window field outright in a future version.
