# aws-tunnels throughput: findings and options

Investigation date: 2026-08-07. All figures measured, not estimated.

## Summary

Each aws-tunnel is capped at **~0.65–0.73 MB/s (~5.5 Mbit/s)**. This is a per-SSM-session
limit imposed on the AWS side. It is **not** CPU, not WAN, not the bastion, and there is no
client-side knob for it.

The one lever that works today, with no AWS changes at all, is **compressing before the bytes
enter the tunnel** (`ssh -C`). Throughput becomes `compression_ratio × 0.7 MB/s`.

## What was ruled out

| Hypothesis | Verdict | Evidence |
| --- | --- | --- |
| Tunnel-pod CPU throttling | **No** | A/B: 200m limit → 80% CFS periods throttled → ~700 KB/s. 2-core limit → 0% throttled → ~693 KB/s. Identical. |
| Home WAN uplink | No | Same pod pulls 7.4–10 MB/s from an internet speed endpoint. |
| Bastion CPU | No | Burstable CPU credits sat flat at maximum, completely unburnt. |
| Bastion/shared bandwidth cap | No | Two independent SSM sessions to the *same* bastion ran simultaneously at ~660 KB/s **each**. |
| Old plugin/agent versions | No | Both ends are well past the versions carrying AWS's throughput fixes. |
| Traefik / socat / TLS | No | Measured inside the tunnel pod at SSM's loopback port — the ceiling exists upstream of all of them. |

The pegged CPU is a *symptom*: the plugin busy-waits on SSM acks. It is the most convincing
wrong answer here — do not re-chase it.

## Why it is per-session, not per-connection

`session-manager-plugin` sets `TCPMultiplexingSupportedAfterThisAgentVersion`, so all
connections through one tunnel are smux-multiplexed onto a **single** datachannel. Measured:

- 4 parallel TCP connections inside one session → ~746 KB/s **aggregate** (no gain)
- 2 separate sessions → ~660 KB/s **each** (clean doubling)

Framing is `StreamDataPayloadSize = 1024` (1 KB messages, ~700/sec at the observed rate).
`OutgoingMessageBufferCapacity` is 10000 (~10 MB), so the client is *not* windowing — the
pacing is on AWS's side. Rate is dead flat: ~11 MB sustained over one keep-alive connection
holds ~695 KB/s with no slow-start ramp.

## The zero-change win: `ssh -C`

Measured end-to-end from a laptop, same object through each path:

| Path | Throughput |
| --- | --- |
| SSM direct (current production path) | ~0.65 MB/s |
| SSH over SSM, no compression | ~0.66 MB/s (no penalty — SSH layering is free) |
| **SSH over SSM with `-C`** | **~1.96 MB/s effective (2.9×)** |

Bulk transfer bracket (`ssh` streaming from the bastion):

