#!/bin/sh
# Init-container reconciler for the Mosquitto dynamic-security state.
#
# Declarative (this file + Vault, reapplied every pod start):
#   - the default ACL policy (deny all)
#   - the roles r-<user> and their ACLs, exactly as listed in clients.conf (grants dropped from
#     the file are revoked; see the format at the top of clients.conf)
#   - the managed users of clients.conf: passwords come from Vault placeholders, rotated every start
# Imperative (survives restarts, never touched here):
#   - any user/group created at runtime with mosquitto_ctrl, including their role assignments
#     (roles are only deleted via a `retired-user` line, so a runtime user bound to r-shelly keeps it)
#
# Mechanics: a throwaway broker on 127.0.0.1:18830 loads the same state file, we drive it
# with mosquitto_ctrl, then stop it. The plugin saves the file after every change.
#
# Failure policy: the broker is a single pod, so a failing reconcile must not become an outage.
# With an existing state file we log loudly and start on the last known state; on a fresh
# volume (nothing to fall back to) we exit non-zero so the crash-loop is visible.
set -u

DB=/mosquitto/data/dynamic-security.json
CFG=/mosquitto/config
PORT=18830

ADMIN="$(cat $CFG/admin-credential)"
ADMIN_USER="${ADMIN%%:*}"
ADMIN_PASS="${ADMIN#*:}"

# "true" gives every managed role read/write on '#' (break-glass, set in kustomization.yaml).
LEARNING="$(cat $CFG/acl-learning 2>/dev/null || echo false)"

HAD_DB=false
[ -f "$DB" ] && HAD_DB=true
ERRORS=0

finish() { # finish <code-for-fresh-volume>
  [ -n "${BROKER:-}" ] && { kill "$BROKER" 2>/dev/null; wait "$BROKER" 2>/dev/null; }
  [ -f "$DB" ] && { chown 1883:1883 "$DB"; chmod 0600 "$DB"; }
  if [ "$ERRORS" -gt 0 ]; then
    echo "reconcile: FINISHED WITH $ERRORS ERROR(S) (had_db=$HAD_DB)"
    $HAD_DB && exit 0
    exit "$1"
  fi
  echo "reconcile: done ($(grep -c . /tmp/roles) roles, learning=$LEARNING)"
  exit 0
}

cat > /tmp/bootstrap.conf <<CONF
listener $PORT 127.0.0.1
allow_anonymous false
plugin /usr/lib/mosquitto_dynamic_security.so
plugin_opt_config_file $DB
CONF

# mosquitto_ctrl nags on stderr about the (localhost-only) plaintext connection; strip it so
# the output only contains real results/errors.
ctrl() {
  mosquitto_ctrl -h 127.0.0.1 -p $PORT -u "$ADMIN_USER" -P "$ADMIN_PASS" dynsec "$@" 2>&1 </dev/null |
    grep -v -e "without encryption" -e "visible on the network"
}

# Idempotent call: "already ..." is fine, anything else is an error (counted, logged without
# the arguments because they can carry passwords).
c() {
  out="$(ctrl "$@")"
  case "$out" in
    "" | *"already"*) ;;
    *) echo "reconcile: ERROR $1 $2 -> $out"; ERRORS=$((ERRORS + 1)) ;;
  esac
}
# Removal call: "not found" is fine.
r() {
  out="$(ctrl "$@")"
  case "$out" in
    "" | *"not found"* | *"does not exist"*) ;;
    *) echo "reconcile: ERROR $1 $2 -> $out"; ERRORS=$((ERRORS + 1)) ;;
  esac
}

: > /tmp/roles

# (Re)create the state file if missing and start the throwaway broker; 0 once the admin can log in.
boot() {
  if [ ! -f "$DB" ]; then
    mosquitto_ctrl dynsec init "$DB" "$ADMIN_USER" "$ADMIN_PASS" || { echo "reconcile: dynsec init failed"; exit 1; }
  fi
  chown 1883:1883 "$DB" # broker drops to the mosquitto user and must rewrite this file
  mosquitto -c /tmp/bootstrap.conf >/tmp/bootstrap.log 2>&1 &
  BROKER=$!
  i=0
  until ctrl listClients | grep -qx "$ADMIN_USER"; do
    i=$((i + 1))
    [ $i -gt 40 ] && return 1
    sleep 0.5
  done
}

if ! boot; then
  # "not authorised" in the log = the broker is up but rejects the admin (rotated Vault password,
  # or a state file written by another Mosquitto version whose hashes this one cannot verify,
  # e.g. after a 2.1 -> 2.0 rollback). Startup slowness never logs that, so it is safe to act on.
  if $HAD_DB && grep -q "not authorised" /tmp/bootstrap.log; then
    echo "reconcile: WARNING admin cannot log in to the existing state file; keeping it as $DB.bak-* and rebuilding managed users/roles (runtime-created users/roles must be recreated from the backup)"
    kill "$BROKER" 2>/dev/null; wait "$BROKER" 2>/dev/null
    mv "$DB" "$DB.bak-$(date +%s)"
    HAD_DB=false # nothing left to fall back on: any further error must be fatal
    boot || { echo "reconcile: ERROR rebuild failed"; cat /tmp/bootstrap.log; ERRORS=$((ERRORS + 1)); finish 1; }
  else
    echo "reconcile: ERROR bootstrap broker unusable"
    cat /tmp/bootstrap.log
    ERRORS=$((ERRORS + 1))
    finish 1
  fi
