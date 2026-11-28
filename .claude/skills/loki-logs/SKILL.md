---
name: loki-logs
description: Fetch cluster logs from Loki, including pods that no longer exist (finished/failed Jobs, pods before a restart, anything older than `kubectl logs` can reach). Use for "why did job X fail", "what did pod Y log at 03:00", "logs from last night", or when kubectl says the pod is gone. Prefer the log-fetcher agent for the actual pulling.
argument-hint: <what to find, namespace/pod, time window>
allowed-tools: Bash(scripts/loki-logs.sh:*)
---

Loki holds ~90 d of pod logs (see `wiki/01-platform/observability.md`). It is only reachable in-cluster and needs
a tenant header, so **always go through `scripts/loki-logs.sh`**; never hand-roll port-forward/curl/LogQL.

## Who does it

Pulling logs is mechanical → delegate to the **`log-fetcher`** agent (Haiku) with a brief naming namespace/pod,
the time window and what to look for. Main model only reads the result and concludes. Run the script directly
only for a single trivial probe.

## Playbook (cheapest first, widen only if needed)

1. **Window first.** Failures are usually minutes wide. Take the time from the alert/Job (`startsAt`, Job start/end),
   pass `--from/--to` in UTC, never "last 7d".
2. `pods`: confirm the pod name exists in Loki (`-n <ns> -p <prefix>`).
3. `count`: how many lines per pod. Hundreds of lines = loop; a handful = something rare.
4. `query -e` (errors/warn/fail/timeout) with `--limit 40`. Identical lines collapse to `(xN, last HH:MM:SS)`.
5. Only then `-g '<specific text>'` around the interesting time, or `-v '<noise>'` to drop a chatty pattern.
6. Still unclear → `raw '<LogQL>'`, or `--full` to keep the key=value tail.

```bash
scripts/loki-logs.sh pods  -n storage -p snapshot-2h-2992 --from 2026-11-27T00:55 --to 2026-11-27T03:00
scripts/loki-logs.sh count -n storage -p snapshot-2h-2992 --since 8h
scripts/loki-logs.sh query -n storage -p snapshot-2h-29929020 -e --from 2026-11-27T00:55 --to 2026-11-27T03:00
scripts/loki-logs.sh query -n storage -a longhorn-manager -g 'babybuddy|dc9d4677' --from 2026-11-26T03:00 --to 2026-11-26T03:10
```

## Conventions

- **Times are UTC** everywhere (node logs, Loki, the script). Say "UTC" when reporting; the nodes' local time is
  UTC+1 in winter, so a `0 4 * * *` CronJob (controller-manager local time) fires at 03:00 UTC.
- **Pod labels are prefix-matched** (`-p snapshot-2h-2992` matches every pod of that Job family). Job names
  encode minutes since epoch, so close names = close in time.
- **Keep it small:** default `--limit 40`, hard cap 300. If you need more, narrow the window or add `-g`, don't
  raise the limit. Never dump a whole stream into the context.
- **Read-only.** The script only queries. It never writes to Loki or the cluster.
- **Tenant:** `LOKI_ORG` defaults to `1` (Loki answers an empty `{"status":"success"}` for any other tenant, so an
  empty result may mean the wrong tenant, not "no logs"). Sanity check with `pods` on a pod you know exists.
- **Public repo:** quote log lines in docs/commits only after removing hostnames, IPs, domains, emails.
- Pod-log labels: `namespace`, `pod`, `container`, `app`, `job_name`, `hostname`, `instance`. Syslog sources
  (NAS, UniFi) are labelled differently and the flags above won't select them: inspect one stream first
  (`raw '{hostname="<name>"}' --limit 3 --full`), then use `raw`.