| Payload | Throughput |
| --- | --- |
| Incompressible, no `-C` | 0.61 MB/s |
| Incompressible, with `-C` | 0.56 MB/s (slightly worse — don't compress random/encrypted data) |
| Highly compressible, with `-C` | **18.4 MB/s (30×)** |

SSH access needs **no key management**: `ec2-instance-connect send-ssh-public-key` pushes an
ephemeral key valid for 60 seconds, then you SSH over the existing SSM forward. Verified working.

```sh
# 1. forward bastion:22 to localhost:2222 over the existing SSM path
aws ssm start-session --target <bastion-instance-id> \
  --document-name AWS-StartPortForwardingSession \
  --parameters '{"portNumber":["22"],"localPortNumber":["2222"]}'

# 2. push a 60-second ephemeral key
ssh-keygen -t ed25519 -N "" -f /tmp/eic
aws ec2-instance-connect send-ssh-public-key --instance-id <bastion-instance-id> \
  --instance-os-user ec2-user --availability-zone <az> \
  --ssh-public-key file:///tmp/eic.pub

# 3. compressed forward to the real target
ssh -C -i /tmp/eic -p 2222 -N -L 18080:<target-host>:<target-port> ec2-user@127.0.0.1
```

### Verified against a real database

Identical Postgres bulk `COPY` (18.2 MB result set), same cluster, three paths:

| Path | Result |
| --- | --- |
| SSM direct + `sslmode=require` (production today) | 29.2s — **0.62 MB/s** |
| SSH, no compression, + `sslmode=disable` | 27.9s — **0.65 MB/s** (SSH itself is free) |
| **SSH `-C` + `sslmode=disable`** | **7.7s — 2.36 MB/s (3.8×)** |

`rds.force_ssl` is `0` on the dev cluster, so plaintext connections are accepted and the
Postgres wire protocol compresses. **Check this per environment before relying on it** — if
`force_ssl` is enabled the connection stays TLS end-to-end and the gain disappears.

Use `scripts/aws-fast-tunnel.sh` to get this pattern in one command.

## Operator support

Implemented in operator **v0.8.0** and enabled here for all three DB tunnels.

| Knob | Config | Helps |
| --- | --- | --- |
| `replicas` | `tunnelDefaults.replicas`, per-tunnel `replicas`, or the status UI | Multi-connection workloads only |
| `ssh.enabled` | `tunnelDefaults.ssh`, or per-tunnel `ssh` | Single streams — the 3.8× |

Replicas are also adjustable at runtime from the auth UI (`POST /tunnel-scale`). The chosen count
is pinned with a `proxies.homelab.io/manualReplicas` annotation that the reconcile loop reads, so
it is not reverted on the next ~30s tick; "reset to config" clears the pin. Scaling to zero is
deliberately *not* possible there — use Stop, so "off" stays a distinct state from "how many".

### What is enabled

`dev-db`, `preprod-db` and `prod-db` all run TLS termination plus the compressed SSH transport:

```yaml
      - name: <env>-db
        ingressMode: tcp
        tls:
          passthrough: false      # was: true
          # secretName omitted -> uses the cluster's default TLSStore wildcard, which already
          # terminates TLS for the *-work.<domain> HTTP tunnels.
        ssh:
          enabled: true
```

The two settings must move together — see the section below for why.

`gitlab-dev` stays on the plain transport: it already carries HTTP that GitLab gzips for any
client sending `Accept-Encoding`, so compressing the tunnel adds little.

Clients need **no change**: they still connect with `sslmode=require` to the same host on :443,
and SNI routing still works because Traefik presents the wildcard certificate.

`rds.force_ssl` was verified `0` on the dev, preprod **and** prod clusters, so the bastion→RDS leg
can be plaintext and therefore compressible. **Re-check it if a cluster is ever re-parameterised**
— with `force_ssl` enabled the payload stays TLS end-to-end inside the SSH tunnel and the gain
disappears.

### Prerequisites (already satisfied)

- **Runner image**: `openssh-clients` is installed as of v0.8.0. An ssh-enabled tunnel on an older
  image fails at startup, so the chart bump and the feature flip must land together.
- **IAM**: the tunnel's AWS role needs `ec2-instance-connect:SendSSHPublicKey`. Nonprod is
  `AdministratorAccess` and prod is `PowerUserAccess`, both of which allow it.
- **Bastions**: all three are Amazon Linux with SSM agent online, so the EC2 Instance Connect
  agent that accepts the ephemeral key is present.

### The security trade-off, stated plainly

Today TLS is one continuous session from the client to RDS. Afterwards the path is segmented:

```text
client --TLS--> Traefik --in-cluster--> tunnel pod --SSH--> bastion --plaintext--> RDS
```

Every hop is protected, but no longer by a single end-to-end TLS session, and the bastion→RDS leg
is plaintext inside the VPC. That is the price of compression: ciphertext cannot be compressed, so
there is no arrangement that keeps end-to-end TLS *and* gets the 3.8×.

## Why compression and TLS-passthrough are mutually exclusive

The 3.8× is a result about **where TLS terminates**, not about SSH. Putting `ssh -C` inside the
tunnel pod while leaving passthrough on would achieve nothing, for a structural reason:

- A DB tunnel is exposed as an `IngressRouteTCP` with `HostSNI(...)`. Under
  `tls.passthrough: true` — how these tunnels used to be configured — routing is by SNI, so the
  **client must speak TLS**; that is what makes the route match at all.
- Consequently the TLS session is negotiated end-to-end between the client and RDS, and every
  byte crossing the tunnel pod is ciphertext.
- Ciphertext does not compress. Measured: compressing incompressible data is *slower*
  (0.61 → 0.56 MB/s).

That is why the change flips `passthrough` to termination rather than just turning on SSH: the
two are mutually exclusive, and termination is what creates a compressible segment.

The GitLab tunnel is a different case — it already carries plaintext HTTP, so it *would* compress,
but GitLab gzips for any client sending `Accept-Encoding`. Enabling SSH there buys little, so it
stays on the plain transport.

## Option: EC2 Instance Connect Endpoint (EICE)

**Not measured** — creating the endpoint was not authorised during the investigation, so EICE's
actual transport speed is still an open question. Everything below is the setup it would need.

Two things to know before investing:

1. **EICE is VPC-local.** It connects to a private IP *inside its own VPC*. It cannot relay to
   somewhere the bastion merely routes to. Any target outside the VPC (e.g. a GitLab host that
   isn't an instance in that account) needs the `ssh -L` second hop, or its own endpoint.
2. **One endpoint per VPC.** Environments in separate VPCs each need their own.

Ports are *not* restricted to 22/3389 — `--remote-port` accepts arbitrary ports (widely-cited
re:Post answers saying otherwise are outdated), so an endpoint can target a database directly.
`--max-websocket-connections` defaults to 10, and each TCP connection gets its own websocket —
which is the reason it *might* beat SSM's single shared datachannel. Unverified.

### AWS-side changes required

```hcl
resource "aws_security_group" "eice" {
  name   = "eice-tunnel"
  vpc_id = var.vpc_id
  egress {
    description     = "to targets reachable in-VPC"
    from_port       = var.target_port
    to_port         = var.target_port
    protocol        = "tcp"
    security_groups = [var.target_sg_id]   # or cidr_blocks = [vpc_cidr]
  }
}

resource "aws_ec2_instance_connect_endpoint" "this" {
  subnet_id          = var.private_subnet_id
  security_group_ids = [aws_security_group.eice.id]
  preserve_client_ip = false
}
```

Plus:

- **Target security group** must allow inbound on the target port *from the EICE security group*.
  Tunnelling to the bastion on port 22 may need no change if 22 is already permitted.
- **IAM** for whoever opens tunnels:
  - `ec2:DescribeInstanceConnectEndpoints`
  - `ec2-instance-connect:OpenTunnel`
  - `ec2-instance-connect:SendSSHPublicKey` (only for the SSH second-hop pattern)
  - Scope with the `ec2-instance-connect:remotePort` condition key to restrict reachable ports.
- No public IP, no inbound internet exposure — same posture as SSM.

Cost: the endpoint itself is free; you pay only normal cross-AZ data transfer. Put the endpoint
in the same AZ as the target to avoid that.

### Test to run once an endpoint exists

```sh
aws ec2-instance-connect open-tunnel \
  --instance-connect-endpoint-id <eice-id> \
  --private-ip-address <target-ip> --remote-port <port> --local-port 19080
# then benchmark the same object over both paths and compare against ~0.7 MB/s
```

## Other options, ranked

Any non-SSM path realistically lands at the home WAN limit (~8–10 MB/s), i.e. **12–15×** current.

1. **`ssh -C` over the existing tunnel** (`scripts/aws-fast-tunnel.sh`) — free, no AWS change,
   works today. Measured 3.8× on a Postgres bulk read. Gains scale with how compressible the
   payload is; do not use it for already-compressed or encrypted traffic.
2. **Do the work remotely** — run the dump on the bastion, compress there, ship via S3 and pull
   at full WAN speed. Zero infra change; best answer for large dumps.
3. **Tailscale / WireGuard subnet router on the bastion** — outbound-only UDP, no inbound ports,
   no public IP, so the security posture matches SSM. Bypasses AWS's websocket relay entirely.
   Note the bastion instance size becomes the constraint once it is a real data path.
4. **More SSM sessions** — stays inside the current security model. Now supported via `replicas`
   (config or the status UI). Does **not** help a single stream; N sessions = N × 0.7 MB/s for
   multi-connection workloads.
5. **AWS Client VPN / Site-to-Site VPN** — full bandwidth, proper solution, but meaningful
   recurring cost, and Site-to-Site needs a stable public IP at the other end.

Cloudflare Tunnel would also work technically but routes traffic through a third party — a
governance decision, not a technical one.

## Unrelated observation

The dev bastion's security group permits inbound TCP 22 from `0.0.0.0/0`, with a rule
description claiming it is restricted to corporate networks. The instance has no public IP, so
current exposure is VPC-internal only, but the rule does not do what its description says.
