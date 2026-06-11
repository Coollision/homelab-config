#!/usr/bin/env bash
#
# import-sso-token.sh — capture an AWS SSO token on your workstation and push it into the cluster
# so the aws-tunnels-operator can silently refresh STS credentials from it (no corporate password,
# no browser in the cluster).
#
# How it works:
#   1. `aws sso login` opens YOUR browser; you approve the device code + MFA once, here, on your
#      machine. This writes a token cache (access token + REFRESH token + OIDC client registration)
#      to ~/.aws/sso/cache/.
#   2. This script uploads that cache into the <stack>-sso-token Secret. The operator seeds it onto
#      disk and runs `aws configure export-credentials`, which refreshes the access token from the
#      refresh token automatically — until the underlying SSO session expires, then re-run this.
#
# PREREQUISITE: your ~/.aws/config must define a matching [sso-session] block, because the token
# cache is keyed by the session name and the operator looks it up by the same name (= the stack
# name). Example:
#
#   [sso-session aws-tunnels]
#   sso_start_url = https://my-sso.awsapps.com/start
#   sso_region    = eu-west-1
#   sso_registration_scopes = sso:account:access     # REQUIRED — this is what mints a refresh token
#
# Usage:
#   NAMESPACE=proxies STACK=aws-tunnels ./import-sso-token.sh
#   SKIP_LOGIN=1 ./import-sso-token.sh        # reuse an existing cache, don't trigger a new login
#
set -euo pipefail

NAMESPACE="${NAMESPACE:-proxies}"
STACK="${STACK:-aws-tunnels}"
# The sso-session name. MUST match the [sso-session NAME] block in ~/.aws/config and the operator's
# session name (which is the stack name). Override only for multi-start-URL setups.
SESSION="${SESSION:-$STACK}"
CACHE_DIR="${CACHE_DIR:-$HOME/.aws/sso/cache}"
SECRET_NAME="${SECRET_NAME:-${STACK}-sso-token}"

sha1hex() {
  if command -v sha1sum >/dev/null 2>&1; then
    sha1sum | awk '{print $1}'
  else
    shasum -a 1 | awk '{print $1}'   # macOS
  fi
}

if [[ "${SKIP_LOGIN:-0}" != "1" ]]; then
  echo ">> aws sso login --sso-session ${SESSION}  (approve in your browser + phone — once)"
  aws sso login --sso-session "${SESSION}"
fi

# The token cache file for an sso-session is named sha1hex(session_name).json. Upload that plus the
# OIDC client-registration file(s) — and nothing else, so tokens for other sessions stay on your
# machine.
token_hash="$(printf '%s' "${SESSION}" | sha1hex)"
token_file="${CACHE_DIR}/${token_hash}.json"

if [[ ! -f "${token_file}" ]]; then
  echo "ERROR: no token cache at ${token_file}" >&2
  echo "       Did 'aws sso login --sso-session ${SESSION}' succeed, and does your ~/.aws/config" >&2
  echo "       define [sso-session ${SESSION}] with sso_registration_scopes = sso:account:access?" >&2
  exit 1
fi

args=(--from-file="${token_hash}.json=${token_file}")
shopt -s nullglob
for reg in "${CACHE_DIR}"/botocore-client-id-*.json; do
  args+=(--from-file="$(basename "${reg}")=${reg}")
done

kubectl create secret generic "${SECRET_NAME}" \
  --namespace "${NAMESPACE}" \
  "${args[@]}" \
  --dry-run=client -o yaml | kubectl apply -f -

echo ">> imported token cache into secret ${NAMESPACE}/${SECRET_NAME}"
echo ">> the operator will now refresh STS creds automatically until the SSO session expires."
