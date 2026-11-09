# TODO — Possible future ideas

[← Back to Home](../Home.md) · Related: [Rejected ideas](../07-rejected/README.md)

> **Status (2026-11-09):** brainstorm backlog. Nothing here is scheduled or built. Each idea was checked against the
> repo and Home Assistant when it was listed, so none duplicates existing work. Promote an idea to its own page
> when work starts.

## Worth exploring

- [ ] **Backup restore drill**: a scheduled job that restores a CNPG database and a Longhorn volume into a scratch
      namespace and checks the data. No restore has ever been tested (only the disabled `dr-test` chart exists), and
      CNPG has no WAL or logical backups, only Longhorn volume backups to the NAS.
- [ ] **UniFi Poller → Grafana**: network, client and WAN metrics next to the cluster dashboards.
- [ ] **Technitium query analytics**: top clients, blocked domains and query trends from the DNS cluster.
- [ ] **IoT honeypot canary**: a fake IoT host on the IoT VLAN that alerts on any contact.
- [ ] **BabyBuddy wake-window insights**: derive wake windows from the logged sleep data.
- [ ] **Weekly house digest**: one summary message a week (energy, alerts, device health).
- [ ] **BLE proxies + Bermuda**: room-level presence from Bluetooth.
- [ ] **Paperless-ngx**: document scanning and archive.
- [ ] **ntfy**: self-hosted push notifications for alerts and automations.
- [ ] **Homebox**: inventory of household items.
- [ ] **HA event flight recorder**: keep a short rolling log of events around an incident for later replay.
- [ ] **Zigbee2MQTT down alert from MQTT state**: alert when the bridge reports offline on `zigbee2mqtt/bridge/state`
      while its pod is still Ready (the pod alerts and the Mosquitto client-count alert do not catch that). Nothing in
      Home Assistant uses the bridge connection entity today. Options: an HA automation on the bridge connection
      binary sensor (offline for 2+ min), or a Prometheus/Grafana rule if the state is exported. Clean up the ~174
      `unavailable` entities before adding any "unavailable count" alert.

## Waiting on hardware

- [ ] **Local LLM stack** (Open WebUI plus a model, HA conversation agent, wiki Q&A, alert triage): waits for a Mac
      Studio M1 Max. The current nodes only fit 1-4B models and the iGPU is already used by Whisper. Benchmark it like
      the [voice benchmark](../02-smarthome/voice-benchmark.md) when the hardware arrives.
- [ ] **Smarter heating optimiser**: after the TRVs are installed and maintenance is done.

## Already ruled out

Do not re-propose: spoken announcements, cluster cost per app, Spoolman, Mealie, garden sensing, Immich (Synology
Photos is enough), nursery sensors, energy dashboard / peak alerts / waste / mailbox / window notifications (all done),
dynamic pricing and presence-based modes (done with n8n workflows and price checks), blackbox probes (parked on
purpose).
