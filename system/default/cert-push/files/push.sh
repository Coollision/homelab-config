#!/usr/bin/env bash
# cert-push: push the cert-manager wildcard to the NAS (DSM web API) and the UniFi console (HTTPS API),
# prove what each one serves afterwards, and exit non-zero on anything off. See wiki/01-platform/cert-push.md.
#
# Inputs (files): /certs/tls.crt + tls.key (wildcard Secret), /creds/<key> (Vault-backed Secret).
# Inputs (env):   TARGETS="nas unifi"  DRYRUN=1 (probe only, no login)  FORCE=1 (push even if up to date)
#                 INSECURE=1 (skip TLS verification of the device API, break-glass only, never a default)
set -uo pipefail

CERT_DIR=${CERT_DIR:-/certs}
CRED_DIR=${CRED_DIR:-/creds}
WORK=${WORK:-/tmp/work}
TARGETS=${TARGETS:-"nas unifi"}
DRYRUN=${DRYRUN:-0}
FORCE=${FORCE:-0}
INSECURE=${INSECURE:-0}
MIN_SERVED_DAYS=${MIN_SERVED_DAYS:-14}
OVERDUE_GRACE_DAYS=${OVERDUE_GRACE_DAYS:-3}
VERIFY_WAIT=${VERIFY_WAIT:-120}
CONN_RETRIES=${CONN_RETRIES:-3}   # attempts for a UniFi call that never connected (curl exit 6/7/35); 30s, 60s... apart
RETRY_BASE=${RETRY_BASE:-30}
NAS_API_PORT=${NAS_API_PORT:-5001}
UNIFI_API_PORT=${UNIFI_API_PORT:-443}
NAME_PREFIX=${NAME_PREFIX:-cert-push}

FAILS=0
SID=""
SYNOTOKEN=""
CSRF=""

log() { printf '%s %s\n' "$(date -u +%FT%TZ)" "$*"; }
fail() { log "FAIL: $*"; FAILS=$((FAILS + 1)); }
cred() { tr -d '\r\n' <"$CRED_DIR/$1"; }

# openssl prints "Nov  1 12:00:00 2027 GMT": GNU date, busybox date and BSD date (local dev) each need their own spelling.
epoch() {
  date -u -d "$1" +%s 2>/dev/null ||
    date -u -D '%b %e %H:%M:%S %Y GMT' -d "$1" +%s 2>/dev/null ||
    date -u -j -f '%b %e %H:%M:%S %Y %Z' "$1" +%s
}
fp_of() { openssl x509 -in "$1" -noout -fingerprint -"$2" 2>/dev/null | cut -d= -f2 | tr -d : | tr 'A-F' 'a-f'; }
cert_date() { openssl x509 -in "$1" -noout -"$2" | cut -d= -f2; }
iso() { date -u -r "$1" +%FT%TZ 2>/dev/null || date -u -d "@$1" +%FT%TZ; }

# ---------------------------------------------------------------------------------------------------------------------
# Source certificate
# ---------------------------------------------------------------------------------------------------------------------
prepare_source() {
  mkdir -p "$WORK" && chmod 700 "$WORK"
  [ -r "$CERT_DIR/tls.crt" ] && [ -r "$CERT_DIR/tls.key" ] || { fail "source tls.crt/tls.key not readable in $CERT_DIR"; return 1; }
  : >"$WORK/leaf.pem" && : >"$WORK/chain.pem"
  # leaf = first certificate, chain = the rest (for ACME issuers ca.crt is empty, the intermediates are in tls.crt)
  awk -v d="$WORK" '/-----BEGIN CERTIFICATE-----/{n++} n>=1{print > (n==1 ? d"/leaf.pem" : d"/chain.pem")}' "$CERT_DIR/tls.crt"
  cat "$WORK/leaf.pem" "$WORK/chain.pem" >"$WORK/fullchain.pem"
  cp "$CERT_DIR/tls.key" "$WORK/key.pem" && chmod 600 "$WORK/key.pem"
  openssl x509 -in "$WORK/leaf.pem" -noout 2>/dev/null || { fail "source leaf certificate is not parseable"; return 1; }

  local a b
  a=$(openssl x509 -in "$WORK/leaf.pem" -noout -pubkey | openssl sha256)
  b=$(openssl pkey -in "$WORK/key.pem" -pubout 2>/dev/null | openssl sha256)
  [ "$a" = "$b" ] || { fail "source tls.key does not match tls.crt"; return 1; }

  FP256=$(fp_of "$WORK/leaf.pem" sha256)
  FP1=$(fp_of "$WORK/leaf.pem" sha1)
  local nb na now life due
  nb=$(epoch "$(cert_date "$WORK/leaf.pem" startdate)")
  na=$(epoch "$(cert_date "$WORK/leaf.pem" enddate)")
  now=$(date -u +%s)
  life=$((na - nb))
  due=$((na - life / 3)) # cert-manager default renewBefore = 1/3 of the lifetime; no fixed day counts
  SRC_EXP=$(iso "$na")
  SRC_RENEW=$(iso "$due")
  [ "$na" -gt "$now" ] || { fail "source certificate already expired ($SRC_EXP)"; return 1; }
  if [ "$now" -gt $((due + OVERDUE_GRACE_DAYS * 86400)) ]; then
    fail "renewal overdue: planned $SRC_RENEW, more than ${OVERDUE_GRACE_DAYS}d ago (cert-manager is not renewing)"
  fi
  log "source: sha256=${FP256:0:16} expires=$SRC_EXP renewal_planned=$SRC_RENEW"
}

