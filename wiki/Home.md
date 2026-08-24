# HomeLabConfig Wiki

This is the internal wiki for this repository: a k3s cluster fully managed by ArgoCD,
running on a small fleet of nodes at home. Everything the cluster runs — from DNS to
Home Assistant to a media stack — is declared here in Git and reconciled automatically.
This wiki explains **how it's built, why it's built that way, and how to operate it.**

> **Docs vs. this wiki:** the pre-existing `docs/` folder holds narrow runbooks for
> specific one-off procedures (DNS bootstrap, a Flannel IPAM bug, the HA→CNPG migration).
> This `wiki/` folder is the map of the whole system — start here, and follow links out
> to `docs/` when a page points you at a detailed runbook.

## Start here

- **New to this repo?** Read [Setup → Bootstrap](00-setup/bootstrap.md) first — it walks
  through how a blank node becomes a working cluster node, and how the cluster then
  bootstraps ArgoCD, which takes over everything else.
- **Want the "why", not just the "what"?** Read [History & Evolution](05-operations/history.md) —
  a narrative of how this repo grew from a flat 2024 layout into its current shape, mined
  from ~1900+ commits.
- **Something's broken?** Go straight to [Known Issues & Troubleshooting](05-operations/known-issues.md) —
  every hard-won lesson from real incidents, organized by symptom.
- **Need a command, not an explanation?** [Ansible cheat sheet](05-operations/ansible-cheatsheet.md) ·
  [kubectl / cluster cheat sheet](05-operations/kubectl-cheatsheet.md)

## Map of the wiki

### [00 · Setup](00-setup/)
How a bare machine becomes a cluster node, and how the cluster bootstraps itself.
| Page | Covers |
|---|---|
| [Bootstrap](00-setup/bootstrap.md) | Preseed → Ansible → k3s → Helm bootstrap of storage/Vault/ArgoCD |
| [Ansible](00-setup/ansible.md) | Every role, what it does, inventory groups |
| [GitOps structure](00-setup/gitops-structure.md) | `argocd/`, `applications/`, `lib/` — how the "app of apps" tree works |

### [01 · Platform](01-platform/)
The infrastructure layer every workload depends on — all under `system/`.
| Page | Covers |
|---|---|
| [Networking](01-platform/networking.md) | MetalLB, Multus/VLANs, Traefik, Cloudflare Tunnel, Technitium DNS |
| [Storage](01-platform/storage.md) | Longhorn, the SMB operator, NFS, PV reclaimer |
| [Secrets — Vault](01-platform/secrets-vault.md) | Vault, auto-unseal, the `<secret:...>` templating scheme |
| [Databases](01-platform/databases.md) | CloudNativePG, and the shared-Postgres pattern used everywhere |
| [Observability](01-platform/observability.md) | kube-prometheus-stack, Loki/Alloy, SNMP, dashboards |
| [Cluster housekeeping](01-platform/cluster-housekeeping.md) | Keel, kube-fledged, Reloader, Sablier scale-to-zero, NFD/descheduler |
| [Certificates](01-platform/certificates.md) | cert-manager + the Cloudflare DNS-01 wildcard cert |
| [Power — NUT](01-platform/power-nut.md) | UPS monitoring, PeaNUT dashboard, planned auto-shutdown |

### [02 · Smart Home](02-smarthome/)
Home Assistant and everything wired into it — all under `workload/smarthome/`.
| Page | Covers |
|---|---|
| [Home Assistant](02-smarthome/home-assistant.md) | The core app, VLANs, DB, ingress |
| [Matter & Thread](02-smarthome/matter-thread.md) | matter-server, thread-border-router, mDNS reflector, the fragile mesh |
| [Zigbee](02-smarthome/zigbee.md) | Zigbee2MQTT, Mosquitto MQTT broker |
| [ESPHome](02-smarthome/esphome.md) | Firmware dashboard, VLAN40 mDNS discovery |
| [Voice](02-smarthome/voice.md) | Whisper (STT) + Piper (TTS) |
| [Automation & MCP](02-smarthome/automation-mcp.md) | n8n, ha-mcp, kubernetes-mcp-server |
| [Smarthome database](02-smarthome/database.md) | The shared CNPG cluster backing HA + n8n |

### [03 · Media Stack](03-media-stack/)
The Radarr/Sonarr/Prowlarr automation pipeline — `workload/arr-stack/`.
| Page | Covers |
|---|---|
| [Overview & topology](03-media-stack/overview.md) | Request flow, every app, the shared DB and NFS libraries |

### [04 · Apps](04-apps/)
Everything else — `workload/apps/`, `workload/databases/`, `workload/secrets/`, `workload/proxies/`.
| Page | Covers |
|---|---|
| [Misc apps](04-apps/misc-apps.md) | BabyBuddy, CloudBeaver, Vaultwarden, WordPress |
| [AWS proxies](04-apps/aws-proxies.md) | aws-tunnels, aws-mcp — SSM-based tunnels into AWS |

### [05 · Operations](05-operations/)
| Page | Covers |
|---|---|
| [History & Evolution](05-operations/history.md) | How this repo got here — narrative from commit history |
| [Known Issues & Troubleshooting](05-operations/known-issues.md) | Every real incident and its root cause, by symptom |
| [Ansible cheat sheet](05-operations/ansible-cheatsheet.md) | Ad-hoc commands: uptime, reboot, facts, targeting a subset of nodes |
| [kubectl / cluster cheat sheet](05-operations/kubectl-cheatsheet.md) | Draining nodes, restarting crashy pods, ArgoCD cache-busting, Sablier status |

## The one-paragraph mental model

Three physical/VM nodes run k3s (one control-plane node, two workers — see the
[Ansible page](00-setup/ansible.md) for the DNS-name-vs-k3s-name split). Ansible turns a
bare Debian install into a joined k3s node and nothing more; **everything above the OS is
Git.** ArgoCD watches this repo and, via three `ApplicationSet`s
([GitOps structure](00-setup/gitops-structure.md)), turns every `system/<ns>/<app>` and
`workload/<ns>/<app>` directory into a running Kubernetes app — the **namespace is
literally the folder name.** Almost no app hand-writes Kubernetes YAML: three tiny Helm
*library* charts in `lib/` (`shared-lib`, `arr-lib`, `longhorn-storage-lib`) turn a
`values.yaml` file into a Deployment/StatefulSet + Service + Ingress + storage + secrets,
so each app's chart is just data. Secrets never live in Git — every `values.yaml` points
at a Vault path (`<secret:kv/data/...>`), resolved at render time by an ArgoCD plugin
sidecar. On top of that platform, three workload families run: a **smart-home stack**
(Home Assistant + Matter/Thread + Zigbee + voice), a **media-automation stack**
(Radarr/Sonarr/Prowlarr), and a grab-bag of **standalone apps**.
