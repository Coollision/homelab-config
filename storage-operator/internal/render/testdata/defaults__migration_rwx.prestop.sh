echo 'Shutting down gracefully...'
sleep 5
while read -r _ mp fstype _; do
  case "$fstype" in nfs*) ;; *) continue ;; esac
  case "$mp" in /shares/*) ;; *) continue ;; esac
  echo "Unmounting $mp"
  timeout 5 umount -f "$mp" 2>/dev/null || umount -l "$mp" 2>/dev/null || echo "  already gone: $mp"
done < /proc/self/mounts
echo 'preStop complete'
exit 0