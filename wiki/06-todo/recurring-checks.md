# TODO — Recurring checks

[← Back to Home](../Home.md) · Related: [Known Issues → VM node disk I/O hang](../05-operations/known-issues.md#vm-node-disk-io-hang)

> **Status (2026-11-23):** things that need a look from time to time, with the next due date. Nothing here is broken. After
> each check, update the baseline table and move the due date.

## Watch the first weekly trim on the VM node {#first-weekly-trim}

- [ ] **Due: Monday 2026-11-30**, shortly after midnight (the guest's weekly `fstrim.timer`, with a random delay of up to ~100
  minutes), then glance at the next two Mondays.

**Why:** Unmap was enabled on 2026-11-23, so the timer now really discards. A large trim made the VM drive's write latency
spike to ~1.5 s before ([Known Issues → VM node disk I/O hang](../05-operations/known-issues.md#vm-node-disk-io-hang)).

**How:** on the NAS, check `dmesg | grep vhost_scsi_get_tag` is still at the original 10 lines and that the node stayed
`Ready` overnight; on the guest, `journalctl -u fstrim.service` shows how much it trimmed. If the node dropped out or the
count grew, replace the timer with a chunked, throttled trim (6 GiB at a time, wait for the drive to settle between chunks).

## NAS NVMe wear and latency, every 4 months {#nas-nvme-check}

- [ ] **Next due: 2027-03-23**, then every 4 months.

**Why:** the NAS has two NVMe drives of the same age (about 4.1 years powered on at the first check). One holds the volume with
the k3s VM's disks, the other holds the NAS's own apps and the old NFS data. After the Longhorn move the write load shifted
between them, and the VM drive stalled once ([Known Issues → VM node disk I/O hang](../05-operations/known-issues.md#vm-node-disk-io-hang)).
The wear counters are the only early warning for the older drive.

**How:**

1. On the NAS (as root), read both drives: `synonvme --smart-info /dev/nvme0n1` and `/dev/nvme1n1`. Note Percentage Used,
   Data Units Written, Media errors, Number of error log entries, Critical Warning and Unsafe Shutdowns.
2. Work out the write rate since the last check: `(units now - units then) x 512000 bytes / days`. (One data unit is 512,000
   bytes.)
3. Sample `/proc/diskstats` twice, 30 seconds apart, on a quiet system and compute the average write latency per drive
   (milliseconds spent writing / writes completed). Compare it with the baseline.
4. Add a row to the baseline table below.

Baseline, first check:

| Date | Drive | Remaining life | Data units written | Power-on hours | Media errors |
|---|---|---|---|---|---|
| 2026-11-23 | VM drive | 77% | 172,265,841 (~88 TB) | 36,118 | 0 |
| 2026-11-23 | Other drive | 36% | 476,083,935 (~244 TB) | 36,118 | 0 |

Write rate at the first check: VM drive ~86 GB/day since the last NAS boot (lifetime average ~59); other drive ~40 GB/day
since the last boot (lifetime average ~162). Latency at the first check: ~41 ms and ~72 ms average write latency in a 30 s
sample (lifetime ~3.8 ms and ~5.2 ms).

**Act when:**

- Any media error, error-log entry or non-zero Critical Warning on either drive: investigate now, do not wait for the next
  check.
- The other drive reaches 20% remaining life or less: order a replacement.
- The other drive's write rate is back above ~150 GB/day: find out what is writing to it.
- The `vhost_scsi_get_tag failed` message appears in the NAS kernel log, or the VM drive's idle write latency stays high: see
  the known issue, and capture the evidence listed there before restarting anything.