# ---------------------------------------------------------------------------------------------------------------------
# Probing: connect to the device address (not DNS), present the name via SNI
# ---------------------------------------------------------------------------------------------------------------------
probe_to_file() { # ip port sni outfile
  timeout 10 openssl s_client -connect "$1:$2" -servername "$3" </dev/null 2>/dev/null |
    openssl x509 -out "$4" 2>/dev/null
  [ -s "$4" ]
}

# verify_probes <label> <ip> <"name:port ..."> <wait-seconds>; sets SERVED_FP / SERVED_EXP from the first probe
verify_probes() {
  local label=$1 ip=$2 probes=$3 wait=$4 deadline bad p host port fp
  case "$probes" in "" | "-" | none) log "$label: no probes configured, served certificate not verified"; return 0 ;; esac
  deadline=$(($(date +%s) + wait))
  while :; do
    bad=""
    for p in $probes; do
      host=${p%:*}
      port=${p##*:}
      rm -f "$WORK/served.pem"
      if ! probe_to_file "$ip" "$port" "$host" "$WORK/served.pem"; then
        bad="$bad $p(unreachable)"
        continue
      fi
      fp=$(fp_of "$WORK/served.pem" sha256)
      [ -n "${SERVED_FP:-}" ] || {
        SERVED_FP=${fp:0:16}
        SERVED_EXP=$(cert_date "$WORK/served.pem" enddate)
      }
      if [ "$fp" != "$FP256" ]; then
        bad="$bad $p(serves-other:${fp:0:12})"
        continue
      fi
      openssl x509 -in "$WORK/served.pem" -noout -checkend $((MIN_SERVED_DAYS * 86400)) >/dev/null ||
        bad="$bad $p(expires-in-<${MIN_SERVED_DAYS}d)"
    done
    if [ -z "$bad" ]; then
      log "$label: all probes serve the managed certificate ($probes)"
      return 0
    fi
    if [ "$(date +%s)" -ge "$deadline" ]; then
      LAST_BAD=$bad
      return 1
    fi
    SERVED_FP=""
    sleep 10
  done
}

summary() { # target action
  log "SUMMARY target=$1 served=${SERVED_FP:-n/a} served_expiry=\"${SERVED_EXP:-n/a}\" source_expiry=$SRC_EXP renewal_planned=$SRC_RENEW action=$2"
}

# ---------------------------------------------------------------------------------------------------------------------
# NAS: DSM web API (SYNO.API.Auth + SYNO.Core.Certificate), same calls the acme.sh synology_dsm hook uses
# ---------------------------------------------------------------------------------------------------------------------
nas_logout() {
  [ -n "$SID" ] || return 0
  "${NAS_CURL[@]}" -X POST "$NAS_BASE/webapi/auth.cgi" -d api=SYNO.API.Auth -d version=3 -d method=logout \
    --data-urlencode "_sid=$SID" >/dev/null 2>&1 || true
  SID=""
}

nas_login() {
  local out
  # credentials go in a POST body read from files (never the URL, never argv)
  out=$("${NAS_CURL[@]}" -X POST "$NAS_BASE/webapi/auth.cgi" -d api=SYNO.API.Auth -d version=3 -d method=login \
    -d format=sid -d enable_syno_token=yes \
    --data-urlencode "account@$CRED_DIR/nas-username" --data-urlencode "passwd@$CRED_DIR/nas-password") || {
    fail "nas: login request failed (network or TLS verification)"
    return 1
  }
  if [ "$(printf '%s' "$out" | jq -r '.success // false')" != "true" ]; then
    fail "nas: login refused (error code $(printf '%s' "$out" | jq -r '.error.code // "?"')); NOT retrying, DSM auto-block counts failures"
    return 1
  fi
  SID=$(printf '%s' "$out" | jq -r '.data.sid')
  SYNOTOKEN=$(printf '%s' "$out" | jq -r '.data.synotoken // empty')
}

