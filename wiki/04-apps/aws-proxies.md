# AWS proxies

[← Back to Home](../Home.md) · See also: [Misc apps](misc-apps.md)

## aws-tunnels — `workload/proxies/aws-tunnels`

Runs an operator that opens SSM (AWS Systems Manager) port-forwarding sessions to
bastion hosts, exposing each as an in-cluster tunnel to an AWS-side service (an HTTP
service and several Postgres RDS instances across different environments). Uses
SSO-based credentials with automatic refresh, so tunnels survive long sessions without
manual re-authentication.

### The throughput ceiling — and why it isn't a bug in this config

Each tunnel session is capped at roughly **0.7 MB/s**, and this is **not** a CPU,
bandwidth, or bastion-side limit in this cluster — it was proven with controlled A/B
testing to be a fixed, AWS-side per-SSM-session rate limit that exists before the traffic
even reaches this cluster's networking. Don't re-investigate CPU limits or Traefik
config chasing this number; the resource `limits` on the tunnel pod are tuned for
*latency jitter*, not throughput, and a comment in the chart says so explicitly.

**The only lever that actually helps: compression over an SSH hop inside the tunnel**,
which cut real-world transfer time by roughly 4x on a test bulk data transfer. This is
why the database tunnels here layer an SSH connection *inside* the SSM session rather
than passing TLS straight through — passing TLS straight through (`tls.passthrough`)
means every byte crossing the tunnel is already-encrypted ciphertext, which can't be
compressed at all. Terminating TLS at the ingress layer instead creates a plaintext
segment inside the cluster that the SSH hop can compress — the client's own
`sslmode=require` connection to the ingress endpoint is unaffected. **This trade-off is
only safe as long as the target databases don't enforce server-side SSL** — it was
explicitly verified before enabling it, and would need re-verifying if that setting is
ever changed on the AWS side.

### Client connection gotcha

Because tunnel routing is done by TLS **SNI** at the ingress layer, a client **must**
initiate a TLS handshake to be routed at all — a plaintext connection attempt sends no
SNI and simply times out with no useful error, which looks exactly like "the tunnel is
down" even though it's healthy. Always connect with SSL required, using a plain
Postgres/MySQL driver — not a topology-aware "smart" driver (e.g. an Aurora-cluster-aware
JDBC wrapper), which will try to discover and connect directly to instance endpoints that
aren't reachable through a single-host tunnel at all and will simply hang.

## aws-mcp

An MCP server exposing read-only AWS API access, following the same "agent tooling via
MCP" pattern as [ha-mcp and kubernetes-mcp-server](../02-smarthome/automation-mcp.md).
**Not currently deployed** — the working version exists only on an unmerged feature
branch; what's present on the main branch under this path today is leftover build
artifacts (a lockfile and a cached dependency archive, no actual chart), not a live
service. Don't be confused into thinking this is running.
