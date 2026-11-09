# TODO — descheduler churn follow-up

[← Back to Home](../Home.md) · Related: [Cluster housekeeping](../01-platform/cluster-housekeeping.md)

> **Status (2026-11-09):** fix shipped (cpu/memory-only balancing, cpu target 90, one balancing profile,
> `minPodAge: 30m`, PVC pods stay evictable). First 26 min showed 0 evictions and 0 overutilized nodes;
> a full day has not been observed yet. Delete this page once verified.

Before the fix: ~1,560 evictions in 7 days (~220/day), 98% from the 40-pod-cap Raspberry Pi node, which was
"overutilized" on pod count only while at ~40% cpu/memory.

- [ ] After a full day (incl. the ~03:00 spike), count evictions in Loki:
  `{app="descheduler"} |= "Evicted pod"` — expect single digits/day, none driven by memory or pod count.
- [ ] Check no node sits above the targets in the `Node has been classified` lines
  (`{app="descheduler"} |= "overutilized"`); the high-memory mini node (~75%) has ~10 points of headroom to the 85% target.
- [ ] Spot-check the pods that were evicted most (music-intake, lidarr, bazarr, mosquitto, tautulli) are no longer restarting.
- [ ] Accepted risks, not todos: single-instance StatefulSets (mosquitto, vault) may restart on a rare eviction;
  `maxNoOfPodsToEvictPerNamespace` stays 6.