nas_list() { # prints "id<TAB>desc<TAB>is_default" of the managed certificate, if any
  "${NAS_CURL[@]}" -X POST "$NAS_BASE/webapi/entry.cgi" -H "X-SYNO-TOKEN: $SYNOTOKEN" \
    -d api=SYNO.Core.Certificate.CRT -d method=list -d version=1 --data-urlencode "_sid=$SID" |
    jq -r --arg p "$NAME_PREFIX " '.data.certificates[]? | select((.desc // "") | startswith($p)) | [.id, .desc, (.is_default | tostring)] | @tsv' |
    head -n1
}

nas_import() { # id desc as_default(true|false)
  local out chain=() dflt=()
  [ -s "$WORK/chain.pem" ] && chain=(-F "inter_cert=@$WORK/chain.pem;filename=chain.pem")
  # DSM treats a PRESENT as_default as true whatever its value (observed: as_default=false made the first import the default).
  # So the field is only sent when the managed certificate already is the default, to keep that status on an update.
  [ "$3" = "true" ] && dflt=(-F "as_default=true")
  out=$("${NAS_CURL[@]}" -X POST \
    "$NAS_BASE/webapi/entry.cgi?api=SYNO.Core.Certificate&method=import&version=1&SynoToken=$SYNOTOKEN&_sid=$SID" \
    -F "key=@$WORK/key.pem;filename=privkey.pem" -F "cert=@$WORK/leaf.pem;filename=cert.pem" "${chain[@]}" \
    -F "id=$1" -F "desc=$2" "${dflt[@]}") || {
    fail "nas: import request failed"
    return 1
  }
  if [ "$(printf '%s' "$out" | jq -r '.success // false')" != "true" ]; then
    fail "nas: import refused: $(printf '%s' "$out" | jq -c '.error // .')"
    return 1
  fi
  log "nas: import ok ($(printf '%s' "$out" | jq -c '.data // {}'))"
}

run_nas() {
  local ip host probes want entry id desc def action="none"
  SERVED_FP="" SERVED_EXP=""
  ip=$(cred nas-ip)
  host=$(cred nas-host)
  probes=$(cred nas-probes)
  NAS_BASE="https://$host:$NAS_API_PORT"
  NAS_CURL=(curl -sS --max-time 120 --resolve "$host:$NAS_API_PORT:$ip")
  [ "$INSECURE" = "1" ] && NAS_CURL+=(-k)
  want="$NAME_PREFIX ${FP256:0:12}"

  if [ "$FORCE" != "1" ] && verify_probes nas "$ip" "$probes" 0; then
    case "$probes" in "" | "-" | none) ;; *) summary nas "up-to-date"; return 0 ;; esac
  elif [ "$FORCE" != "1" ]; then
    log "nas: probe says not up to date:${LAST_BAD:-} (proceeding to push)"
  fi
  if [ "$DRYRUN" = "1" ]; then
    log "nas: DRYRUN, would log in and import '$want'"
    verify_probes nas "$ip" "$probes" 0 || log "nas: DRYRUN finding:${LAST_BAD:-} (would be pushed)"
    summary nas "dryrun"
    return 0
  fi

  nas_login || { summary nas "login-failed"; return 1; }
  entry=$(nas_list)
  IFS=$'\t' read -r id desc def <<<"$entry"
  if [ -n "$entry" ] && [ "$desc" = "$want" ] && [ "$FORCE" != "1" ]; then
    log "nas: certificate '$want' already imported (id $id)"
    action="already-imported"
  else
    # same id => DSM keeps the service bindings and restarts what is bound to it; first run has no id and creates it
    nas_import "${id:-}" "$want" "$([ "$def" = "true" ] && echo true || echo false)" && action="imported" || action="import-failed"
  fi
  if [ "$action" != "import-failed" ]; then
    entry=$(nas_list)
    case "$entry" in *"$want"*) ;; *) fail "nas: '$want' not listed after import"; action="not-listed" ;; esac
    # guard: an import must never change which certificate is the default
    if [ "$def" != "true" ] && [ "${entry##*$'\t'}" = "true" ]; then
      fail "nas: the managed certificate became the DSM default by itself, set the previous default back in Security > Certificate"
      action="$action,default-changed"
    fi
  fi
  nas_logout
  [ "$action" = "import-failed" ] || [ "$action" = "not-listed" ] && { summary nas "$action"; return 1; }

  if ! verify_probes nas "$ip" "$probes" "$VERIFY_WAIT"; then
    fail "nas: served certificate is not the managed one:${LAST_BAD:-}"
    summary nas "$action,verify-failed"
    return 1
  fi
  summary nas "$action,verified"
}

