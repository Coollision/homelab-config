# History & Evolution

[← Back to Home](../Home.md) · See also: [Known Issues](known-issues.md)

This narrative is mined from this repo's ~1900+ commit history. It's here to explain
*why* things are shaped the way they are today — the current-state pages elsewhere in
this wiki describe the "what"; this page is the "how we got here."

## The GitOps structure was there from day one

Unlike a lot of homelab repos that start flat and get restructured into an app-of-apps
pattern later, **this repo's ApplicationSet pattern and ArgoCD self-management were both
present in the very first commit.** The ["directory path = deployment
target"](../00-setup/gitops-structure.md) convention wasn't a later cleanup — it's the
original design.

## Platform buildout (early)

The basics — ingress, cert-manager, node-feature-discovery, image caching, monitoring —
landed within the repo's first couple of months. Traefik and the monitoring stack
(kube-prometheus-stack, later Loki+Promtail) have been under **continuous, incremental
maintenance ever since**, with no single rebuild event — version bumps and periodic
tuning commits, not migrations.

## Media stack: steady growth, then a deliberate restructure

The media-automation apps grew one image-version-bump at a time for a long stretch, in a
flat namespace layout with a self-managed database. In mid-2026, this was deliberately
restructured: moved into its own dedicated namespace, switched from NFS-backed to
Longhorn-backed config storage, and had its shared database migrated onto CloudNativePG
— not a cosmetic rename, a genuine isolation + storage-engine upgrade. See
[Known Issues → moving a workload between namespaces](known-issues.md#moving-a-workload-between-namespaces)
for what that migration actually involved operationally.

## Smarthome / Matter-Thread: fast buildout, then a fragility-hardening phase

The Thread/Matter stack was stood up quickly, then went through a concentrated burst of
fragility fixes once real devices were on the mesh: the SRP lease clamp, the mDNS
reflector rewrite, per-stub MAC-address uniqueness fixes, and Multus VLAN-trunking
refinements all landed in a tight sequence once the mesh was under real load. This
matches — and is the origin of — everything on the
[Matter & Thread](../02-smarthome/matter-thread.md) page.

## Database consolidation onto CloudNativePG

A clean, linear sequence: the CloudNativePG operator was added, the shared media-stack
database was cut over to it (triggered by a memory-pressure incident unrelated to the
database itself — see below), n8n was onboarded with its own dedicated Postgres cluster,
and finally Home Assistant's recorder was migrated off its bundled MariaDB onto the same
CNPG cluster n8n uses. The Home Assistant migration was explicitly framed as
**consolidation, not a performance fix** — one less database engine to operate, not a
reaction to MariaDB being slow or broken. See [Platform → Databases](../01-platform/databases.md).

## Scale-to-zero (Sablier) rollout

Landed as a single feature immediately after the database consolidation work, then
followed by a short, tight tail of hardening fixes — persisting OAuth state across
restarts for the MCP bridge, fixing a Longhorn volume health check that misread a
scaled-to-zero volume as stuck, and fixing a hard-mount deadlock in the SMB operator that
Sablier's own detach behavior had exposed. All of Sablier's trickiest lessons (see
[Known Issues](known-issues.md#sablier-scale-to-zero-gotchas)) trace back to this same
short window.

## Vault hardening, in a single day

Vault's auto-unseal mechanism went through three complete redesigns in one day: a
CronJob submitting keys → an in-pod sidecar → the current on-demand, no-resident-secret
version → a same-day fix for a JSON-parsing gotcha. What looks like a carefully evolved
design in [Secrets — Vault](../01-platform/secrets-vault.md) was actually iterated to its
current shape very quickly under real pressure, not designed up front.

## Storage: Longhorn arrived designed for recovery, not accidentally

Longhorn wasn't part of the original platform — it was introduced later as a formal
abstraction (a shared library chart), and the commit that introduced it added the SMB
operator, a disaster-recovery test chart, *and* a full teardown script all in the same
change. Storage-stability tuning (replica placement, rebuild concurrency limits,
avoiding replica stacking) continued incrementally for months afterward.

## Networking / DNS: matured alongside the smarthome buildout

Multus VLAN trunking, dual-stack/IPv6-aware DNS, and the clustered Technitium DNS design
all matured over the same stretch as the Matter/Thread buildout — not a coincidence, since
Thread specifically needs solid IPv6 routing. A later fix made the DNS/networking layer
survive an ISP delegated-prefix rotation without manual intervention, and a follow-up
fixed Multus stub MAC-address collisions that had been confusing the network controller's
device fingerprinting.

## What did *not* happen: no disaster-recovery event

Despite the amount of DR-adjacent tooling in this repo (a DR test chart, a Longhorn
teardown script, backup-retention policies everywhere), there is **no evidence in the
commit history of an actual cluster rebuild or disaster-recovery event having happened.**
All of that tooling was built proactively as resilience investment, not in response to a
real outage. If a genuine rebuild ever does happen, it will be the first time this
tooling gets used for real rather than tested.

## The Renovate batching workflow

Automated dependency-update commits (`chore(deps): batch renovate updates`) are, by
volume, the single largest contributor to this repo's commit count. The current
practice — grouping many individual Renovate branches into one batch, verified together
and merged as one commit — was formalized partway through this repo's life; earlier on,
each dependency bump was merged one at a time as its own commit.
