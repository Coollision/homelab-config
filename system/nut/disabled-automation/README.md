# NUT shutdown automation — design (NOT built yet)

Goal: during a power outage, **shed load progressively** so the core homelab stays up as long as the
UPS lasts, and everything shuts down cleanly before the battery dies.

```
  UPS on battery (OB)                 battery low (LB)
        │                                   │
        ▼                                   ▼
  Stage 1: non-essential            Stage 2: full graceful shutdown
  - cordon + drain the VM worker     - cordon + drain remaining workers
  - shut down the NAS                - shut down the control-plane node last
    (biggest single UPS draw)        - tell upsd to cut load (FSD)
```

## Trigger options (pick one when building)

1. **upsmon `NOTIFYCMD`** (in the `nut` pod) — NUT's native hook. upsmon already runs in the pod and
   fires `ONBATT` / `LOWBATT` events. A `NOTIFYCMD` script could call out to a controller. Downside:
   the pod would need a ServiceAccount with RBAC to cordon/drain nodes, plus a way to power off the
   NAS and signal shutdown to non-cluster nodes. Mount the script under `/usr/local/bin` (NOT
   `/etc/nut`) and set `NOTIFYCMD` via the `config:` env in [`../nut/values.yaml`](../nut/values.yaml).

2. **Prometheus Alertmanager → webhook** — alert on `network_ups_tools_ups_status{flag="OB"} == 1`
   (stage 1) and `{flag="LB"} == 1` (stage 2), route to a small receiver that runs the drain. Nice
   because the alerting/metrics already exist (see [`../nut/templates/servicemonitor.yaml`](../nut/templates/servicemonitor.yaml)).

3. **Home Assistant** (likely the cleanest here) — HA already orchestrates the house and will have the
   NUT integration for controls. An HA automation on the UPS status sensor can call `shell_command` /
   a webhook into the cluster and use the Synology integration to power the NAS down. Keeps the
   sequencing logic in one place with good observability.

## What the automation must do

| Stage | Action | How |
|-------|--------|-----|
| 1 (OB) | cordon + drain the **VM worker** | `kubectl cordon/drain <node>` (RBAC-scoped SA) |
| 1 (OB) | shut down the **NAS** | Synology API / HA Synology integration / SSH `poweroff` |
| 2 (LB) | drain remaining workers + control-plane | `kubectl drain` in node order |
| 2 (LB) | cut UPS load | `upscmd ups@<lb-ip> load.off` (admin already has `instcmds ALL`) |

Add hysteresis / a minimum on-battery duration so a 5-second flicker doesn't drain the cluster.

## Where the code will live

- **K8s manifests** (RBAC ServiceAccount, the drain Job/controller, Alertmanager rule + receiver) →
  here: rename this dir from `disabled-automation` to `automation` to activate it, and add a
  `Chart.yaml` + `templates/` like the sibling apps.
- **HA automation** (if option 3) → the Home Assistant config, built with the
  `home-assistant-best-practices` skill.

## Already in place (forward-compatible)

- `admin` user has `instcmds ALL` + `actions SET` → can issue `load.off` / `fsd`.
- `upsmon` runs in the `nut` pod → `NOTIFYCMD` is a ready hook.
- `network_ups_tools_ups_status{flag="OB"|"LB"}` metrics exist for alert-based triggering.