# ---------------------------------------------------------------------------------------------------------------------
# UniFi OS 6: user certificates through the console's own API (undocumented, verify on the real console)
#   create -> activate -> verify -> (roll back on failure) -> only then delete every other managed certificate
# ---------------------------------------------------------------------------------------------------------------------
# ucall <method> <path> [json-file]: HTTP status in $HTTP, body in $WORK/u.body, CSRF token refreshed from every response
ucall() {
  local args
  args=("${UCURL[@]}" -X "$1" -D "$WORK/u.hdr" -o "$WORK/u.body" -w '%{http_code}' -H 'Content-Type: application/json')
  [ -n "$CSRF" ] && args+=(-H "X-CSRF-Token: $CSRF")
  [ -n "${3:-}" ] && args+=(--data-binary "@$3")
  # Only retry when no HTTP exchange happened (DNS/connect/TLS handshake failure: curl exit 6, 7, 35): nothing reached the
  # console, so a retry cannot double-submit or count as a failed login. Any HTTP status, even 5xx, is never retried.
  local try=1 rc
  while :; do
    HTTP=$("${args[@]}" "$UNIFI_BASE$2")
    rc=$?
    [ "$rc" -eq 0 ] || HTTP=000
    case "$rc" in 6 | 7 | 35) ;; *) break ;; esac
    [ "$try" -lt "$CONN_RETRIES" ] || break
    log "unifi: $1 $2 could not connect (curl exit $rc), retry $try/$((CONN_RETRIES - 1)) in $((RETRY_BASE * try))s"
    sleep $((RETRY_BASE * try))
    try=$((try + 1))
  done
  local nc
  nc=$(awk -F': ' 'tolower($1)=="x-updated-csrf-token" || tolower($1)=="x-csrf-token" {print $2}' "$WORK/u.hdr" 2>/dev/null | tr -d '\r' | tail -n1)
  [ -z "$nc" ] || CSRF=$nc
}

body_hint() { head -c 300 "$WORK/u.body" 2>/dev/null | tr '\n\r' '  '; }

