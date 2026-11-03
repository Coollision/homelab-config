#!/bin/sh
# Init-container reconciler for the Mosquitto dynamic-security state.
#
# Declarative (this file + Vault, reapplied every pod start):
#   - the default ACL policy (deny all)
#   - the roles r-<user> below
#   - the "managed" users: credential1..N from Vault ("user:password"), password rotated, role attached
# Imperative (survives restarts, never touched here):
#   - any user/role/group created at runtime with mosquitto_ctrl
#
# Mechanics: a throwaway broker on 127.0.0.1:1884 loads the same state file, we drive it
# with mosquitto_ctrl, then stop it so the plugin flushes the file before the real broker starts.
set -u

DB=/mosquitto/data/dynamic-security.json
CFG=/mosquitto/config
PORT=1884

ADMIN="$(cat $CFG/admin-credential)"
ADMIN_USER="${ADMIN%%:*}"
ADMIN_PASS="${ADMIN#*:}"

# Break-glass: "true" gives every managed role read/write on '#' (set in kustomization.yaml).
LEARNING="$(cat $CFG/acl-learning 2>/dev/null || echo false)"

[ -f "$DB" ] || mosquitto_ctrl dynsec init "$DB" "$ADMIN_USER" "$ADMIN_PASS" || exit 1
chown 1883:1883 "$DB" # broker drops to the mosquitto user and must rewrite this file

cat > /tmp/bootstrap.conf <<EOF
listener $PORT 127.0.0.1
allow_anonymous false
plugin /usr/lib/mosquitto_dynamic_security.so
plugin_opt_config_file $DB
EOF
mosquitto -c /tmp/bootstrap.conf >/tmp/bootstrap.log 2>&1 &
BROKER=$!

ctrl() { mosquitto_ctrl -h 127.0.0.1 -p $PORT -u "$ADMIN_USER" -P "$ADMIN_PASS" dynsec "$@" 2>&1; }

# Tolerate "already exists" (idempotency); print anything else without echoing arguments
# (they can carry passwords).
c() {
  out="$(ctrl "$@")"
  case "$out" in
    "" | *"already exists"* | *"not found"*) ;;
    *) echo "reconcile: $1 $2 -> $out" ;;
  esac
}

i=0
until ctrl listClients >/dev/null 2>&1; do
  i=$((i + 1))
  [ $i -gt 40 ] && { echo "reconcile: bootstrap broker never came up"; cat /tmp/bootstrap.log; exit 1; }
  sleep 0.5
done

# --- default policy: nothing allowed unless a role says so -------------------------------
for t in publishClientToBroker publishBrokerToClient subscribe; do c setDefaultACLAccess $t deny; done
c setDefaultACLAccess unsubscribe allow

# --- roles -------------------------------------------------------------------------------
: > /tmp/roles
role() { # role <user>: recreate r-<user> from scratch so removed ACLs actually disappear
  CUR="r-$1"
  echo "$1" >> /tmp/roles
  c deleteRole "$CUR"
  c createRole "$CUR"
  [ "$LEARNING" = "true" ] && rw '#'
  return 0
}
rw() { for t in publishClientToBroker publishBrokerToClient subscribePattern unsubscribePattern; do c addRoleACL "$CUR" $t "$1" allow; done; }
ro() { for t in publishBrokerToClient subscribePattern unsubscribePattern; do c addRoleACL "$CUR" $t "$1" allow; done; }

role zigbee2mqtt
rw 'zigbee2mqtt/#'
rw 'homeassistant/#' # discovery configs + homeassistant/status birth message

role homeAssistant
rw 'homeassistant/#'
rw 'zigbee2mqtt/#'
rw 'shellies/#'
rw 'ems-esp/#'

# One shared account for all Gen1 Shellies. With per-device users (mosquitto_ctrl createClient)
# this can become 'shellies/%u/#' + 'shellies/announce' + 'shellies/command'.
role shelly
rw 'shellies/#'

role emsChip
rw 'ems-esp/#'
rw 'homeassistant/#'

role moonraker
rw 'K1C-F1CE/#'

role debug
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
    *"already exists"*) c setClientPassword "$user" "$pass" ;;
    "") ;;
    *) echo "reconcile: createClient $user -> $out" ;;
  esac
  if grep -qx "$user" /tmp/roles; then
    c addClientRole "$user" "r-$user"
  else
    echo "reconcile: WARNING user '$user' has no role in reconcile.sh -> denied everything"
  fi
done

kill "$BROKER"
wait "$BROKER" 2>/dev/null
chown 1883:1883 "$DB"
chmod 0600 "$DB"
echo "reconcile: done ($(grep -c . /tmp/roles) roles, learning=$LEARNING)"
