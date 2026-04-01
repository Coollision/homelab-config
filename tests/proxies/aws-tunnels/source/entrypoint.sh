#!/bin/sh
# Required env vars (set per-deployment):
#   BASTION_NAME  — EC2 Name tag, e.g. bastion-dev / bastion-test / bastion-preprod
#   REMOTE_HOST   — private hostname/IP reachable from the bastion
#   REMOTE_PORT   — port of the remote service
#   LOCAL_PORT    — port this container listens on (default 8080)
: "${BASTION_NAME:?BASTION_NAME is required}"
: "${REMOTE_HOST:?REMOTE_HOST is required}"
: "${REMOTE_PORT:?REMOTE_PORT is required}"
TUNNEL_NAME="${TUNNEL_NAME:-aws-tunnel}"
LOCAL_PORT="${LOCAL_PORT:-8080}"
LAST_LOGIN="/aws-creds/.last-login"
STATUS_DIR="/aws-creds/tunnel-status"
STATE_FILE="${STATUS_DIR}/${TUNNEL_NAME}.state"
ERROR_FILE="${STATUS_DIR}/${TUNNEL_NAME}.error"
RETRY_CRED=30
RETRY_ERROR=10

mkdir -p "$STATUS_DIR"

set_state() {
  state="$1"
  message="$2"
  printf '%s\n' "$state" > "$STATE_FILE"
  if [ -n "$message" ]; then
    printf '%s\n' "$message" > "$ERROR_FILE"
  else
    : > "$ERROR_FILE"
  fi
}

get_login_mtime() {
  if [ -f "$LAST_LOGIN" ]; then
    stat -c '%Y' "$LAST_LOGIN" 2>/dev/null || stat -f '%m' "$LAST_LOGIN" 2>/dev/null
  fi
}

wait_for_new_login() {
  reason="$1"
  set_state "auth_required" "$reason"
  echo "[tunnel] ${reason} — waiting for a new login signal from auth server"

  base_mtime=$(get_login_mtime)

  while true; do
    new_mtime=$(get_login_mtime)

    if [ -n "$new_mtime" ] && [ "$new_mtime" != "$base_mtime" ]; then
      echo "[tunnel] New login signal detected — retrying tunnel setup"
      set_state "reconnecting" "new login signal detected"
      return 0
    fi

    sleep 5
  done
}

if ! command -v session-manager-plugin > /dev/null 2>&1; then
  echo "[tunnel] Installing SSM Session Manager plugin..."
  ARCH=$(uname -m)
  if [ "$ARCH" = "aarch64" ]; then
    SSM_URL="https://s3.amazonaws.com/session-manager-downloads/plugin/latest/linux_arm64/session-manager-plugin.rpm"
  else
    SSM_URL="https://s3.amazonaws.com/session-manager-downloads/plugin/latest/linux_64bit/session-manager-plugin.rpm"
  fi
  curl -fsSL "$SSM_URL" -o /tmp/ssm-plugin.rpm
  rpm -i /tmp/ssm-plugin.rpm && rm /tmp/ssm-plugin.rpm
fi

set_state "starting" "tunnel process started"

KNOWN_LOGIN_MTIME=""
while true; do
  if ! ls /root/.aws/sso/cache/*.json > /dev/null 2>&1; then
    wait_for_new_login "no SSO token found"
    continue
  fi

  CURRENT_MTIME=$(get_login_mtime)
  if [ -n "$KNOWN_LOGIN_MTIME" ] && [ "$CURRENT_MTIME" != "$KNOWN_LOGIN_MTIME" ]; then
    echo "[tunnel] .last-login updated — reconnecting..."
  fi
  KNOWN_LOGIN_MTIME="$CURRENT_MTIME"

  echo "[tunnel] Resolving instance ID for '${BASTION_NAME}'..."
  INSTANCE_ID=$(aws ec2 describe-instances \
    --region eu-central-1 \
    --filters \
      "Name=tag:Name,Values=${BASTION_NAME}" \
      "Name=instance-state-name,Values=running" \
    --query "Reservations[0].Instances[0].InstanceId" \
    --output text 2>&1)

  if [ -z "$INSTANCE_ID" ] || [ "$INSTANCE_ID" = "None" ]; then
    set_state "error" "no running bastion found for ${BASTION_NAME}"
    echo "[tunnel] ERROR: No running instance found for '${BASTION_NAME}' — retrying in ${RETRY_ERROR}s"
    sleep $RETRY_ERROR
    continue
  fi

  if echo "$INSTANCE_ID" | grep -qi "error\|expired\|token\|AuthFailure\|InvalidClientToken"; then
    wait_for_new_login "AWS credentials error"
    continue
  fi

  echo "[tunnel] Bastion: ${BASTION_NAME} → ${INSTANCE_ID}"
  echo "[tunnel] Forwarding: 0.0.0.0:${LOCAL_PORT} → ${REMOTE_HOST}:${REMOTE_PORT}"
  set_state "running" "forwarding ${LOCAL_PORT} to ${REMOTE_HOST}:${REMOTE_PORT}"

  SSM_LOG=$(mktemp)
  aws ssm start-session \
    --region eu-central-1 \
    --target "$INSTANCE_ID" \
    --document-name AWS-StartPortForwardingSessionToRemoteHost \
    --parameters "{\"host\":[\"${REMOTE_HOST}\"],\"portNumber\":[\"${REMOTE_PORT}\"],\"localPortNumber\":[\"${LOCAL_PORT}\"]}" >"$SSM_LOG" 2>&1 &
  SSM_PID=$!

  RESTART_SIGNALLED=0
  while kill -0 "$SSM_PID" >/dev/null 2>&1; do
    LATEST_MTIME=$(get_login_mtime)
    if [ -n "$KNOWN_LOGIN_MTIME" ] && [ -n "$LATEST_MTIME" ] && [ "$LATEST_MTIME" != "$KNOWN_LOGIN_MTIME" ]; then
      echo "[tunnel] Restart signal detected while session is active — reconnecting now"
      set_state "reconnecting" "restart signal received"
      kill "$SSM_PID" >/dev/null 2>&1 || true
      RESTART_SIGNALLED=1
      KNOWN_LOGIN_MTIME="$LATEST_MTIME"
      break
    fi
    sleep 2
  done

  wait "$SSM_PID" >/dev/null 2>&1
  EXIT_CODE=$?
  SSM_OUTPUT=$(cat "$SSM_LOG")
  rm -f "$SSM_LOG"
  printf '%s\n' "$SSM_OUTPUT"

  if [ "$RESTART_SIGNALLED" = "1" ]; then
    continue
  fi

  if echo "$SSM_OUTPUT" | grep -qi "expired\|token\|AuthFailure\|InvalidClientToken\|Unauthorized\|AccessDenied"; then
    wait_for_new_login "AWS credentials expired/invalid during SSM session"
    continue
  fi

  set_state "error" "SSM session exited with code ${EXIT_CODE}"
  echo "[tunnel] SSM session exited (code ${EXIT_CODE}) — retrying in ${RETRY_ERROR}s"
  sleep $RETRY_ERROR
done