# normalise the list response (array or wrapped) into [{id,name,active,exp}] with exp as epoch seconds or null
UNIFI_NORMALISE='
  def ts: if type=="number" then (if . > 100000000000 then ./1000 else . end)
          elif type=="string" then ((sub("\\.[0-9]+";"") | fromdateiso8601?) // null) else null end;
  (if type=="array" then . else (.data // .certificates // []) end)
  | map({id: (.id // .uuid), name: (.name // ""), active: (.active // .isActive // false),
         exp: ((.valid_to // .expiresAt // .validTo // .notAfter // .expires // null) | ts)})'

# sets LIST (not run in a subshell, ucall must be able to refresh the CSRF token)
unifi_list() {
  ucall GET /api/userCertificates
  [ "$HTTP" = "200" ] && LIST=$(jq -c "$UNIFI_NORMALISE" "$WORK/u.body")
}

unifi_status() { # id true|false
  printf '{"active":%s}' "$2" >"$WORK/status.json"
  ucall PUT "/api/userCertificates/$1/status" "$WORK/status.json"
  [ "$HTTP" = "200" ] || [ "$HTTP" = "204" ]
}

run_unifi() {
  local ip host probes want id prev action="none" now n
  SERVED_FP="" SERVED_EXP=""
  ip=$(cred unifi-ip)
  host=$(cred unifi-host)
  probes=$(cred unifi-probes)
  case "$probes" in "" | "-" | none) probes="$host:$UNIFI_API_PORT" ;; esac
  UNIFI_BASE="https://$host:$UNIFI_API_PORT"
  UCURL=(curl -sS --max-time 120 --resolve "$host:$UNIFI_API_PORT:$ip" -b "$WORK/u.jar" -c "$WORK/u.jar")
  [ "$INSECURE" = "1" ] && UCURL+=(-k)
  want="$NAME_PREFIX ${FP1:0:8}"

  if [ "$FORCE" != "1" ] && verify_probes unifi "$ip" "$probes" 0; then
    summary unifi "up-to-date"
    return 0
  fi
  [ "$FORCE" = "1" ] || log "unifi: probe says not up to date:${LAST_BAD:-} (proceeding to push)"
  if [ "$DRYRUN" = "1" ]; then
    log "unifi: DRYRUN, would log in and create/activate '$want'"
    verify_probes unifi "$ip" "$probes" 0 || log "unifi: DRYRUN finding:${LAST_BAD:-} (would be pushed)"
    summary unifi "dryrun"
    return 0
  fi

  jq -n --rawfile u "$CRED_DIR/unifi-username" --rawfile p "$CRED_DIR/unifi-password" \
    '{username: ($u | rtrimstr("\n")), password: ($p | rtrimstr("\n")), rememberMe: false}' >"$WORK/login.json"
  ucall POST /api/auth/login "$WORK/login.json"
  rm -f "$WORK/login.json"
  [ "$HTTP" = "200" ] || { fail "unifi: login refused or failed (HTTP $HTTP): $(body_hint)"; summary unifi "login-failed"; return 1; }

  unifi_list || { fail "unifi: listing user certificates failed (HTTP $HTTP, 403 = account lacks the certificate permission, 404 = endpoint changed): $(body_hint)"; summary unifi "list-failed"; return 1; }
  prev=$(jq -r '[.[] | select(.active)][0].id // empty' <<<"$LIST")
  id=$(jq -r --arg n "$want" '[.[] | select(.name == $n)][0].id // empty' <<<"$LIST")

  if [ -z "$id" ]; then
    jq -n --arg name "$want" --rawfile cert "$WORK/fullchain.pem" --rawfile key "$WORK/key.pem" \
      '{name: $name, cert: $cert, key: $key}' >"$WORK/payload.json"
    ucall POST /api/userCertificates "$WORK/payload.json"
    rm -f "$WORK/payload.json"
    case "$HTTP" in 200 | 201) ;; *) fail "unifi: create refused (HTTP $HTTP): $(body_hint)"; summary unifi "create-failed"; return 1 ;; esac
    id=$(jq -r '.id // .uuid // .data.id // empty' "$WORK/u.body")
    [ -n "$id" ] || { fail "unifi: create returned no id"; summary unifi "create-failed"; return 1; }
    action="created"
  else
    action="exists"
  fi

  unifi_status "$id" true || { fail "unifi: activate refused (HTTP $HTTP): $(body_hint)"; summary unifi "$action,activate-failed"; return 1; }
  if ! verify_probes unifi "$ip" "$probes" "$VERIFY_WAIT"; then
    fail "unifi: console does not serve the new certificate:${LAST_BAD:-}"
    if [ -n "$prev" ] && [ "$prev" != "$id" ]; then
      if unifi_status "$prev" true; then
        log "unifi: rolled back to previous certificate $prev"
      else
        fail "unifi: rollback to $prev FAILED, reactivate it in the console UI"
      fi
    fi
    summary unifi "$action,verify-failed"
    return 1
  fi

  # cleanup only after a passing verify: keep exactly ONE pushed certificate. Every other managed (name prefix) certificate that is
  # not the active new one goes, expired or not. Certificates we did not create (other names) are never touched. The rollback copy
  # therefore only exists during a run; the console UI and the NAS keep their own copies.
  unifi_list || LIST="[]"
  n=0
  for old in $(jq -r --arg p "$NAME_PREFIX " --arg id "$id" \
    '.[] | select((.name | startswith($p)) and (.id != $id) and (.active | not)) | .id' <<<"$LIST"); do
    ucall DELETE "/api/userCertificates/$old"
    if [ "$HTTP" = "200" ] || [ "$HTTP" = "204" ]; then
      n=$((n + 1))
    else
      log "unifi: cleanup of $old failed (HTTP $HTTP): $(body_hint)"
    fi
  done
  [ "$n" -eq 0 ] || log "unifi: removed $n older managed certificate(s)"
  summary unifi "$action,verified"
}

# ---------------------------------------------------------------------------------------------------------------------
trap nas_logout EXIT
[ "$INSECURE" = "1" ] && log "WARNING: INSECURE=1, device API TLS is NOT verified (break-glass)"
[ "$DRYRUN" = "1" ] && log "DRYRUN=1: probe + compare + expiry checks only, no login"
prepare_source || exit 1
for t in $TARGETS; do
  case "$t" in
    nas) run_nas || FAILS=$((FAILS + 1)) ;;
    unifi) run_unifi || FAILS=$((FAILS + 1)) ;;
    *) fail "unknown target '$t'" ;;
  esac
done
[ "$FAILS" -eq 0 ] && { log "OK"; exit 0; }
log "FAILED ($FAILS problem(s))"
exit 1
