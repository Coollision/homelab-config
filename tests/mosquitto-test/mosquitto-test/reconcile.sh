#!/bin/sh
# Init-container reconciler for the Mosquitto dynamic-security state.
#
# Declarative (this file + Vault, reapplied every pod start):
#   - the default ACL policy (deny all)
#   - the ACLs of the roles r-<user> below (added when missing, removed only via `retire`)
#   - the "managed" users: credential1..N from Vault ("user:password"), password rotated, role attached
# Imperative (survives restarts, never touched here):
#   - any user/group created at runtime with mosquitto_ctrl, including their role assignments
#     (roles are never deleted, so a runtime user bound to r-shelly keeps it)
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

if [ "$HAD_DB" = false ]; then
  mosquitto_ctrl dynsec init "$DB" "$ADMIN_USER" "$ADMIN_PASS" || { echo "reconcile: dynsec init failed"; exit 1; }
fi
chown 1883:1883 "$DB" # broker drops to the mosquitto user and must rewrite this file

cat > /tmp/bootstrap.conf <<CONF
listener $PORT 127.0.0.1
allow_anonymous false
plugin /usr/lib/mosquitto_dynamic_security.so
plugin_opt_config_file $DB
CONF
mosquitto -c /tmp/bootstrap.conf >/tmp/bootstrap.log 2>&1 &
BROKER=$!

# mosquitto_ctrl nags on stderr about the (localhost-only) plaintext connection; strip it so
# the output only contains real results/errors.
ctrl() {
  mosquitto_ctrl -h 127.0.0.1 -p $PORT -u "$ADMIN_USER" -P "$ADMIN_PASS" dynsec "$@" 2>&1 |
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
i=0
until ctrl listClients | grep -qx "$ADMIN_USER"; do
  i=$((i + 1))
  if [ $i -gt 40 ]; then
    echo "reconcile: ERROR bootstrap broker unusable (admin credential mismatch with the state file?)"
    cat /tmp/bootstrap.log
    ERRORS=$((ERRORS + 1))
    finish 1
  fi
  sleep 0.5
done

# --- default policy: nothing allowed unless a role says so -------------------------------
for t in publishClientSend publishClientReceive subscribe; do c setDefaultACLAccess $t deny; done
c setDefaultACLAccess unsubscribe allow

# --- roles -------------------------------------------------------------------------------
role() { # role <user> [ro]: "ro" roles only ever get read access, also in learning mode
  CUR="r-$1"
  echo "$1" >> /tmp/roles
  c createRole "$CUR"
  # '#' would otherwise cover the admin channel: only the admin role may touch $CONTROL.
  for t in publishClientSend publishClientReceive subscribePattern; do c addRoleACL "$CUR" $t '$CONTROL/#' deny 10; done
  if [ "$LEARNING" = "true" ]; then
    if [ "${2:-}" = ro ]; then ro '#'; rwsend '#'; else rw '#'; fi
  elif [ "${2:-}" = ro ]; then
    r removeRoleACL "$CUR" publishClientSend '#'
  else
    for t in publishClientSend publishClientReceive subscribePattern unsubscribePattern; do r removeRoleACL "$CUR" $t '#'; done
  fi
}
rwsend() { c addRoleACL "$CUR" publishClientSend "$1" allow; }
rw() { for t in publishClientSend publishClientReceive subscribePattern unsubscribePattern; do c addRoleACL "$CUR" $t "$1" allow; done; }
ro() { for t in publishClientReceive subscribePattern unsubscribePattern; do c addRoleACL "$CUR" $t "$1" allow; done; }
retire() { r removeRoleACL "r-$1" "$2" "$3"; } # retire <user> <acltype> <topic>: ACLs dropped from git must be listed here

role zigbee2mqtt
rw 'zigbee2mqtt/#'
rw 'homeassistant/#' # discovery configs + homeassistant/status birth message

role homeAssistant
rw 'homeassistant/#'
rw 'zigbee2mqtt/#'
rw 'shellies/#'
rw 'ems-esp/#'
rw 'printers/#' # automation publishing printer power readings (found by the prod topic capture)

# One shared account for all Gen1 Shellies. With per-device users (mosquitto_ctrl createClient
# + addClientRole <user> r-shelly) this can later become per-device topics.
role shelly
rw 'shellies/#'

role emsChip
rw 'ems-esp/#'
rw 'homeassistant/#'

role moonraker
rw 'K1C-F1CE/#'
ro 'printers/#' # provisional: assumed consumer of HA's printers/<name>/power, confirm in the usage capture

role debug ro
ro '#'
ro '$SYS/#'

# --- managed users -----------------------------------------------------------------------
for f in "$CFG"/credential*; do
  line="$(cat "$f")"
  user="${line%%:*}"
  pass="${line#*:}"
  [ -n "$user" ] || continue
  out="$(ctrl createClient "$user" -p "$pass")"
  case "$out" in
    *"already"*) c setClientPassword "$user" "$pass" ;;
    "") ;;
    *) echo "reconcile: ERROR createClient $user -> $out"; ERRORS=$((ERRORS + 1)) ;;
  esac
  if grep -qx "$user" /tmp/roles; then
    # 2.0.22 answers "Internal error" (not "already ...") when the binding exists, so look first.
    ctrl getClient "$user" | grep -q "r-$user" || c addClientRole "$user" "r-$user"
  else
    echo "reconcile: WARNING user '$user' has no role in reconcile.sh -> denied everything"
  fi
done

finish 1