fi

# --- default policy: nothing allowed unless a role says so -------------------------------
for t in publishClientSend publishClientReceive subscribe; do c setDefaultACLAccess $t deny; done
c setDefaultACLAccess unsubscribe allow

# --- clients.conf: roles, ACLs and users ------------------------------------------------
# One role r-<user> per client. The wanted ACL set per role comes from clients.conf (parsed by
# clients.awk, the same code the CI lint runs); anything else on the live role is removed, so
# dropping a line from clients.conf revokes it. Not touched: the $CONTROL denies below, roles that
# are not in clients.conf, and users/groups created at runtime with mosquitto_ctrl.
US="$(printf '\037')"
ACL_RW="publishClientSend publishClientReceive subscribePattern unsubscribePattern"
ACL_RO="publishClientReceive subscribePattern unsubscribePattern"

if ! awk -f "$CFG/clients.awk" "$CFG/clients.conf" > /tmp/clients.tsv 2> /tmp/clients.err; then
  echo "reconcile: ERROR clients.conf is invalid, keeping the last applied state:"
  cat /tmp/clients.err
  ERRORS=$((ERRORS + 1))
  finish 1
fi

# want <user> <acltype> <topic>: record one wanted grant
want() { printf '%s%s%s\n' "$2" "$US" "$3" >> "/tmp/want-$1"; }

while IFS="$US" read -r kind a b c; do
  case "$kind" in
    U)
      echo "$a" >> /tmp/roles
      : > "/tmp/want-$a"
      # learning mode = break-glass: every role may read/write '#' (readonly roles: read only)
      if [ "$LEARNING" = "true" ]; then
        if [ "$c" = readonly ]; then for t in $ACL_RO; do want "$a" "$t" '#'; done
        else for t in $ACL_RW; do want "$a" "$t" '#'; done; fi
      fi ;;
    A)
      case "$b" in
        rw) for t in $ACL_RW; do want "$a" "$t" "$c"; done ;;
        ro) for t in $ACL_RO; do want "$a" "$t" "$c"; done ;;
        send) want "$a" publishClientSend "$c" ;;
      esac ;;
  esac
done < /tmp/clients.tsv

# Revoke first, then grant, so a failed run never leaves more access than the file allows.
while IFS="$US" read -r kind user _; do
  [ "$kind" = U ] || continue
  role="r-$user"
  c createRole "$role"
  # live allow-grants as "<type>US<topic>"; the lines look like "<type> : allow : <topic> (priority: N)"
  ctrl getRole "$role" | awk -v US="$US" '
    { sub(/^ACLs:[ \t]*/, ""); sub(/^[ \t]+/, "") }
    / : allow : / {
      i = index($0, " : allow : "); t = substr($0, 1, i - 1)
      topic = substr($0, i + 11); sub(/ \(priority: [0-9]+\)$/, "", topic)
      gsub(/[ \t]+$/, "", t); print t US topic
    }' | sort > "/tmp/live-$user"
  sort -u "/tmp/want-$user" > "/tmp/want-$user.s"
  comm -23 "/tmp/live-$user" "/tmp/want-$user.s" > "/tmp/revoke-$user"
  while IFS="$US" read -r t topic; do
    echo "reconcile: revoke $user $t $topic"
    r removeRoleACL "$role" "$t" "$topic"
  done < "/tmp/revoke-$user"
  # '#' would otherwise cover the admin channel: only the admin role may touch $CONTROL.
  for t in publishClientSend publishClientReceive subscribePattern; do c addRoleACL "$role" $t '$CONTROL/#' deny 10; done
  comm -13 "/tmp/live-$user" "/tmp/want-$user.s" > "/tmp/grant-$user"
  while IFS="$US" read -r t topic; do
    c addRoleACL "$role" "$t" "$topic" allow
  done < "/tmp/grant-$user"
done < /tmp/clients.tsv

# Users: create or rotate the password, bind the role.
while IFS="$US" read -r kind user pass _; do
  [ "$kind" = U ] || continue
  out="$(ctrl createClient "$user" -p "$pass")"
  case "$out" in
    *"already"*) c setClientPassword "$user" "$pass" ;;
    "") ;;
    *) echo "reconcile: ERROR createClient $user -> $out"; ERRORS=$((ERRORS + 1)) ;;
  esac
  # 2.0.22 answers "Internal error" (not "already ...") when the binding exists, so look first.
  ctrl getClient "$user" | grep -q "r-$user" || c addClientRole "$user" "r-$user"
done < /tmp/clients.tsv

# Explicit removals only (a `retired-user` line), never by absence.
while IFS="$US" read -r kind user _; do
  [ "$kind" = X ] || continue
  echo "reconcile: retiring user $user"
  r deleteClient "$user"
  r deleteRole "r-$user"
done < /tmp/clients.tsv

finish 1
