#!/bin/bash
set -e
echo 'Starting SMB server initialization...'
echo ''

# Create SMB user for authentication
useradd -M -s /usr/sbin/nologin alice 2>/dev/null || true
echo -e "$SMB_PASSWORD\n$SMB_PASSWORD" | smbpasswd -a -s alice
echo 'SMB user alice configured'
echo ''

echo 'NFS share (reconciler-managed): media'
mkdir -p /shares/media
echo 'NFS share (reconciler-managed): backup'
mkdir -p /shares/backup
echo 'NFS share (reconciler-managed): docs'
mkdir -p /shares/docs
echo 'Direct CSI mount ready: cfg'
mkdir -p /shares/cfg
echo 'Direct CSI mount ready: long'
mkdir -p /shares/long
echo ''

# Install and start the NFS mount reconciler
cat > /usr/local/bin/smb-mount-reconciler.sh <<'SMB_OPERATOR_RECONCILER_EOF'
#!/bin/bash
# Managed by smb-operator. Reconciles /shares/* NFS mounts against the mount table.
TABLE=/etc/smb-mounts/mounts.tsv
OPTS='vers=4.1,soft,timeo=30'
INTERVAL=7

log() { echo "[mount-reconciler] $*"; }

# Current NFS mount for a path, as 'server:/export', or empty if not mounted.
current_mount() {
  awk -v p="$1" '$2==p && $3 ~ /^nfs/ {print $1}' /proc/self/mounts | tail -n 1
}

# Detach a mount. Try a clean unmount first, time-boxed because it can block on a
# dead server, then fall back to a lazy unmount which always succeeds immediately.
#
# Success is judged by the resulting state, not by exit codes: a umount killed by
# `timeout` can still complete in the kernel, so the mount is gone while every command
# in the chain reported failure. Trusting the rc there produced spurious "could not
# unmount" warnings and made the caller skip a reconcile cycle.
detach() {
  local mp="$1"
  timeout 5 umount "$mp" 2>/dev/null \
    || timeout 5 umount -f "$mp" 2>/dev/null \
    || umount -l "$mp" 2>/dev/null
  [ -z "$(current_mount "$mp")" ]
}

# Is this mount path listed in the table at all?
in_table() {
  awk -F'\t' -v p="$1" '$1 !~ /^#/ && $2 == p { found = 1 } END { exit !found }' "$TABLE"
}

# Bring one mount point in line with its desired endpoint ('' means keep it unmounted).
reconcile_one() {
  local mpath="$1" want="$2" have
  have="$(current_mount "$mpath")"

  [ "$want" = "$have" ] && return 0

  if [ -n "$have" ]; then
    if detach "$mpath"; then
      log "unmounted $have from $mpath"
    else
      log "WARNING: could not unmount $mpath (had $have)"
      return 1
    fi
  fi

  if [ -z "$want" ]; then
    log "$mpath is detached (no endpoint); serving empty share"
    return 0
  fi

  mkdir -p "$mpath"
  if timeout 30 mount -t nfs -o "$OPTS" "$want" "$mpath"; then
    log "mounted $want -> $mpath"
  else
    log "mount failed: $want -> $mpath (will retry in ${INTERVAL}s)"
  fi
}

log "started (interval=${INTERVAL}s, opts=${OPTS})"

while true; do
  if [ -r "$TABLE" ]; then
    while IFS=$'\t' read -r name mpath endpoint; do
      case "$name" in ''|'#'*) continue ;; esac
      [ -n "$mpath" ] || continue
      reconcile_one "$mpath" "$endpoint"
    done < "$TABLE"

    # Drop any /shares/* NFS mount the table no longer knows about.
    while read -r src mp fstype _; do
      case "$fstype" in nfs*) ;; *) continue ;; esac
      case "$mp" in /shares/*) ;; *) continue ;; esac
      if ! in_table "$mp"; then
        detach "$mp" && log "unmounted orphaned $src from $mp"
      fi
    done < /proc/self/mounts
  else
    log "mount table $TABLE not readable yet"
  fi
  sleep "$INTERVAL"
done
SMB_OPERATOR_RECONCILER_EOF
chmod +x /usr/local/bin/smb-mount-reconciler.sh
/usr/local/bin/smb-mount-reconciler.sh &
echo 'Mount reconciler started'
echo ''

echo 'Waiting up to 12s for initial mounts...'
for _ in $(seq 1 12); do
  want=$(awk -F'\t' '$1 !~ /^#/ && NF>=3 && $3 != "" {c++} END {print c+0}' /etc/smb-mounts/mounts.tsv 2>/dev/null || echo 0)
  have=$(awk '$3 ~ /^nfs/ && $2 ~ /^\/shares\// {c++} END {print c+0}' /proc/self/mounts)
  [ "$have" -ge "$want" ] && break
  sleep 1
done
echo ''

echo 'Starting Samba daemon...'
exec /usr/sbin/smbd --foreground --no-process-group