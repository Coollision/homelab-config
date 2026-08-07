#!/usr/bin/env bash
# Compressed AWS tunnel: reach a private host through an SSM bastion at ~3-4x the speed of a
# plain `aws ssm start-session` port-forward.
#
# Why this is faster: a single SSM session is rate-limited by AWS to ~0.7 MB/s regardless of
# CPU, RTT or instance size (see docs/aws-tunnels-throughput.md). That cap is on the *wire*
# bytes, so the only way to move more data is to send fewer bytes. This script forwards
# bastion:22 over SSM, then runs `ssh -C` through it — gzip happens ON THE BASTION, before the
# bytes enter the bottleneck. Measured on a Postgres bulk COPY: 0.62 -> 2.36 MB/s (3.8x).
#
# Authentication uses EC2 Instance Connect ephemeral keys (valid 60s, pushed just-in-time), so
# there is no long-lived key material to manage or rotate. Nothing is left on the bastion.
#
# Only compress compressible traffic. Encrypted or already-compressed payloads (TLS, git
# packfiles, gzip, images, parquet) get *slower* with -C — measured 0.61 -> 0.56 MB/s. For a
# database this means connecting WITHOUT TLS (sslmode=disable): SSH already encrypts the hop,
# and a TLS payload is incompressible. Requires rds.force_ssl=0 on the target.
#
# Usage:
#   scripts/aws-fast-tunnel.sh --bastion bastion-dev --target db.internal --port 5432 --local 15432
#   scripts/aws-fast-tunnel.sh --bastion bastion-dev --target gitlab.internal --port 81 --local 18080
#   scripts/aws-fast-tunnel.sh --bastion bastion-dev --target db.internal --port 5432 --local 15432 \
#       --profile my-sso-profile --region eu-central-1
#
# Then, in another shell:
#   psql "host=127.0.0.1 port=15432 user=... dbname=... sslmode=disable"
#
# Runs in the foreground; Ctrl-C tears down the tunnel, the SSH process and the temp key.
set -euo pipefail

BASTION="" TARGET="" PORT="" LOCAL="" PROFILE="" REGION="${AWS_REGION:-eu-central-1}"
OS_USER="ec2-user" NO_COMPRESS=0

usage() { sed -n '2,30p' "$0" | sed 's/^# \{0,1\}//'; exit "${1:-1}"; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --bastion)   BASTION="$2"; shift 2 ;;
    --target)    TARGET="$2";  shift 2 ;;
    --port)      PORT="$2";    shift 2 ;;
    --local)     LOCAL="$2";   shift 2 ;;
    --profile)   PROFILE="$2"; shift 2 ;;
    --region)    REGION="$2";  shift 2 ;;
    --os-user)   OS_USER="$2"; shift 2 ;;
    --no-compress) NO_COMPRESS=1; shift ;;
    -h|--help)   usage 0 ;;
    *) echo "unknown argument: $1" >&2; usage ;;
  esac
done

[[ -n "$BASTION" && -n "$TARGET" && -n "$PORT" && -n "$LOCAL" ]] || usage

AWS=(aws --region "$REGION")
[[ -n "$PROFILE" ]] && AWS+=(--profile "$PROFILE")

# Pick a free loopback port for the bastion SSH forward so parallel tunnels don't collide.
ssh_port=$(python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()')
workdir=$(mktemp -d); key="$workdir/eic"
ssm_pid="" ssh_pid=""

cleanup() {
  [[ -n "$ssh_pid" ]] && kill "$ssh_pid" 2>/dev/null || true
  # `aws ssm start-session` forks session-manager-plugin as a child; killing only the aws
  # wrapper orphans the plugin, which keeps the SSM session and the local port alive.
  # Reap the children first, then the wrapper.
  if [[ -n "$ssm_pid" ]]; then
    pkill -P "$ssm_pid" 2>/dev/null || true
    kill "$ssm_pid" 2>/dev/null || true
  fi
  rm -rf "$workdir"
}
trap cleanup EXIT INT TERM

wait_port() {  # wait_port <port> <seconds> <label>
  local p="$1" limit="$2" label="$3" i=0
  while (( i < limit * 5 )); do
    nc -z 127.0.0.1 "$p" >/dev/null 2>&1 && return 0
    sleep 0.2; ((i++))
  done
  echo "timed out waiting for $label on 127.0.0.1:$p" >&2; return 1
}

echo "resolving bastion '$BASTION'..."
instance_id=$("${AWS[@]}" ec2 describe-instances \
  --filters "Name=tag:Name,Values=$BASTION" "Name=instance-state-name,Values=running" \
  --query 'Reservations[].Instances[0].InstanceId' --output text)
[[ -n "$instance_id" && "$instance_id" != "None" ]] || { echo "no running instance tagged '$BASTION'" >&2; exit 1; }

az=$("${AWS[@]}" ec2 describe-instances --instance-ids "$instance_id" \
  --query 'Reservations[].Instances[0].Placement.AvailabilityZone' --output text)
echo "  $instance_id ($az)"

echo "opening SSM forward to bastion:22 -> 127.0.0.1:$ssh_port ..."
"${AWS[@]}" ssm start-session --target "$instance_id" \
  --document-name AWS-StartPortForwardingSession \
  --parameters "{\"portNumber\":[\"22\"],\"localPortNumber\":[\"$ssh_port\"]}" >/dev/null 2>&1 &
ssm_pid=$!
wait_port "$ssh_port" 30 "SSM forward"

# EC2 Instance Connect keys expire after 60s, so push immediately before connecting.
ssh-keygen -t ed25519 -N "" -f "$key" -q -C "aws-fast-tunnel"
echo "pushing ephemeral SSH key (valid 60s)..."
"${AWS[@]}" ec2-instance-connect send-ssh-public-key \
  --instance-id "$instance_id" --instance-os-user "$OS_USER" --availability-zone "$az" \
  --ssh-public-key "file://$key.pub" >/dev/null

compress=(-C); label="compressed"
(( NO_COMPRESS )) && { compress=(); label="uncompressed"; }

echo "opening $label tunnel 127.0.0.1:$LOCAL -> $TARGET:$PORT ..."
ssh "${compress[@]}" -i "$key" -p "$ssh_port" \
  -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR \
  -o ServerAliveInterval=30 -o ServerAliveCountMax=3 -o ExitOnForwardFailure=yes \
  -N -L "$LOCAL:$TARGET:$PORT" "$OS_USER@127.0.0.1" &
ssh_pid=$!
wait_port "$LOCAL" 30 "SSH forward"

cat <<EOF

  ready: 127.0.0.1:$LOCAL  ->  $TARGET:$PORT   ($label, via $BASTION)

  For databases connect WITHOUT TLS so the payload stays compressible:
      psql "host=127.0.0.1 port=$LOCAL user=<user> dbname=<db> sslmode=disable"

  Ctrl-C to tear down.
EOF

wait "$ssh_pid"
